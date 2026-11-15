package store

import (
	"context"
	"sort"
	"sync"
)

// MemoryStore is an in-memory implementation of Store for testing and local dev fallback.
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	runs     map[string]*WorkflowRun
}

// NewMemoryStore creates an initialized MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions: make(map[string]*Session),
		runs:     make(map[string]*WorkflowRun),
	}
}

func (m *MemoryStore) CreateSession(_ context.Context, s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	cp.Messages = append([]Message(nil), s.Messages...)
	m.sessions[s.ID] = &cp
	return nil
}

func (m *MemoryStore) GetSession(_ context.Context, id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	cp.Messages = append([]Message(nil), s.Messages...)
	return &cp, nil
}

func (m *MemoryStore) ListSessions(_ context.Context, personaID string, limit int) ([]Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Session
	for _, s := range m.sessions {
		if personaID != "" && s.PersonaID != personaID {
			continue
		}
		cp := *s
		cp.Messages = nil
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return ErrNotFound
	}
	delete(m.sessions, id)
	return nil
}

func (m *MemoryStore) AddMessage(_ context.Context, msg *Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[msg.SessionID]
	if !ok {
		return ErrNotFound
	}
	s.Messages = append(s.Messages, *msg)
	s.UpdatedAt = msg.CreatedAt
	return nil
}

func (m *MemoryStore) CreateWorkflowRun(_ context.Context, run *WorkflowRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *run
	cp.Steps = append([]WorkflowStep(nil), run.Steps...)
	m.runs[run.ID] = &cp
	return nil
}

func (m *MemoryStore) UpdateWorkflowRun(_ context.Context, run *WorkflowRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[run.ID]; !ok {
		return ErrNotFound
	}
	cp := *run
	cp.Steps = append([]WorkflowStep(nil), run.Steps...)
	m.runs[run.ID] = &cp
	return nil
}

func (m *MemoryStore) GetWorkflowRun(_ context.Context, id string) (*WorkflowRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	cp.Steps = append([]WorkflowStep(nil), r.Steps...)
	return &cp, nil
}

func (m *MemoryStore) ListWorkflowRuns(_ context.Context, personaID, status string, limit int) ([]WorkflowRun, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []WorkflowRun
	for _, r := range m.runs {
		if personaID != "" && r.PersonaID != personaID {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		cp := *r
		cp.Steps = append([]WorkflowStep(nil), r.Steps...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) Close() error {
	return nil
}

