package ahp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// ClientConn wraps a single WebSocket connection speaking JSON-RPC 2.0 AHP.
type ClientConn struct {
	host            *Host
	ws              *websocket.Conn
	clientID        string
	clientInfo      *ahptypes.Implementation
	protocolVersion string
	authenticated   bool

	mu            sync.Mutex
	writeMu       sync.Mutex
	subscriptions map[string]bool
}

// NewClientConn wraps an accepted WebSocket into an AHP ClientConn.
func NewClientConn(host *Host, ws *websocket.Conn, authenticated bool) *ClientConn {
	return &ClientConn{
		host:          host,
		ws:            ws,
		authenticated: authenticated,
		subscriptions: make(map[string]bool),
	}
}

// Serve handles the read loop of the connection.
func (c *ClientConn) Serve(ctx context.Context) {
	c.host.RegisterClient(c)
	defer func() {
		c.host.UnregisterClient(c)
		_ = c.ws.Close(websocket.StatusNormalClosure, "connection closed")
	}()

	for {
		typ, msgBytes, err := c.ws.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				slog.Debug("AHP client disconnected", "clientID", c.clientID, "err", err)
			}
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		var msg ahptypes.JsonRpcMessage
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			c.SendError(0, ahptypes.ErrorCodeParseError, fmt.Sprintf("invalid JSON-RPC: %v", err), nil)
			continue
		}

		switch {
		case msg.Request != nil:
			c.handleRequest(ctx, msg.Request)
		case msg.Notification != nil:
			c.handleNotification(ctx, msg.Notification)
		}
	}
}

// SendNotification sends a JSON-RPC notification to the client.
func (c *ClientConn) SendNotification(notif ahptypes.JsonRpcNotification) {
	c.sendMessage(notif)
}

// SendSuccess sends a JSON-RPC success response to the client.
func (c *ClientConn) SendSuccess(id uint64, result any) {
	var raw json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			c.SendError(id, ahptypes.ErrorCodeInternalError, fmt.Sprintf("serialization error: %v", err), nil)
			return
		}
		raw = b
	} else {
		raw = json.RawMessage(`{}`)
	}
	resp := ahptypes.JsonRpcSuccessResponse{
		JsonRpc: ahptypes.JsonRpcV2,
		ID:      id,
		Result:  raw,
	}
	c.sendMessage(resp)
}

// SendError sends a JSON-RPC error response to the client.
func (c *ClientConn) SendError(id uint64, code int32, message string, data any) {
	var rawData json.RawMessage
	if data != nil {
		rawData, _ = json.Marshal(data)
	}
	errResp := ahptypes.JsonRpcErrorResponse{
		JsonRpc: ahptypes.JsonRpcV2,
		ID:      id,
		Error: ahptypes.JsonRpcError{
			Code:    code,
			Message: message,
			Data:    rawData,
		},
	}
	c.sendMessage(errResp)
}

func (c *ClientConn) sendMessage(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.ws.Write(writeCtx, websocket.MessageText, data)
}

// CheckAuth verifies that the client is authorized if an API key is required.
func (c *ClientConn) CheckAuth() bool {
	if c.host.cfg.APIKey == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authenticated
}

// UpgradeHandler returns an http.HandlerFunc that upgrades incoming WebSocket connections to AHP.
func UpgradeHandler(host *Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify optional API Key during HTTP handshake
		authenticated := host.cfg.APIKey == ""
		if !authenticated {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				auth := r.Header.Get("Authorization")
				if strings.HasPrefix(auth, "Bearer ") {
					key = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
				}
			}
			if key == "" {
				key = r.URL.Query().Get("token")
				if key == "" {
					key = r.URL.Query().Get("apiKey")
				}
			}
			if key == host.cfg.APIKey {
				authenticated = true
			}
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // Allow cross-origin / browser access
		})
		if err != nil {
			slog.Warn("failed to accept AHP websocket", "err", err)
			return
		}

		client := NewClientConn(host, conn, authenticated)
		client.Serve(r.Context())
	}
}

