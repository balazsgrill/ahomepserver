package ahp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/balazsgrill/ahomepserver/internal/agent"
	"github.com/balazsgrill/ahomepserver/internal/ahp"
	"github.com/balazsgrill/ahomepserver/internal/config"
	"github.com/balazsgrill/ahomepserver/internal/persona"
	"github.com/balazsgrill/ahomepserver/internal/store"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

func ptr[T any](v T) *T {
	return &v
}

func setupTestAHP(t *testing.T) (*httptest.Server, *ahp.Host) {
	t.Helper()

	// 1. Mock LLM Server
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Hello from AHP test agent!",
					},
				},
			},
		})
	}))
	t.Cleanup(llmSrv.Close)

	// 2. Temp personas directory
	tempDir := t.TempDir()
	pDir := filepath.Join(tempDir, "test-bot")
	_ = os.MkdirAll(filepath.Join(pDir, "skills"), 0o755)

	cfgContent := fmt.Sprintf(`
id: test-bot
name: Test Bot
description: AHP Test Assistant
llm:
  endpoint: %s
  model: test-model
  token_key: TEST_TOKEN
workflows:
  - name: test-flow
    description: Test workflow description
    prompt_template: Run test flow
`, llmSrv.URL)
	_ = os.WriteFile(filepath.Join(pDir, "config.yaml"), []byte(cfgContent), 0o644)
	_ = os.WriteFile(filepath.Join(pDir, "prompt.md"), []byte("You are a test assistant."), 0o644)
	_ = os.WriteFile(filepath.Join(pDir, "skills", "diagnostics.md"), []byte("# Diagnostics skill"), 0o644)

	cfg := &config.Config{
		DatabaseURL:  "memory://",
		PersonasDir:  tempDir,
		ScanInterval: time.Minute,
		MaxAgentTurn: 5,
	}
	tokens := config.NewTokenResolver("")
	pMgr := persona.NewManager(tempDir, 0, nil)
	if _, err := pMgr.Scan(); err != nil {
		t.Fatalf("Scan failed: %v", err)
	}

	st := store.NewMemoryStore()
	runner := agent.NewRunner(tokens, 5)
	host := ahp.NewHost(cfg, tokens, pMgr, st, runner)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ahp", ahp.UpgradeHandler(host))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return ts, host
}

// wsRPC is a test helper for sending and receiving JSON-RPC 2.0 messages over WebSocket.
type wsRPC struct {
	conn   *websocket.Conn
	nextID uint64
	inbox  chan *ahptypes.JsonRpcMessage
}

func newWSRPC(ctx context.Context, wsURL string) (*wsRPC, error) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	return &wsRPC{
		conn:   conn,
		nextID: 1,
		inbox:  make(chan *ahptypes.JsonRpcMessage, 200),
	}, nil
}

func (c *wsRPC) Call(ctx context.Context, method string, params any) (*ahptypes.JsonRpcMessage, error) {
	reqID := c.nextID
	c.nextID++

	var pBytes json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		pBytes = b
	}

	req := ahptypes.JsonRpcRequest{
		JsonRpc: ahptypes.JsonRpcV2,
		ID:      reqID,
		Method:  method,
		Params:  pBytes,
	}
	data, _ := json.Marshal(req)
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, err
	}

	for {
		_, respBytes, err := c.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		var msg ahptypes.JsonRpcMessage
		if err := json.Unmarshal(respBytes, &msg); err != nil {
			return nil, err
		}
		if (msg.SuccessResponse != nil && msg.SuccessResponse.ID == reqID) ||
			(msg.ErrorResponse != nil && msg.ErrorResponse.ID == reqID) {
			return &msg, nil
		}
		c.inbox <- &msg
	}
}

func (c *wsRPC) Notify(ctx context.Context, method string, params any) error {
	var pBytes json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		pBytes = b
	}
	notif := ahptypes.JsonRpcNotification{
		JsonRpc: ahptypes.JsonRpcV2,
		Method:  method,
		Params:  pBytes,
	}
	data, _ := json.Marshal(notif)
	return c.conn.Write(ctx, websocket.MessageText, data)
}

func (c *wsRPC) ReadMessage(ctx context.Context) (*ahptypes.JsonRpcMessage, error) {
	select {
	case msg := <-c.inbox:
		return msg, nil
	default:
	}

	for {
		select {
		case msg := <-c.inbox:
			return msg, nil
		default:
			_, respBytes, err := c.conn.Read(ctx)
			if err != nil {
				return nil, err
			}
			var msg ahptypes.JsonRpcMessage
			if err := json.Unmarshal(respBytes, &msg); err != nil {
				return nil, err
			}
			return &msg, nil
		}
	}
}

func (c *wsRPC) Close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "done")
}

func TestAHPProtocolHandshakeAndNegotiation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ts, _ := setupTestAHP(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ahp"

	// 1. Incompatible version must be rejected with -32005 (UnsupportedProtocolVersion)
	rpc1, err := newWSRPC(ctx, wsURL)
	if err != nil {
		t.Fatalf("failed to connect ws: %v", err)
	}
	defer rpc1.Close()

	resp1, err := rpc1.Call(ctx, "initialize", ahptypes.InitializeParams{
		Channel:          "ahp-root://",
		ProtocolVersions: []string{"99.0.0"},
		ClientId:         "test-client-bad-version",
	})
	if err != nil {
		t.Fatalf("rpc call failed: %v", err)
	}
	if resp1.ErrorResponse == nil || resp1.ErrorResponse.Error.Code != ahptypes.ErrorCodeUnsupportedProtocolVersion {
		t.Fatalf("expected ErrorCodeUnsupportedProtocolVersion (-32005), got: %+v", resp1)
	}

	// 2. Compatible version negotiation with initial subscriptions
	rpc2, err := newWSRPC(ctx, wsURL)
	if err != nil {
		t.Fatalf("failed to connect ws: %v", err)
	}
	defer rpc2.Close()

	resp2, err := rpc2.Call(ctx, "initialize", ahptypes.InitializeParams{
		Channel:              "ahp-root://",
		ProtocolVersions:     []string{"1.1.0", "1.0.0"},
		ClientId:             "test-client-1",
		InitialSubscriptions: []string{"ahp-root://", "ahp-automations://"},
	})
	if err != nil {
		t.Fatalf("rpc call failed: %v", err)
	}
	if resp2.SuccessResponse == nil {
		t.Fatalf("expected success initialize response, got: %+v", resp2)
	}

	var initRes ahptypes.InitializeResult
	if err := json.Unmarshal(resp2.SuccessResponse.Result, &initRes); err != nil {
		t.Fatalf("failed to unmarshal InitializeResult: %v", err)
	}
	if initRes.ProtocolVersion != "1.1.0" {
		t.Errorf("expected negotiated version 1.1.0, got %s", initRes.ProtocolVersion)
	}
	if len(initRes.Snapshots) != 2 {
		t.Fatalf("expected 2 snapshots for initial subscriptions, got %d", len(initRes.Snapshots))
	}

	// Verify root snapshot contains agents
	rootSnap := initRes.Snapshots[0]
	if rootSnap.Resource != "ahp-root://" || rootSnap.State.Root == nil {
		t.Fatalf("expected RootState snapshot, got: %+v", rootSnap)
	}
	if len(rootSnap.State.Root.Agents) != 1 || rootSnap.State.Root.Agents[0].Provider != "test-bot" {
		t.Errorf("unexpected agents in root snapshot: %+v", rootSnap.State.Root.Agents)
	}

	// Verify automations snapshot contains preloaded workflow templates
	autoSnap := initRes.Snapshots[1]
	if autoSnap.Resource != "ahp-automations://" || autoSnap.State.Automations == nil {
		t.Fatalf("expected AutomationState snapshot, got: %+v", autoSnap)
	}
	if len(autoSnap.State.Automations.Entries) != 1 {
		t.Errorf("expected 1 automation entry from workflow template, got: %+v", autoSnap.State.Automations.Entries)
	}

	// 3. Protocol ping
	pingResp, err := rpc2.Call(ctx, "ping", map[string]any{"channel": "ahp-root://"})
	if err != nil || pingResp.SuccessResponse == nil {
		t.Fatalf("ping failed: %+v, err: %v", pingResp, err)
	}
}

func TestAHPSessionAndChatTurnExecution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ts, _ := setupTestAHP(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ahp"

	rpc, err := newWSRPC(ctx, wsURL)
	if err != nil {
		t.Fatalf("failed to connect ws: %v", err)
	}
	defer rpc.Close()

	// Initialize
	_, err = rpc.Call(ctx, "initialize", ahptypes.InitializeParams{
		Channel:              "ahp-root://",
		ProtocolVersions:     []string{"1.1.0"},
		ClientId:             "tester",
		InitialSubscriptions: []string{"ahp-root://"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Create Session
	sessionID := uuid.NewString()
	sessionURI := fmt.Sprintf("ahp-session:/%s", sessionID)
	createResp, err := rpc.Call(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel:  sessionURI,
		Provider: ptr("test-bot"),
	})
	if err != nil || createResp.SuccessResponse == nil {
		t.Fatalf("createSession failed: %+v, err: %v", createResp, err)
	}

	// Read broadcast notification: root/sessionAdded
	msg, err := rpc.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("failed to read root/sessionAdded: %v", err)
	}
	if msg.Notification == nil || msg.Notification.Method != "root/sessionAdded" {
		t.Fatalf("expected root/sessionAdded notification, got: %+v", msg)
	}

	// 2. Subscribe to session channel
	subResp, err := rpc.Call(ctx, "subscribe", ahptypes.SubscribeParams{
		Channel: sessionURI,
	})
	if err != nil || subResp.SuccessResponse == nil {
		t.Fatalf("subscribe to session failed: %+v, err: %v", subResp, err)
	}
	var subRes ahptypes.SubscribeResult
	_ = json.Unmarshal(subResp.SuccessResponse.Result, &subRes)
	if subRes.Snapshot == nil || subRes.Snapshot.State.Session == nil {
		t.Fatalf("expected SessionState snapshot, got: %+v", subRes)
	}
	sessState := subRes.Snapshot.State.Session
	if sessState.Lifecycle != ahptypes.SessionLifecycleReady {
		t.Errorf("expected session lifecycle ready, got %s", sessState.Lifecycle)
	}
	if sessState.DefaultChat == nil {
		t.Fatalf("expected default chat on session")
	}
	chatURI := *sessState.DefaultChat

	// 3. Subscribe to chat channel
	chatSubResp, err := rpc.Call(ctx, "subscribe", ahptypes.SubscribeParams{
		Channel: chatURI,
	})
	if err != nil || chatSubResp.SuccessResponse == nil {
		t.Fatalf("subscribe to chat failed: %+v, err: %v", chatSubResp, err)
	}

	// 4. Dispatch chat/turnStarted action
	turnID := "turn-" + uuid.NewString()[:8]
	err = rpc.Notify(ctx, "dispatchAction", ahptypes.DispatchActionParams{
		Channel:   chatURI,
		ClientSeq: 1,
		Action: ahptypes.StateAction{
			Value: &ahptypes.ChatTurnStartedAction{
				Type:      ahptypes.ActionTypeChatTurnStarted,
				TurnId:    turnID,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
				Message: ahptypes.Message{
					Text: "Hello test bot!",
					Origin: ahptypes.MessageOrigin{
						Kind: ahptypes.MessageKindUser,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to dispatch turnStarted: %v", err)
	}

	// 5. Verify streaming actions arrive with strictly increasing serverSeq
	var lastSeq int64 = 0
	turnCompleted := false

	for !turnCompleted {
		actionMsg, err := rpc.ReadMessage(ctx)
		if err != nil {
			t.Fatalf("failed to read action: %v", err)
		}
		if actionMsg.Notification == nil {
			continue
		}
		if actionMsg.Notification.Method == "root/sessionSummaryChanged" {
			continue
		}
		if actionMsg.Notification.Method != "action" {
			continue
		}

		var env ahptypes.ActionEnvelope
		if err := json.Unmarshal(actionMsg.Notification.Params, &env); err != nil {
			t.Fatalf("failed to unmarshal ActionEnvelope: %v", err)
		}

		if env.ServerSeq <= lastSeq {
			t.Errorf("expected monotonic serverSeq, got %d <= %d", env.ServerSeq, lastSeq)
		}
		lastSeq = env.ServerSeq

		switch env.Action.Value.(type) {
		case *ahptypes.ChatTurnStartedAction:
			// Turn started echo
		case *ahptypes.ChatResponsePartAction:
			// Assistant response part created
		case *ahptypes.ChatDeltaAction:
			// Assistant text streaming chunk
		case *ahptypes.ChatTurnCompleteAction:
			turnCompleted = true
		case *ahptypes.ChatErrorAction:
			t.Fatalf("turn errored: %+v", env.Action.Value)
		}
	}

	if !turnCompleted {
		t.Errorf("chat turn never completed")
	}

	// 6. Dispose session
	dispResp, err := rpc.Call(ctx, "disposeSession", ahptypes.DisposeSessionParams{
		Channel: sessionURI,
	})
	if err != nil || dispResp.SuccessResponse == nil {
		t.Fatalf("disposeSession failed: %+v, err: %v", dispResp, err)
	}

	// Verify root/sessionRemoved notification
	var removedMsg *ahptypes.JsonRpcMessage
	for {
		msg, err := rpc.ReadMessage(ctx)
		if err != nil {
			t.Fatalf("failed to read sessionRemoved: %v", err)
		}
		if msg.Notification != nil && msg.Notification.Method == "root/sessionRemoved" {
			removedMsg = msg
			break
		}
	}
	if removedMsg == nil {
		t.Fatalf("never received root/sessionRemoved notification")
	}
}

func TestAHPAutomationsAndWebhookTrigger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ts, _ := setupTestAHP(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ahp"

	rpc, err := newWSRPC(ctx, wsURL)
	if err != nil {
		t.Fatalf("failed to connect ws: %v", err)
	}
	defer rpc.Close()

	// Initialize with automations subscription
	_, err = rpc.Call(ctx, "initialize", ahptypes.InitializeParams{
		Channel:              "ahp-root://",
		ProtocolVersions:     []string{"1.1.0"},
		ClientId:             "automation-tester",
		InitialSubscriptions: []string{"ahp-automations://"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Run automation via standard AHP runAutomation command
	automationURI := "ahp-automation:/test-bot-test-flow"
	runResp, err := rpc.Call(ctx, "runAutomation", ahptypes.RunAutomationParams{
		Channel:    "ahp-automations://",
		Automation: automationURI,
		RequestId:  "req-1",
	})
	if err != nil || runResp.SuccessResponse == nil {
		t.Fatalf("runAutomation failed: %+v, err: %v", runResp, err)
	}

	var runRes ahptypes.RunAutomationResult
	_ = json.Unmarshal(runResp.SuccessResponse.Result, &runRes)
	if !strings.HasPrefix(runRes.Resource, "ahp-automation-run:/") {
		t.Fatalf("expected ahp-automation-run URI, got %s", runRes.Resource)
	}

	// 2. Subscribe to automation run channel
	runSubResp, err := rpc.Call(ctx, "subscribe", ahptypes.SubscribeParams{
		Channel: runRes.Resource,
	})
	if err != nil || runSubResp.SuccessResponse == nil {
		t.Fatalf("subscribe to automation run failed: %+v, err: %v", runSubResp, err)
	}
	var subRes ahptypes.SubscribeResult
	_ = json.Unmarshal(runSubResp.SuccessResponse.Result, &subRes)
	if subRes.Snapshot == nil || subRes.Snapshot.State.AutomationRun == nil {
		t.Fatalf("expected AutomationRunState snapshot, got: %+v", subRes)
	}
}

func TestAHPReconnectAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ts, host := setupTestAHP(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ahp"

	// 1. First client connects and establishes sequence history
	rpc1, err := newWSRPC(ctx, wsURL)
	if err != nil {
		t.Fatal(err)
	}

	initResp, err := rpc1.Call(ctx, "initialize", ahptypes.InitializeParams{
		Channel:              "ahp-root://",
		ProtocolVersions:     []string{"1.1.0"},
		ClientId:             "client-reconnect",
		InitialSubscriptions: []string{"ahp-root://"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var initRes ahptypes.InitializeResult
	_ = json.Unmarshal(initResp.SuccessResponse.Result, &initRes)
	initialSeq := initRes.ServerSeq

	// Create a session to generate actions and bump serverSeq
	createResp, err := rpc1.Call(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel:  "ahp-session:/reconnect-sess",
		Provider: ptr("test-bot"),
	})
	if err != nil || createResp.SuccessResponse == nil {
		t.Fatal("createSession failed")
	}

	currentSeq := host.CurrentSeq()
	if currentSeq <= initialSeq {
		t.Fatalf("expected serverSeq to advance, got %d <= %d", currentSeq, initialSeq)
	}

	_ = rpc1.Close()

	// 2. Client reconnects with reconnect command specifying lastSeenServerSeq: initialSeq
	rpc2, err := newWSRPC(ctx, wsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc2.Close()

	reconResp, err := rpc2.Call(ctx, "reconnect", ahptypes.ReconnectParams{
		Channel:           "ahp-root://",
		ClientId:          "client-reconnect",
		LastSeenServerSeq: initialSeq,
		Subscriptions:     []string{"ahp-session:/reconnect-sess"},
	})
	if err != nil || reconResp.SuccessResponse == nil {
		t.Fatalf("reconnect failed: %+v, err: %v", reconResp, err)
	}

	var reconRes ahptypes.ReconnectResult
	if err := json.Unmarshal(reconResp.SuccessResponse.Result, &reconRes); err != nil {
		t.Fatalf("failed to unmarshal ReconnectResult: %v", err)
	}
	replayRes, ok := reconRes.Value.(*ahptypes.ReconnectReplayResult)
	if !ok {
		t.Fatalf("expected ReconnectReplayResult, got: %+v", reconRes.Value)
	}
	if len(replayRes.Actions) == 0 {
		t.Errorf("expected replayed actions for missed session actions, got 0")
	}
}
