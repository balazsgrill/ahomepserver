package ahp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/balazsgrill/ahomepserver/internal/store"
	"github.com/google/uuid"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func (c *ClientConn) handleRequest(ctx context.Context, req *ahptypes.JsonRpcRequest) {
	switch req.Method {
	case "initialize":
		c.handleInitialize(ctx, req)
	case "ping":
		c.SendSuccess(req.ID, map[string]any{})
	case "reconnect":
		c.handleReconnect(ctx, req)
	case "authenticate":
		c.handleAuthenticate(ctx, req)
	case "subscribe":
		c.handleSubscribe(ctx, req)
	case "listSessions":
		c.handleListSessions(ctx, req)
	case "createSession":
		c.handleCreateSession(ctx, req)
	case "disposeSession":
		c.handleDisposeSession(ctx, req)
	case "createChat":
		c.handleCreateChat(ctx, req)
	case "fetchTurns":
		c.SendSuccess(req.ID, ahptypes.FetchTurnsResult{})
	case "runAutomation":
		c.handleRunAutomation(ctx, req)
	case "fetchAutomationRuns":
		c.SendSuccess(req.ID, map[string]any{})
	case "listAutomationTriggerDefinitions":
		c.handleListAutomationTriggerDefinitions(ctx, req)
	default:
		c.SendError(req.ID, ahptypes.ErrorCodeMethodNotFound, fmt.Sprintf("method not found: %s", req.Method), nil)
	}
}

func (c *ClientConn) handleNotification(ctx context.Context, notif *ahptypes.JsonRpcNotification) {
	switch notif.Method {
	case "unsubscribe":
		var params ahptypes.UnsubscribeParams
		if err := json.Unmarshal(notif.Params, &params); err == nil && params.Channel != "" {
			c.host.Unsubscribe(c, params.Channel)
		}
	case "dispatchAction":
		c.handleDispatchAction(ctx, notif)
	}
}

func (c *ClientConn) handleInitialize(ctx context.Context, req *ahptypes.JsonRpcRequest) {
	var params ahptypes.InitializeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInvalidParams, fmt.Sprintf("invalid initialize params: %v", err), nil)
		return
	}

	selectedVersion, err := ahptypes.NegotiateProtocolVersion(params.ProtocolVersions)
	if err != nil || selectedVersion == "" {
		c.SendError(req.ID, ahptypes.ErrorCodeUnsupportedProtocolVersion, "unsupported protocol version", ahptypes.UnsupportedProtocolVersionErrorData{
			SupportedVersions: ahptypes.SupportedProtocolVersions(),
		})
		return
	}

	c.mu.Lock()
	c.protocolVersion = selectedVersion
	c.clientID = params.ClientId
	c.clientInfo = params.ClientInfo
	c.mu.Unlock()

	var snapshots []ahptypes.Snapshot
	for _, chURI := range params.InitialSubscriptions {
		c.host.Subscribe(c, chURI)
		snap, err := c.host.GetSnapshot(ctx, chURI)
		if err == nil && snap != nil {
			snapshots = append(snapshots, *snap)
		}
	}

	c.SendSuccess(req.ID, ahptypes.InitializeResult{
		ProtocolVersion: selectedVersion,
		ServerSeq:       c.host.CurrentSeq(),
		ServerInfo: &ahptypes.Implementation{
			Name:    "ahomepserver",
			Version: ptr("1.1.0"),
		},
		Snapshots: snapshots,
		Automations: &ahptypes.AutomationCapabilities{
			Create: &ahptypes.AutomationCreateCapability{},
		},
		MultipleChatProviders: &ahptypes.MultipleChatProvidersCapability{},
	})
}

func (c *ClientConn) handleReconnect(ctx context.Context, req *ahptypes.JsonRpcRequest) {
	var params ahptypes.ReconnectParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInvalidParams, fmt.Sprintf("invalid reconnect params: %v", err), nil)
		return
	}

	c.mu.Lock()
	c.clientID = params.ClientId
	c.mu.Unlock()

	for _, chURI := range params.Subscriptions {
		c.host.Subscribe(c, chURI)
	}

	if c.host.CanReplay(params.LastSeenServerSeq) {
		actions := c.host.GetReplayActions(params.LastSeenServerSeq, params.Subscriptions)
		c.SendSuccess(req.ID, ahptypes.ReconnectResult{
			Value: &ahptypes.ReconnectReplayResult{
				Actions: actions,
			},
		})
		return
	}

	// Gap exceeds replay buffer: return fresh snapshots
	var snapshots []ahptypes.Snapshot
	for _, chURI := range params.Subscriptions {
		snap, err := c.host.GetSnapshot(ctx, chURI)
		if err == nil && snap != nil {
			snapshots = append(snapshots, *snap)
		}
	}
	c.SendSuccess(req.ID, ahptypes.ReconnectResult{
		Value: &ahptypes.ReconnectSnapshotResult{
			Snapshots: snapshots,
		},
	})
}

func (c *ClientConn) handleAuthenticate(_ context.Context, req *ahptypes.JsonRpcRequest) {
	var params ahptypes.AuthenticateParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInvalidParams, fmt.Sprintf("invalid authenticate params: %v", err), nil)
		return
	}

	if c.host.cfg.APIKey == "" || params.Token == c.host.cfg.APIKey {
		c.mu.Lock()
		c.authenticated = true
		c.mu.Unlock()
		c.SendSuccess(req.ID, map[string]any{})
		return
	}

	c.SendError(req.ID, ahptypes.ErrorCodeAuthRequired, "invalid authentication token", nil)
}

func (c *ClientConn) handleSubscribe(ctx context.Context, req *ahptypes.JsonRpcRequest) {
	if !c.CheckAuth() {
		c.SendError(req.ID, ahptypes.ErrorCodeAuthRequired, "authentication required", nil)
		return
	}

	var params ahptypes.SubscribeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInvalidParams, fmt.Sprintf("invalid subscribe params: %v", err), nil)
		return
	}

	c.host.Subscribe(c, params.Channel)
	snap, err := c.host.GetSnapshot(ctx, params.Channel)
	if err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeNotFound, err.Error(), nil)
		return
	}

	c.SendSuccess(req.ID, ahptypes.SubscribeResult{
		Snapshot: snap,
	})
}

func (c *ClientConn) handleListSessions(ctx context.Context, req *ahptypes.JsonRpcRequest) {
	if !c.CheckAuth() {
		c.SendError(req.ID, ahptypes.ErrorCodeAuthRequired, "authentication required", nil)
		return
	}

	sessions, err := c.host.store.ListSessions(ctx, "", 100)
	if err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInternalError, err.Error(), nil)
		return
	}

	items := make([]ahptypes.SessionSummary, 0, len(sessions))
	for _, s := range sessions {
		resURI := fmt.Sprintf("ahp-session:/%s", s.ID)
		chatURI := fmt.Sprintf("ahp-chat:/%s", s.ID)
		summary := ahptypes.SessionSummary{
			Resource:    resURI,
			Provider:    s.PersonaID,
			Title:       s.Title,
			Status:      ahptypes.SessionStatusIdle,
			CreatedAt:   s.CreatedAt.UTC().Format(time.RFC3339),
			ModifiedAt:  s.UpdatedAt.UTC().Format(time.RFC3339),
			DefaultChat: ptr(chatURI),
			Chats: []ahptypes.SessionChatSummary{
				{
					Resource: chatURI,
					Title:    s.Title,
					Status:   ptr(ahptypes.SessionStatusIdle),
				},
			},
		}
		items = append(items, summary)
	}

	c.SendSuccess(req.ID, ahptypes.ListSessionsResult{
		Items: items,
	})
}

func (c *ClientConn) handleCreateSession(ctx context.Context, req *ahptypes.JsonRpcRequest) {
	if !c.CheckAuth() {
		c.SendError(req.ID, ahptypes.ErrorCodeAuthRequired, "authentication required", nil)
		return
	}

	var params ahptypes.CreateSessionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInvalidParams, fmt.Sprintf("invalid createSession params: %v", err), nil)
		return
	}

	sessionID := strings.TrimPrefix(params.Channel, "ahp-session:/")
	if sessionID == "" {
		sessionID = uuid.NewString()
		params.Channel = fmt.Sprintf("ahp-session:/%s", sessionID)
	}

	c.host.mu.Lock()
	if _, exists := c.host.sessions[params.Channel]; exists {
		c.host.mu.Unlock()
		c.SendError(req.ID, ahptypes.ErrorCodeSessionAlreadyExists, "session already exists", nil)
		return
	}

	provider := ""
	if params.Provider != nil && *params.Provider != "" {
		provider = *params.Provider
	} else if len(c.host.rootState.Agents) > 0 {
		provider = c.host.rootState.Agents[0].Provider
	}

	title := "New Session"
	if p, ok := c.host.personas.Get(provider); ok {
		title = fmt.Sprintf("%s Session", p.Name)
	}

	defaultChatURI := fmt.Sprintf("ahp-chat:/%s", sessionID)
	now := time.Now().UTC()

	// Persist session to store
	stSess := &store.Session{
		ID:        sessionID,
		PersonaID: provider,
		Title:     title,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := c.host.store.CreateSession(ctx, stSess); err != nil {
		c.host.mu.Unlock()
		c.SendError(req.ID, ahptypes.ErrorCodeInternalError, fmt.Sprintf("failed to create session: %v", err), nil)
		return
	}

	// Register in-memory session and default chat
	sessState := &ahptypes.SessionState{
		Provider:  provider,
		Title:     title,
		Status:    ahptypes.SessionStatusIdle,
		Lifecycle: ahptypes.SessionLifecycleReady,
		Chats: []ahptypes.ChatSummary{
			{
				Resource: defaultChatURI,
				Title:    title,
				Status:   ahptypes.SessionStatusIdle,
			},
		},
		DefaultChat: ptr(defaultChatURI),
	}
	chatState := &ahptypes.ChatState{
		Resource: defaultChatURI,
		Title:    title,
		Status:   ahptypes.SessionStatusIdle,
		Turns:    []ahptypes.Turn{},
	}

	c.host.sessions[params.Channel] = sessState
	c.host.chats[defaultChatURI] = chatState
	c.host.chatToSession[defaultChatURI] = params.Channel
	c.host.mu.Unlock()

	// Notify root subscribers of new session
	summary := ahptypes.SessionSummary{
		Resource:    params.Channel,
		Provider:    provider,
		Title:       title,
		Status:      ahptypes.SessionStatusIdle,
		CreatedAt:   now.Format(time.RFC3339),
		ModifiedAt:  now.Format(time.RFC3339),
		DefaultChat: ptr(defaultChatURI),
		Chats: []ahptypes.SessionChatSummary{
			{
				Resource: defaultChatURI,
				Title:    title,
				Status:   ptr(ahptypes.SessionStatusIdle),
			},
		},
	}
	c.host.BroadcastNotification("ahp-root://", "root/sessionAdded", ahptypes.SessionAddedParams{
		Channel: "ahp-root://",
		Summary: summary,
	})

	c.host.BroadcastAction(params.Channel, ahptypes.StateAction{
		Value: &ahptypes.SessionReadyAction{
			Type: ahptypes.ActionTypeSessionReady,
		},
	}, nil)

	c.SendSuccess(req.ID, map[string]any{})
}

func (c *ClientConn) handleDisposeSession(ctx context.Context, req *ahptypes.JsonRpcRequest) {
	if !c.CheckAuth() {
		c.SendError(req.ID, ahptypes.ErrorCodeAuthRequired, "authentication required", nil)
		return
	}

	var params ahptypes.DisposeSessionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInvalidParams, fmt.Sprintf("invalid disposeSession params: %v", err), nil)
		return
	}

	sessionID := strings.TrimPrefix(params.Channel, "ahp-session:/")
	_ = c.host.store.DeleteSession(ctx, sessionID)

	c.host.mu.Lock()
	if sess, ok := c.host.sessions[params.Channel]; ok {
		for _, ch := range sess.Chats {
			delete(c.host.chats, ch.Resource)
			delete(c.host.chatToSession, ch.Resource)
		}
	}
	delete(c.host.sessions, params.Channel)
	c.host.mu.Unlock()

	c.host.BroadcastNotification("ahp-root://", "root/sessionRemoved", ahptypes.SessionRemovedParams{
		Channel: "ahp-root://",
		Session: params.Channel,
	})

	c.SendSuccess(req.ID, map[string]any{})
}

func (c *ClientConn) handleCreateChat(_ context.Context, req *ahptypes.JsonRpcRequest) {
	if !c.CheckAuth() {
		c.SendError(req.ID, ahptypes.ErrorCodeAuthRequired, "authentication required", nil)
		return
	}

	var params ahptypes.CreateChatParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInvalidParams, fmt.Sprintf("invalid createChat params: %v", err), nil)
		return
	}

	title := "New Chat"
	if params.Title != nil && *params.Title != "" {
		title = *params.Title
	}

	c.host.mu.Lock()
	c.host.chats[params.Chat] = &ahptypes.ChatState{
		Resource: params.Chat,
		Title:    title,
		Status:   ahptypes.SessionStatusIdle,
		Turns:    []ahptypes.Turn{},
	}
	c.host.chatToSession[params.Chat] = params.Channel
	c.host.mu.Unlock()

	c.host.BroadcastAction(params.Channel, ahptypes.StateAction{
		Value: &ahptypes.SessionChatAddedAction{
			Type: ahptypes.ActionTypeSessionChatAdded,
			Summary: ahptypes.ChatSummary{
				Resource: params.Chat,
				Title:    title,
				Status:   ahptypes.SessionStatusIdle,
			},
		},
	}, nil)

	c.SendSuccess(req.ID, map[string]any{})
}

func (c *ClientConn) handleRunAutomation(ctx context.Context, req *ahptypes.JsonRpcRequest) {
	if !c.CheckAuth() {
		c.SendError(req.ID, ahptypes.ErrorCodeAuthRequired, "authentication required", nil)
		return
	}

	var params ahptypes.RunAutomationParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeInvalidParams, fmt.Sprintf("invalid runAutomation params: %v", err), nil)
		return
	}

	runURI, err := c.host.TriggerAutomation(ctx, params.Automation, nil, "ahp-client")
	if err != nil {
		c.SendError(req.ID, ahptypes.ErrorCodeNotFound, err.Error(), nil)
		return
	}

	c.SendSuccess(req.ID, ahptypes.RunAutomationResult{
		Resource: runURI,
	})
}

func (c *ClientConn) handleListAutomationTriggerDefinitions(_ context.Context, req *ahptypes.JsonRpcRequest) {
	c.SendSuccess(req.ID, ahptypes.ListAutomationTriggerDefinitionsResult{
		Items: []ahptypes.AutomationTriggerDefinition{
			{
				Type:        "webhook",
				Title:       "HTTP Webhook Trigger",
				Description: ptr("Triggers when an external HTTP POST webhook is received"),
				Events: []ahptypes.AutomationTriggerEventDefinition{
					{
						Id:          "post",
						Title:       "Webhook POST",
						Description: ptr("POST /webhook/<automation_id>"),
					},
				},
			},
		},
	})
}

func (c *ClientConn) handleDispatchAction(ctx context.Context, notif *ahptypes.JsonRpcNotification) {
	if !c.CheckAuth() {
		return
	}

	var params ahptypes.DispatchActionParams
	if err := json.Unmarshal(notif.Params, &params); err != nil {
		return
	}

	origin := &ahptypes.ActionOrigin{
		ClientId:  c.clientID,
		ClientSeq: params.ClientSeq,
	}

	switch a := params.Action.Value.(type) {
	case *ahptypes.ChatTurnStartedAction:
		// Echo turn started action to subscribers with sequence number
		c.host.BroadcastAction(params.Channel, params.Action, origin)
		// Begin agent turn execution asynchronously
		go c.host.executeChatTurn(context.Background(), params.Channel, a)

	case *ahptypes.AutomationCreateRequestedAction:
		c.host.mu.Lock()
		entry := ahptypes.AutomationEntry{
			Resource:   a.Resource,
			Definition: a.Definition,
			Operations: []ahptypes.AutomationOperation{"run", "remove"},
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			ModifiedAt: time.Now().UTC().Format(time.RFC3339),
		}
		c.host.autoState.Entries = append(c.host.autoState.Entries, entry)
		c.host.mu.Unlock()

		c.host.BroadcastAction("ahp-automations://", ahptypes.StateAction{
			Value: &ahptypes.AutomationSetAction{
				Type:       ahptypes.ActionTypeAutomationSet,
				Automation: entry,
			},
		}, origin)

	case *ahptypes.AutomationRemovedAction:
		c.host.mu.Lock()
		for i, ent := range c.host.autoState.Entries {
			if ent.Resource == a.Resource {
				c.host.autoState.Entries = append(c.host.autoState.Entries[:i], c.host.autoState.Entries[i+1:]...)
				break
			}
		}
		c.host.mu.Unlock()

		c.host.BroadcastAction("ahp-automations://", params.Action, origin)

	default:
		c.host.BroadcastAction(params.Channel, params.Action, origin)
	}
}
