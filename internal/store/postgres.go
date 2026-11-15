package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("not found")

// PostgresStore implements Store backed by PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore connects to PostgreSQL and ensures the schema exists.
func NewPostgresStore(ctx context.Context, connURL string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", connURL)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	s := &PostgresStore{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating postgres schema: %w", err)
	}
	return s, nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	persona_id TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sessions_persona_id ON sessions(persona_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS messages (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	role TEXT NOT NULL,
	content TEXT NOT NULL DEFAULT '',
	tool_calls JSONB NOT NULL DEFAULT '[]'::jsonb,
	tool_call_id TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, created_at ASC);

CREATE TABLE IF NOT EXISTS workflow_runs (
	id TEXT PRIMARY KEY,
	persona_id TEXT NOT NULL,
	workflow_name TEXT NOT NULL DEFAULT '',
	trigger_source TEXT NOT NULL DEFAULT '',
	input_payload JSONB NOT NULL DEFAULT '{}'::jsonb,
	prompt TEXT NOT NULL DEFAULT '',
	webhook_url TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	output TEXT NOT NULL DEFAULT '',
	steps JSONB NOT NULL DEFAULT '[]'::jsonb,
	error TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	started_at TIMESTAMPTZ,
	completed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_persona_status ON workflow_runs(persona_id, status, created_at DESC);
`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

func (s *PostgresStore) CreateSession(ctx context.Context, sess *Session) error {
	metaJSON, err := json.Marshal(sess.Metadata)
	if err != nil || sess.Metadata == nil {
		metaJSON = []byte(`{}`)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, persona_id, title, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, sess.ID, sess.PersonaID, sess.Title, metaJSON, sess.CreatedAt, sess.UpdatedAt)
	return err
}

func (s *PostgresStore) GetSession(ctx context.Context, id string) (*Session, error) {
	var sess Session
	var metaRaw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, persona_id, title, metadata, created_at, updated_at
		FROM sessions WHERE id = $1
	`, id).Scan(&sess.ID, &sess.PersonaID, &sess.Title, &metaRaw, &sess.CreatedAt, &sess.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(metaRaw, &sess.Metadata)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, role, content, tool_calls, tool_call_id, created_at
		FROM messages WHERE session_id = $1 ORDER BY created_at ASC
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var m Message
		var tcRaw []byte
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &tcRaw, &m.ToolCallID, &m.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(tcRaw, &m.ToolCalls)
		sess.Messages = append(sess.Messages, m)
	}
	return &sess, rows.Err()
}

func (s *PostgresStore) ListSessions(ctx context.Context, personaID string, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if personaID != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, persona_id, title, metadata, created_at, updated_at
			FROM sessions WHERE persona_id = $1 ORDER BY updated_at DESC LIMIT $2
		`, personaID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, persona_id, title, metadata, created_at, updated_at
			FROM sessions ORDER BY updated_at DESC LIMIT $1
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var sess Session
		var metaRaw []byte
		if err := rows.Scan(&sess.ID, &sess.PersonaID, &sess.Title, &metaRaw, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metaRaw, &sess.Metadata)
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) AddMessage(ctx context.Context, m *Message) error {
	tcJSON, err := json.Marshal(m.ToolCalls)
	if err != nil || m.ToolCalls == nil {
		tcJSON = []byte(`[]`)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, role, content, tool_calls, tool_call_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, m.ID, m.SessionID, m.Role, m.Content, tcJSON, m.ToolCallID, m.CreatedAt)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE sessions SET updated_at = $1 WHERE id = $2
	`, m.CreatedAt, m.SessionID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) CreateWorkflowRun(ctx context.Context, run *WorkflowRun) error {
	inJSON, err := json.Marshal(run.InputPayload)
	if err != nil || run.InputPayload == nil {
		inJSON = []byte(`{}`)
	}
	stepsJSON, err := json.Marshal(run.Steps)
	if err != nil || run.Steps == nil {
		stepsJSON = []byte(`[]`)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workflow_runs (
			id, persona_id, workflow_name, trigger_source, input_payload, prompt,
			webhook_url, status, output, steps, error, created_at, started_at, completed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`, run.ID, run.PersonaID, run.WorkflowName, run.TriggerSource, inJSON, run.Prompt,
		run.WebhookURL, run.Status, run.Output, stepsJSON, run.Error, run.CreatedAt, run.StartedAt, run.CompletedAt)
	return err
}

func (s *PostgresStore) UpdateWorkflowRun(ctx context.Context, run *WorkflowRun) error {
	stepsJSON, err := json.Marshal(run.Steps)
	if err != nil || run.Steps == nil {
		stepsJSON = []byte(`[]`)
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE workflow_runs
		SET status = $1, output = $2, steps = $3, error = $4, started_at = $5, completed_at = $6
		WHERE id = $7
	`, run.Status, run.Output, stepsJSON, run.Error, run.StartedAt, run.CompletedAt, run.ID)
	return err
}

func (s *PostgresStore) GetWorkflowRun(ctx context.Context, id string) (*WorkflowRun, error) {
	var run WorkflowRun
	var inRaw, stepsRaw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, persona_id, workflow_name, trigger_source, input_payload, prompt,
		       webhook_url, status, output, steps, error, created_at, started_at, completed_at
		FROM workflow_runs WHERE id = $1
	`, id).Scan(
		&run.ID, &run.PersonaID, &run.WorkflowName, &run.TriggerSource, &inRaw, &run.Prompt,
		&run.WebhookURL, &run.Status, &run.Output, &stepsRaw, &run.Error, &run.CreatedAt, &run.StartedAt, &run.CompletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(inRaw, &run.InputPayload)
	_ = json.Unmarshal(stepsRaw, &run.Steps)
	return &run, nil
}

func (s *PostgresStore) ListWorkflowRuns(ctx context.Context, personaID, status string, limit int) ([]WorkflowRun, error) {
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT id, persona_id, workflow_name, trigger_source, input_payload, prompt,
		       webhook_url, status, output, steps, error, created_at, started_at, completed_at
		FROM workflow_runs WHERE 1=1
	`
	var args []interface{}
	idx := 1
	if personaID != "" {
		query += fmt.Sprintf(" AND persona_id = $%d", idx)
		args = append(args, personaID)
		idx++
	}
	if status != "" {
		query += fmt.Sprintf(" AND status = $%d", idx)
		args = append(args, status)
		idx++
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", idx)
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WorkflowRun
	for rows.Next() {
		var run WorkflowRun
		var inRaw, stepsRaw []byte
		if err := rows.Scan(
			&run.ID, &run.PersonaID, &run.WorkflowName, &run.TriggerSource, &inRaw, &run.Prompt,
			&run.WebhookURL, &run.Status, &run.Output, &stepsRaw, &run.Error, &run.CreatedAt, &run.StartedAt, &run.CompletedAt,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(inRaw, &run.InputPayload)
		_ = json.Unmarshal(stepsRaw, &run.Steps)
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}

