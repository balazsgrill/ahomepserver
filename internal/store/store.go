package store

import (
	"context"
	"time"
)

// ToolCallFunction represents an OpenAI-style function invocation.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall represents a tool call made by the assistant.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// Message represents a single chat message in a session.
type Message struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"session_id"`
	Role       string     `json:"role"` // "system", "user", "assistant", "tool"
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Session represents an end-user conversation session with a persona.
type Session struct {
	ID        string            `json:"id"`
	PersonaID string            `json:"persona_id"`
	Title     string            `json:"title"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Messages  []Message         `json:"messages,omitempty"`
}

// WorkflowStep records a single step (e.g. tool call or LLM turn) during an automated workflow run.
type WorkflowStep struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"` // "llm_call", "tool_call", "tool_result", "error"
	Name      string    `json:"name,omitempty"`
	Input     string    `json:"input,omitempty"`
	Output    string    `json:"output,omitempty"`
}

// WorkflowRun represents an automated workflow execution triggered by an external server or API.
type WorkflowRun struct {
	ID            string                 `json:"id"`
	PersonaID     string                 `json:"persona_id"`
	WorkflowName  string                 `json:"workflow_name"`
	TriggerSource string                 `json:"trigger_source"`
	InputPayload  map[string]interface{} `json:"input_payload,omitempty"`
	Prompt        string                 `json:"prompt"`
	WebhookURL    string                 `json:"webhook_url,omitempty"`
	Status        string                 `json:"status"` // "pending", "running", "completed", "failed"
	Output        string                 `json:"output,omitempty"`
	Steps         []WorkflowStep         `json:"steps,omitempty"`
	Error         string                 `json:"error,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	StartedAt     *time.Time             `json:"started_at,omitempty"`
	CompletedAt   *time.Time             `json:"completed_at,omitempty"`
}

// Store defines the persistence operations for sessions and workflow runs.
type Store interface {
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	ListSessions(ctx context.Context, personaID string, limit int) ([]Session, error)
	DeleteSession(ctx context.Context, id string) error
	AddMessage(ctx context.Context, m *Message) error

	CreateWorkflowRun(ctx context.Context, run *WorkflowRun) error
	UpdateWorkflowRun(ctx context.Context, run *WorkflowRun) error
	GetWorkflowRun(ctx context.Context, id string) (*WorkflowRun, error)
	ListWorkflowRuns(ctx context.Context, personaID, status string, limit int) ([]WorkflowRun, error)

	Close() error
}

