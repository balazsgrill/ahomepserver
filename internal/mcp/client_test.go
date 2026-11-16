package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListToolsLargeSSEResponse(t *testing.T) {
	largeDesc := strings.Repeat("A", 128*1024) // 128 KB > default 64 KB bufio.MaxScanTokenSize

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "text/event-stream")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"protocolVersion\":\"2024-11-05\"}}\n\n", req.ID)
		case "tools/list":
			payload, _ := json.Marshal(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name":        "dokeep_search_documents",
							"description": largeDesc,
							"inputSchema": map[string]interface{}{"type": "object"},
						},
					},
				},
			})
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", string(payload))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	client := NewClient("gateway", srv.URL, "http", "test-token", nil)
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools failed on >64KB SSE line: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "dokeep_search_documents" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
}

