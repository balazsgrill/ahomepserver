package ahp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/balazsgrill/ahomepserver/internal/agent"
	"github.com/balazsgrill/ahomepserver/internal/config"
	"github.com/balazsgrill/ahomepserver/internal/persona"
	"github.com/balazsgrill/ahomepserver/internal/store"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const (
	maxReplayBufferSize = 5000
)

// Host represents the authoritative Agent Host Protocol server.
// It manages monotonic sequence numbering, pure state reducers,
// subscriptions, client connection lifecycles, and action broadcasting.
type Host struct {
	cfg      *config.Config
	tokens   *config.TokenResolver
	personas *persona.Manager
	store    store.Store
	runner   *agent.Runner

	mu              sync.RWMutex
	serverSeq       int64
	replayBuffer    []ahptypes.ActionEnvelope
	replayStartSeq  int64

	// Authoritative channel states
	rootState      *ahptypes.RootState
	autoState      *ahptypes.AutomationState
	sessions       map[string]*ahptypes.SessionState       // key: ahp-session:/<uuid>
	chats          map[string]*ahptypes.ChatState          // key: ahp-chat:/<cid>
	automationRuns map[string]*ahptypes.AutomationRunState // key: ahp-automation-run:/<id>

	// Chat -> owning session URI mapping
	chatToSession map[string]string

	// Active subscriptions: channelURI -> set of *ClientConn
	subscriptions map[string]map[*ClientConn]bool
	clients       map[*ClientConn]bool
}

// NewHost creates and initializes a new AHP Host.
func NewHost(
	cfg *config.Config,
	tokens *config.TokenResolver,
	personas *persona.Manager,
	st store.Store,
	runner *agent.Runner,
) *Host {
	h := &Host{
		cfg:            cfg,
		tokens:         tokens,
		personas:       personas,
		store:          st,
		runner:         runner,
		replayBuffer:   make([]ahptypes.ActionEnvelope, 0, 512),
		replayStartSeq: 1,
		rootState: &ahptypes.RootState{
			Agents:         []ahptypes.AgentInfo{},
			ActiveSessions: ptr(int64(0)),
		},
		autoState: &ahptypes.AutomationState{
			Entries: []ahptypes.AutomationEntry{},
		},
		sessions:       make(map[string]*ahptypes.SessionState),
		chats:          make(map[string]*ahptypes.ChatState),
		automationRuns: make(map[string]*ahptypes.AutomationRunState),
		chatToSession:  make(map[string]string),
		subscriptions:  make(map[string]map[*ClientConn]bool),
		clients:        make(map[*ClientConn]bool),
	}

	h.SyncPersonasToRoot()
	return h
}

// CurrentSeq returns the current monotonic serverSeq.
func (h *Host) CurrentSeq() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.serverSeq
}

// RegisterClient adds a new connected client.
func (h *Host) RegisterClient(c *ClientConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = true
}

// UnregisterClient cleans up a disconnected client and all its subscriptions.
func (h *Host) UnregisterClient(c *ClientConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	for chURI, subs := range h.subscriptions {
		delete(subs, c)
		if len(subs) == 0 {
			delete(h.subscriptions, chURI)
		}
	}
}

// Subscribe registers a client to receive actions and notifications for a channel.
func (h *Host) Subscribe(c *ClientConn, channelURI string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subscriptions[channelURI]
	if subs == nil {
		subs = make(map[*ClientConn]bool)
		h.subscriptions[channelURI] = subs
	}
	subs[c] = true
	c.mu.Lock()
	c.subscriptions[channelURI] = true
	c.mu.Unlock()
}

// Unsubscribe removes a client subscription for a channel.
func (h *Host) Unsubscribe(c *ClientConn, channelURI string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subs := h.subscriptions[channelURI]; subs != nil {
		delete(subs, c)
		if len(subs) == 0 {
			delete(h.subscriptions, channelURI)
		}
	}
	c.mu.Lock()
	delete(c.subscriptions, channelURI)
	c.mu.Unlock()
}

// SyncPersonasToRoot updates the root state and automation catalogue from loaded personas.
func (h *Host) SyncPersonasToRoot() {
	allPersonas := h.personas.List()
	agents := make([]ahptypes.AgentInfo, 0, len(allPersonas))
	automations := make([]ahptypes.AutomationEntry, 0)

	for _, p := range allPersonas {
		agents = append(agents, PersonaToAgentInfo(p))
		for _, wf := range p.Workflows {
			automations = append(automations, WorkflowToAutomationEntry(p.ID, p.Name, wf))
		}
	}

	h.mu.Lock()
	isInitial := len(h.rootState.Agents) == 0
	h.rootState.Agents = agents
	h.autoState.Entries = automations
	h.mu.Unlock()

	if !isInitial {
		// Broadcast root/agentsChanged action to root subscribers
		h.BroadcastAction("ahp-root://", ahptypes.StateAction{
			Value: &ahptypes.RootAgentsChangedAction{
				Type:   ahptypes.ActionTypeRootAgentsChanged,
				Agents: agents,
			},
		}, nil)
	}
}

// BroadcastAction applies a state action to the authoritative channel state using the
// standard pure reducer, logs it to the replay buffer with a monotonic serverSeq, and sends
// an action notification to every client subscribed to the target channel.
func (h *Host) BroadcastAction(channelURI string, action ahptypes.StateAction, origin *ahptypes.ActionOrigin) int64 {
	h.mu.Lock()
	h.serverSeq++
	seq := h.serverSeq

	// 1. Apply pure reducer according to channel scheme
	switch {
	case channelURI == "ahp-root://":
		ahp.ApplyActionToRoot(h.rootState, action)
	case channelURI == "ahp-automations://":
		ahp.ApplyActionToAutomation(h.autoState, action)
	case strings.HasPrefix(channelURI, "ahp-session:/"):
		if s, ok := h.sessions[channelURI]; ok {
			ahp.ApplyActionToSession(s, action)
		}
	case strings.HasPrefix(channelURI, "ahp-chat:/"):
		if c, ok := h.chats[channelURI]; ok {
			ahp.ApplyActionToChat(c, action)
		}
	case strings.HasPrefix(channelURI, "ahp-automation-run:/"):
		if r, ok := h.automationRuns[channelURI]; ok {
			ahp.ApplyActionToAutomationRun(r, action)
		}
	}

	// 2. Wrap in ActionEnvelope
	envelope := ahptypes.ActionEnvelope{
		Channel:   channelURI,
		Action:    action,
		ServerSeq: seq,
		Origin:    origin,
	}

	// 3. Append to replay ring buffer
	h.replayBuffer = append(h.replayBuffer, envelope)
	if len(h.replayBuffer) > maxReplayBufferSize {
		h.replayBuffer = h.replayBuffer[1:]
		h.replayStartSeq = h.replayBuffer[0].ServerSeq
	}

	// 4. Collect subscribed clients
	var targetClients []*ClientConn
	if subs := h.subscriptions[channelURI]; subs != nil {
		targetClients = make([]*ClientConn, 0, len(subs))
		for c := range subs {
			targetClients = append(targetClients, c)
		}
	}
	h.mu.Unlock()

	// 5. Send notification to all subscribers outside mutex lock
	paramsRaw, err := json.Marshal(envelope)
	if err == nil {
		notif := ahptypes.JsonRpcNotification{
			JsonRpc: ahptypes.JsonRpcV2,
			Method:  "action",
			Params:  paramsRaw,
		}
		for _, c := range targetClients {
			c.SendNotification(notif)
		}
	}

	return seq
}

// BroadcastNotification sends an out-of-band protocol notification (e.g. root/sessionAdded)
// to all subscribers of the target channel.
func (h *Host) BroadcastNotification(channelURI string, method string, params any) {
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return
	}

	h.mu.RLock()
	var targetClients []*ClientConn
	if subs := h.subscriptions[channelURI]; subs != nil {
		targetClients = make([]*ClientConn, 0, len(subs))
		for c := range subs {
			targetClients = append(targetClients, c)
		}
	}
	h.mu.RUnlock()

	notif := ahptypes.JsonRpcNotification{
		JsonRpc: ahptypes.JsonRpcV2,
		Method:  method,
		Params:  paramsRaw,
	}
	for _, c := range targetClients {
		c.SendNotification(notif)
	}
}

// GetSnapshot retrieves the current snapshot for any channel URI.
func (h *Host) GetSnapshot(ctx context.Context, channelURI string) (*ahptypes.Snapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	fromSeq := h.serverSeq

	switch {
	case channelURI == "ahp-root://":
		// Deep copy root state
		b, _ := json.Marshal(h.rootState)
		var root ahptypes.RootState
		_ = json.Unmarshal(b, &root)
		return &ahptypes.Snapshot{
			Resource: channelURI,
			State:    ahptypes.SnapshotState{Root: &root},
			FromSeq:  fromSeq,
		}, nil

	case channelURI == "ahp-automations://":
		b, _ := json.Marshal(h.autoState)
		var auto ahptypes.AutomationState
		_ = json.Unmarshal(b, &auto)
		return &ahptypes.Snapshot{
			Resource: channelURI,
			State:    ahptypes.SnapshotState{Automations: &auto},
			FromSeq:  fromSeq,
		}, nil

	case strings.HasPrefix(channelURI, "ahp-session:/"):
		s, ok := h.sessions[channelURI]
		if !ok {
			// Try to hydrate from persistent store
			sessionID := strings.TrimPrefix(channelURI, "ahp-session:/")
			storedSess, err := h.store.GetSession(ctx, sessionID)
			if err != nil || storedSess == nil {
				return nil, fmt.Errorf("session not found: %s", channelURI)
			}
			s = h.hydrateSession(storedSess)
		}
		b, _ := json.Marshal(s)
		var sess ahptypes.SessionState
		_ = json.Unmarshal(b, &sess)
		return &ahptypes.Snapshot{
			Resource: channelURI,
			State:    ahptypes.SnapshotState{Session: &sess},
			FromSeq:  fromSeq,
		}, nil

	case strings.HasPrefix(channelURI, "ahp-chat:/"):
		c, ok := h.chats[channelURI]
		if !ok {
			// Find session for this chat and hydrate
			chatID := strings.TrimPrefix(channelURI, "ahp-chat:/")
			sessionURI, found := h.chatToSession[channelURI]
			if !found {
				// Search sessions in store
				sessionURI = fmt.Sprintf("ahp-session:/%s", chatID)
			}
			sessionID := strings.TrimPrefix(sessionURI, "ahp-session:/")
			storedSess, err := h.store.GetSession(ctx, sessionID)
			if err == nil && storedSess != nil {
				_ = h.hydrateSession(storedSess)
				c = h.chats[channelURI]
			}
		}
		if c == nil {
			return nil, fmt.Errorf("chat not found: %s", channelURI)
		}
		b, _ := json.Marshal(c)
		var chat ahptypes.ChatState
		_ = json.Unmarshal(b, &chat)
		return &ahptypes.Snapshot{
			Resource: channelURI,
			State:    ahptypes.SnapshotState{Chat: &chat},
			FromSeq:  fromSeq,
		}, nil

	case strings.HasPrefix(channelURI, "ahp-automation-run:/"):
		r, ok := h.automationRuns[channelURI]
		if !ok {
			return nil, fmt.Errorf("automation run not found: %s", channelURI)
		}
		b, _ := json.Marshal(r)
		var run ahptypes.AutomationRunState
		_ = json.Unmarshal(b, &run)
		return &ahptypes.Snapshot{
			Resource: channelURI,
			State:    ahptypes.SnapshotState{AutomationRun: &run},
			FromSeq:  fromSeq,
		}, nil
	}

	return nil, fmt.Errorf("unsupported channel URI scheme: %s", channelURI)
}

// hydrateSession creates in-memory SessionState and ChatState from persistent store.Session.
func (h *Host) hydrateSession(s *store.Session) *ahptypes.SessionState {
	sessionURI := fmt.Sprintf("ahp-session:/%s", s.ID)
	defaultChatURI := fmt.Sprintf("ahp-chat:/%s", s.ID)

	turns := MessagesToTurns(s.Messages)

	chatState := &ahptypes.ChatState{
		Resource: defaultChatURI,
		Title:    s.Title,
		Status:   ahptypes.SessionStatusIdle,
		Turns:    turns,
	}
	h.chats[defaultChatURI] = chatState
	h.chatToSession[defaultChatURI] = sessionURI

	sessState := &ahptypes.SessionState{
		Provider:  s.PersonaID,
		Title:     s.Title,
		Status:    ahptypes.SessionStatusIdle,
		Lifecycle: ahptypes.SessionLifecycleReady,
		Chats: []ahptypes.ChatSummary{
			{
				Resource: defaultChatURI,
				Title:    s.Title,
				Status:   ahptypes.SessionStatusIdle,
			},
		},
		DefaultChat: &defaultChatURI,
	}
	h.sessions[sessionURI] = sessState
	return sessState
}

// CanReplay checks if missed actions can be replayed from the buffer.
func (h *Host) CanReplay(lastSeenSeq int64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if lastSeenSeq == h.serverSeq {
		return true
	}
	if lastSeenSeq < h.replayStartSeq-1 {
		return false
	}
	return true
}

// GetReplayActions retrieves missed action envelopes for the given subscriptions.
func (h *Host) GetReplayActions(lastSeenSeq int64, subscriptions []string) []ahptypes.ActionEnvelope {
	h.mu.RLock()
	defer h.mu.RUnlock()

	subSet := make(map[string]bool, len(subscriptions))
	for _, s := range subscriptions {
		subSet[s] = true
	}

	var missed []ahptypes.ActionEnvelope
	for _, env := range h.replayBuffer {
		if env.ServerSeq > lastSeenSeq && subSet[env.Channel] {
			missed = append(missed, env)
		}
	}
	return missed
}

// AutoState returns the current automation state pointer.
func (h *Host) AutoState() *ahptypes.AutomationState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.autoState
}
