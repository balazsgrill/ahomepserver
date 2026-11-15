package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Tool represents a tool exposed by an MCP server.
type Tool struct {
	ServerName  string          `json:"server_name"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// QualifiedName returns a collision-free tool name "<server>__<tool>".
func (t Tool) QualifiedName() string {
	return fmt.Sprintf("%s__%s", sanitizeName(t.ServerName), t.Name)
}

func sanitizeName(s string) string {
	var out strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteRune('_')
		}
	}
	return out.String()
}

// Client communicates with an MCP server over Streamable HTTP or SSE.
type Client struct {
	name       string
	endpoint   string
	transport  string
	bearerAuth string
	headers    map[string]string
	httpClient *http.Client

	reqID     atomic.Int64
	mu        sync.Mutex
	sessionID string
	inited    bool
}

// NewClient creates an MCP client for a single MCP server.
func NewClient(name, endpoint, transport, bearerAuth string, headers map[string]string) *Client {
	return &Client{
		name:       name,
		endpoint:   strings.TrimSpace(endpoint),
		transport:  strings.ToLower(strings.TrimSpace(transport)),
		bearerAuth: strings.TrimSpace(bearerAuth),
		headers:    headers,
		httpClient: &http.Client{Timeout: 45 * time.Second},
	}
}

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Initialize performs the MCP initialize handshake if not already initialized.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	if c.inited {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	params := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo": map[string]string{
			"name":    "ahomepserver",
			"version": "0.1.0",
		},
	}

	if _, err := c.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("mcp initialize (%s): %w", c.name, err)
	}

	// Best-effort initialized notification
	_ = c.notify(ctx, "notifications/initialized", map[string]interface{}{})

	c.mu.Lock()
	c.inited = true
	c.mu.Unlock()
	return nil
}

// ListTools queries tools/list on the MCP server.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}

	res, err := c.call(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("mcp tools/list (%s): %w", c.name, err)
	}

	var parsed struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		return nil, fmt.Errorf("decoding tools/list from %s: %w", c.name, err)
	}

	tools := make([]Tool, 0, len(parsed.Tools))
	for _, t := range parsed.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, Tool{
			ServerName:  c.name,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return tools, nil
}

// CallTool invokes tools/call on the MCP server and returns a string representation of the result.
func (c *Client) CallTool(ctx context.Context, toolName string, arguments map[string]interface{}) (string, error) {
	if err := c.Initialize(ctx); err != nil {
		return "", err
	}
	if arguments == nil {
		arguments = map[string]interface{}{}
	}
	params := map[string]interface{}{
		"name":      toolName,
		"arguments": arguments,
	}
	res, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return "", fmt.Errorf("mcp tools/call %s on %s: %w", toolName, c.name, err)
	}

	var toolResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &toolResult); err == nil && len(toolResult.Content) > 0 {
		var parts []string
		for _, item := range toolResult.Content {
			if item.Text != "" {
				parts = append(parts, item.Text)
			}
		}
		out := strings.Join(parts, "\n")
		if toolResult.IsError {
			return out, fmt.Errorf("mcp tool %s returned error: %s", toolName, out)
		}
		return out, nil
	}

	return string(res), nil
}

func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.transport == "sse" {
		return c.callSSE(ctx, method, params)
	}
	return c.callHTTP(ctx, method, params)
}

func (c *Client) callHTTP(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id := c.reqID.Add(1)
	reqPayload := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.mu.Lock()
		c.sessionID = sid
		c.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		return readResponseFromSSE(resp.Body, id)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// callSSE handles legacy MCP SSE servers where GET opens an SSE stream that emits an "endpoint" event.
func (c *Client) callSSE(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	sseCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("SSE connect HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	var eventType string
	var postEndpoint string
	id := c.reqID.Add(1)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if eventType == "endpoint" && postEndpoint == "" {
				resolved, err := resolveURL(c.endpoint, data)
				if err != nil {
					return nil, err
				}
				postEndpoint = resolved
				// Send the JSON-RPC request via POST to postEndpoint
				reqPayload := jsonRPCRequest{
					JSONRPC: "2.0",
					ID:      id,
					Method:  method,
					Params:  params,
				}
				b, _ := json.Marshal(reqPayload)
				postReq, err := http.NewRequestWithContext(sseCtx, http.MethodPost, postEndpoint, bytes.NewReader(b))
				if err != nil {
					return nil, err
				}
				c.applyHeaders(postReq)
				postReq.Header.Set("Content-Type", "application/json")
				postResp, err := c.httpClient.Do(postReq)
				if err != nil {
					return nil, err
				}
				postResp.Body.Close()
			} else if eventType == "message" || eventType == "" {
				var rpcResp jsonRPCResponse
				if err := json.Unmarshal([]byte(data), &rpcResp); err == nil && rpcResp.ID == id {
					if rpcResp.Error != nil {
						return nil, fmt.Errorf("JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
					}
					return rpcResp.Result, nil
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("SSE stream closed before response received")
}

func (c *Client) notify(ctx context.Context, method string, params interface{}) error {
	if c.transport == "sse" {
		return nil
	}
	reqPayload := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(reqPayload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) applyHeaders(req *http.Request) {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.bearerAuth != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearerAuth)
	}
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
}

func readResponseFromSSE(r io.Reader, expectedID int64) (json.RawMessage, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var rpcResp jsonRPCResponse
		if err := json.Unmarshal([]byte(data), &rpcResp); err == nil && rpcResp.ID == expectedID {
			if rpcResp.Error != nil {
				return nil, fmt.Errorf("JSON-RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
			}
			return rpcResp.Result, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no matching JSON-RPC response in SSE stream")
}

func resolveURL(baseStr, refStr string) (string, error) {
	baseURL, err := url.Parse(baseStr)
	if err != nil {
		return "", err
	}
	refURL, err := url.Parse(refStr)
	if err != nil {
		return "", err
	}
	return baseURL.ResolveReference(refURL).String(), nil
}

