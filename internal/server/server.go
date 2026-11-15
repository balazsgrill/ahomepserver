package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/balazsgrill/ahomepserver/internal/agent"
	"github.com/balazsgrill/ahomepserver/internal/config"
	"github.com/balazsgrill/ahomepserver/internal/persona"
	"github.com/balazsgrill/ahomepserver/internal/store"
	"github.com/google/uuid"
)

//go:embed ui/*
var uiFS embed.FS

// Server exposes the AHP REST API, SSE endpoints, and embedded Web UI.
type Server struct {
	cfg      *config.Config
	tokens   *config.TokenResolver
	personas *persona.Manager
	store    store.Store
	runner   *agent.Runner
	mux      *http.ServeMux
}

// New creates and wires a new AHP HTTP Server.
func New(
	cfg *config.Config,
	tokens *config.TokenResolver,
	personas *persona.Manager,
	st store.Store,
	runner *agent.Runner,
) *Server {
	s := &Server{
		cfg:      cfg,
		tokens:   tokens,
		personas: personas,
		store:    st,
		runner:   runner,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// Handler returns the root http.Handler with optional API key protection.
func (s *Server) Handler() http.Handler {
	return s.authMiddleware(s.mux)
}

func (s *Server) registerRoutes() {
	// Health
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	// Personas
	s.mux.HandleFunc("GET /api/v1/personas", s.handleListPersonas)
	s.mux.HandleFunc("GET /api/v1/personas/{id}", s.handleGetPersona)
	s.mux.HandleFunc("POST /api/v1/personas/reload", s.handleReloadPersonas)

	// End-user Sessions
	s.mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession)
	s.mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
	s.mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleDeleteSession)
	s.mux.HandleFunc("POST /api/v1/sessions/{id}/messages", s.handleSendMessage)

	// Automated Workflows (triggered by external servers)
	s.mux.HandleFunc("POST /api/v1/workflows/trigger", s.handleTriggerWorkflow)
	s.mux.HandleFunc("GET /api/v1/workflows/runs", s.handleListWorkflowRuns)
	s.mux.HandleFunc("GET /api/v1/workflows/runs/{id}", s.handleGetWorkflowRun)

	// Embedded Web UI
	sub, err := fs.Sub(uiFS, "ui")
	if err == nil {
		s.mux.Handle("GET /", http.FileServer(http.FS(sub)))
	}
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey == "" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		// Allow browser UI requests from same-origin if no header is set, or require Bearer / X-API-Key
		key := r.Header.Get("X-API-Key")
		if key == "" {
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				key = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
			}
		}
		if key != s.cfg.APIKey {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"monitor": s.personas.Status(),
	})
}

type personaView struct {
	*persona.Persona
	LLMTokenResolved bool            `json:"llm_token_resolved"`
	MCPServersView   []mcpServerView `json:"mcp_servers"`
}

type mcpServerView struct {
	persona.MCPServerConfig
	TokenResolved bool `json:"token_resolved"`
}

func (s *Server) enrichPersona(p *persona.Persona) personaView {
	mcpViews := make([]mcpServerView, 0, len(p.MCPServers))
	for _, m := range p.MCPServers {
		mcpViews = append(mcpViews, mcpServerView{
			MCPServerConfig: m,
			TokenResolved:   m.TokenKey == "" || s.tokens.Resolve(m.TokenKey) != "",
		})
	}
	return personaView{
		Persona:          p,
		LLMTokenResolved: p.LLM.TokenKey != "" && s.tokens.Resolve(p.LLM.TokenKey) != "",
		MCPServersView:   mcpViews,
	}
}

func (s *Server) handleListPersonas(w http.ResponseWriter, _ *http.Request) {
	list := s.personas.List()
	views := make([]personaView, 0, len(list))
	for _, p := range list {
		views = append(views, s.enrichPersona(p))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"personas": views,
		"status":   s.personas.Status(),
	})
}

func (s *Server) handleGetPersona(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := s.personas.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "persona not found"})
		return
	}
	writeJSON(w, http.StatusOK, s.enrichPersona(p))
}

func (s *Server) handleReloadPersonas(w http.ResponseWriter, _ *http.Request) {
	changed, err := s.personas.Scan()
	resp := map[string]interface{}{
		"changed": changed,
		"status":  s.personas.Status(),
	}
	if err != nil {
		resp["warning"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PersonaID string            `json:"persona_id"`
		Title     string            `json:"title"`
		Metadata  map[string]string `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.PersonaID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "persona_id is required"})
		return
	}
	p, ok := s.personas.Get(req.PersonaID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("persona %q not found", req.PersonaID)})
		return
	}
	now := time.Now().UTC()
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = fmt.Sprintf("Session with %s", p.Name)
	}
	sess := &store.Session{
		ID:        uuid.NewString(),
		PersonaID: p.ID,
		Title:     title,
		Metadata:  req.Metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateSession(r.Context(), sess); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	personaID := r.URL.Query().Get("persona_id")
	sessions, err := s.store.ListSessions(r.Context(), personaID, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []store.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sessions": sessions})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteSession(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	sess, err := s.store.GetSession(r.Context(), sessionID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	p, ok := s.personas.Get(sess.PersonaID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("persona %q is no longer loaded", sess.PersonaID)})
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "non-empty content is required"})
		return
	}

	userMsg := store.Message{
		ID:        uuid.NewString(),
		SessionID: sessionID,
		Role:      "user",
		Content:   strings.TrimSpace(req.Content),
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.AddMessage(r.Context(), &userMsg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	history := append(sess.Messages, userMsg)

	streamMode := r.URL.Query().Get("stream") == "true" || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	if streamMode {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)

		genMsgs, _, runErr := s.runner.RunTurn(r.Context(), p, sessionID, history, func(ev agent.Event) {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		})
		for i := range genMsgs {
			_ = s.store.AddMessage(r.Context(), &genMsgs[i])
		}
		if runErr != nil {
			errMsg := store.Message{
				ID:        uuid.NewString(),
				SessionID: sessionID,
				Role:      "assistant",
				Content:   fmt.Sprintf("⚠️ Agent error: %v", runErr),
				CreatedAt: time.Now().UTC(),
			}
			_ = s.store.AddMessage(r.Context(), &errMsg)
		}
		return
	}

	genMsgs, reply, runErr := s.runner.RunTurn(r.Context(), p, sessionID, history, nil)
	for i := range genMsgs {
		_ = s.store.AddMessage(r.Context(), &genMsgs[i])
	}
	if runErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"error":    runErr.Error(),
			"messages": genMsgs,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"reply":    reply,
		"messages": genMsgs,
	})
}

func (s *Server) handleTriggerWorkflow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PersonaID     string                 `json:"persona_id"`
		WorkflowName  string                 `json:"workflow_name"`
		TriggerSource string                 `json:"trigger_source"`
		Prompt        string                 `json:"prompt"`
		Input         map[string]interface{} `json:"input"`
		WebhookURL    string                 `json:"webhook_url"`
		Async         bool                   `json:"async"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.PersonaID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "persona_id is required"})
		return
	}
	p, ok := s.personas.Get(req.PersonaID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("persona %q not found", req.PersonaID)})
		return
	}

	// Build prompt from workflow template if matched or use provided prompt + input payload
	prompt := strings.TrimSpace(req.Prompt)
	for _, wf := range p.Workflows {
		if strings.EqualFold(wf.Name, req.WorkflowName) && wf.PromptTemplate != "" && prompt == "" {
			prompt = wf.PromptTemplate
			break
		}
	}
	if len(req.Input) > 0 {
		inBytes, _ := json.MarshalIndent(req.Input, "", "  ")
		if prompt != "" {
			prompt = fmt.Sprintf("%s\n\n## Workflow Input Payload\n```json\n%s\n```", prompt, string(inBytes))
		} else {
			prompt = fmt.Sprintf("Process the following workflow input payload:\n```json\n%s\n```", string(inBytes))
		}
	}
	if prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt or input payload is required"})
		return
	}

	now := time.Now().UTC()
	source := req.TriggerSource
	if source == "" {
		source = r.RemoteAddr
	}
	wfName := req.WorkflowName
	if wfName == "" {
		wfName = "default"
	}

	run := &store.WorkflowRun{
		ID:            uuid.NewString(),
		PersonaID:     p.ID,
		WorkflowName:  wfName,
		TriggerSource: source,
		InputPayload:  req.Input,
		Prompt:        prompt,
		WebhookURL:    req.WebhookURL,
		Status:        "pending",
		CreatedAt:     now,
	}
	if err := s.store.CreateWorkflowRun(r.Context(), run); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if req.Async {
		go s.executeWorkflowRun(context.Background(), p, run)
		writeJSON(w, http.StatusAccepted, run)
		return
	}

	s.executeWorkflowRun(r.Context(), p, run)
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) executeWorkflowRun(ctx context.Context, p *persona.Persona, run *store.WorkflowRun) {
	started := time.Now().UTC()
	run.Status = "running"
	run.StartedAt = &started
	_ = s.store.UpdateWorkflowRun(ctx, run)

	history := []store.Message{
		{
			ID:        uuid.NewString(),
			SessionID: run.ID,
			Role:      "user",
			Content:   run.Prompt,
			CreatedAt: started,
		},
	}

	_, reply, err := s.runner.RunTurn(ctx, p, run.ID, history, func(ev agent.Event) {
		if ev.Type == "tool_call" || ev.Type == "tool_result" || ev.Type == "error" {
			run.Steps = append(run.Steps, store.WorkflowStep{
				Timestamp: ev.Timestamp,
				Type:      ev.Type,
				Name:      ev.Name,
				Input:     ev.Input,
				Output:    ev.Output,
			})
			_ = s.store.UpdateWorkflowRun(ctx, run)
		}
	})

	finished := time.Now().UTC()
	run.CompletedAt = &finished
	if err != nil {
		run.Status = "failed"
		run.Error = err.Error()
	} else {
		run.Status = "completed"
		run.Output = reply
	}
	if updateErr := s.store.UpdateWorkflowRun(ctx, run); updateErr != nil {
		slog.Warn("failed to update workflow run", "id", run.ID, "err", updateErr)
	}

	// Send webhook callback if configured
	if run.WebhookURL != "" {
		b, _ := json.Marshal(run)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, run.WebhookURL, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			client := &http.Client{Timeout: 15 * time.Second}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
	}
}

func (s *Server) handleListWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	personaID := r.URL.Query().Get("persona_id")
	status := r.URL.Query().Get("status")
	runs, err := s.store.ListWorkflowRuns(r.Context(), personaID, status, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if runs == nil {
		runs = []store.WorkflowRun{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"runs": runs})
}

func (s *Server) handleGetWorkflowRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.store.GetWorkflowRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workflow run not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

