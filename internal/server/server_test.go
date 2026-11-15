package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/balazsgrill/ahomepserver/internal/agent"
	"github.com/balazsgrill/ahomepserver/internal/config"
	"github.com/balazsgrill/ahomepserver/internal/persona"
	"github.com/balazsgrill/ahomepserver/internal/store"
)

func TestEndUserSessionAndWorkflowIntegration(t *testing.T) {
	// 1. Mock MCP Server
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-mcp-token" {
			http.Error(w, "unauthorized mcp", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name":        "get_temperature",
							"description": "Reads room temperature",
							"inputSchema": map[string]interface{}{"type": "object"},
						},
					},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"content": []map[string]string{
						{"type": "text", "text": "21.5 C"},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer mcpSrv.Close()

	// 2. Mock OpenAI-compatible LLM Server
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-llm-token" {
			http.Error(w, "unauthorized llm", http.StatusUnauthorized)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		hasToolResult := false
		for _, m := range req.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "21.5 C") {
				hasToolResult = true
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if !hasToolResult {
			// Turn 1: Request MCP tool call
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{
					{
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "",
							"tool_calls": []map[string]interface{}{
								{
									"id":   "call_1",
									"type": "function",
									"function": map[string]string{
										"name":      "sensors__get_temperature",
										"arguments": `{"room":"living_room"}`,
									},
								},
							},
						},
					},
				},
			})
			return
		}

		// Turn 2: Return final answer incorporating MCP tool result
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "The living room temperature is 21.5 C.",
					},
				},
			},
		})
	}))
	defer llmSrv.Close()

	// 3. Set up persona folder and tokens file
	tmpDir := t.TempDir()
	tokensFile := filepath.Join(tmpDir, "tokens.env")
	_ = os.WriteFile(tokensFile, []byte("TEST_LLM_KEY=secret-llm-token\nTEST_MCP_KEY=secret-mcp-token\n"), 0o600)

	personasDir := filepath.Join(tmpDir, "personas")
	pDir := filepath.Join(personasDir, "homebot")
	_ = os.MkdirAll(filepath.Join(pDir, "skills"), 0o755)
	cfgContent := "id: homebot\nname: HomeBot\nllm:\n  endpoint: " + llmSrv.URL + "\n  model: test-model\n  token_key: TEST_LLM_KEY\nmcp_servers:\n  - name: sensors\n    url: " + mcpSrv.URL + "\n    token_key: TEST_MCP_KEY\n"
	_ = os.WriteFile(filepath.Join(pDir, "config.yaml"), []byte(cfgContent), 0o644)
	_ = os.WriteFile(filepath.Join(pDir, "prompt.md"), []byte("You are HomeBot."), 0o644)
	_ = os.WriteFile(filepath.Join(pDir, "skills", "climate.md"), []byte("Check sensors before answering climate questions."), 0o644)

	cfg := &config.Config{
		ListenAddr:   ":0",
		DatabaseURL:  "memory://",
		PersonasDir:  personasDir,
		ScanInterval: time.Minute,
		TokensFile:   tokensFile,
		MaxAgentTurn: 5,
	}
	tokens := config.NewTokenResolver(tokensFile)
	pMgr := persona.NewManager(personasDir, 0, tokens.Reload)
	if _, err := pMgr.Scan(); err != nil {
		t.Fatalf("persona scan failed: %v", err)
	}
	memStore := store.NewMemoryStore()
	runner := agent.NewRunner(tokens, 5)
	srv := New(cfg, tokens, pMgr, memStore, runner)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 4. Test End-User Session Flow
	createBody := `{"persona_id":"homebot","title":"Temp Check"}`
	resp, err := http.Post(ts.URL+"/api/v1/sessions", "application/json", strings.NewReader(createBody))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session failed: status=%v err=%v", resp.StatusCode, err)
	}
	var createdSess store.Session
	_ = json.NewDecoder(resp.Body).Decode(&createdSess)
	resp.Body.Close()

	msgBody := `{"content":"What is the living room temperature?"}`
	resp, err = http.Post(ts.URL+"/api/v1/sessions/"+createdSess.ID+"/messages", "application/json", strings.NewReader(msgBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("send message failed: status=%v err=%v", resp.StatusCode, err)
	}
	var msgResp struct {
		Reply string `json:"reply"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&msgResp)
	resp.Body.Close()

	if msgResp.Reply != "The living room temperature is 21.5 C." {
		t.Errorf("unexpected reply: %q", msgResp.Reply)
	}

	// 5. Test Automated Workflow Trigger Flow
	wfReq := map[string]interface{}{
		"persona_id":    "homebot",
		"workflow_name": "climate-audit",
		"prompt":        "Run climate check",
		"input":         map[string]interface{}{"zone": "living_room"},
		"async":         false,
	}
	wfBytes, _ := json.Marshal(wfReq)
	resp, err = http.Post(ts.URL+"/api/v1/workflows/trigger", "application/json", bytes.NewReader(wfBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("workflow trigger failed: status=%v err=%v", resp.StatusCode, err)
	}
	var wfRun store.WorkflowRun
	_ = json.NewDecoder(resp.Body).Decode(&wfRun)
	resp.Body.Close()

	if wfRun.Status != "completed" {
		t.Errorf("expected workflow status=completed, got %s (err=%s)", wfRun.Status, wfRun.Error)
	}
	if wfRun.Output != "The living room temperature is 21.5 C." {
		t.Errorf("unexpected workflow output: %q", wfRun.Output)
	}
	if len(wfRun.Steps) < 2 {
		t.Errorf("expected workflow tool call steps to be recorded, got %d", len(wfRun.Steps))
	}
}

