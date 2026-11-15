package persona

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// ScanStatus reports metadata about the personas directory or in-memory git monitor.
type ScanStatus struct {
	Directory    string    `json:"directory"`
	SourceType   string    `json:"source_type"` // "local_dir" or "in_memory_git"
	GitBranch    string    `json:"git_branch,omitempty"`
	ScanInterval string    `json:"scan_interval"`
	LastScanAt   time.Time `json:"last_scan_at"`
	LastChangeAt time.Time `json:"last_change_at"`
	GitRevision  string    `json:"git_revision,omitempty"`
	PersonaCount int       `json:"persona_count"`
	ScanErrors   []string  `json:"scan_errors,omitempty"`
}

// GitOptions configures an in-memory go-git repository for personas.
type GitOptions struct {
	URL           string
	Branch        string
	SubDir        string
	TokenProvider func() string // Optional callback returning an HTTPS token
}

// Manager monitors a personas folder (local OS directory or in-memory go-git repository)
// at a specified cadence and maintains active personas in memory.
type Manager struct {
	dir          string
	sourceType   string
	interval     time.Duration
	onReloadHook func()

	// In-memory go-git fields
	gitOpts    *GitOptions
	gitRepo    *git.Repository
	worktreeFS billy.Filesystem

	mu           sync.RWMutex
	personas     map[string]*Persona
	lastScanAt   time.Time
	lastChangeAt time.Time
	gitRevision  string
	scanErrors   []string
}

// NewManager creates a new persona directory manager backed by a local OS directory.
func NewManager(dir string, interval time.Duration, onReloadHook func()) *Manager {
	return &Manager{
		dir:          dir,
		sourceType:   "local_dir",
		interval:     interval,
		onReloadHook: onReloadHook,
		personas:     make(map[string]*Persona),
	}
}

// NewGitManager creates a persona manager backed by an in-memory go-git repository cloned from opts.URL.
func NewGitManager(opts GitOptions, interval time.Duration, onReloadHook func()) *Manager {
	display := opts.URL
	if opts.SubDir != "" && opts.SubDir != "." && opts.SubDir != "/" {
		display = fmt.Sprintf("%s (subdir: %s)", opts.URL, opts.SubDir)
	}
	return &Manager{
		dir:          display,
		sourceType:   "in_memory_git",
		interval:     interval,
		onReloadHook: onReloadHook,
		gitOpts:      &opts,
		personas:     make(map[string]*Persona),
	}
}

// NewInMemoryGitRepoManager creates a persona manager backed by an already-initialized in-memory go-git repository and memfs worktree.
func NewInMemoryGitRepoManager(repo *git.Repository, wtFS billy.Filesystem, subDir string, interval time.Duration, onReloadHook func()) *Manager {
	return &Manager{
		dir:          "memory://git/" + strings.TrimPrefix(subDir, "/"),
		sourceType:   "in_memory_git",
		interval:     interval,
		onReloadHook: onReloadHook,
		gitOpts:      &GitOptions{SubDir: subDir},
		gitRepo:      repo,
		worktreeFS:   wtFS,
		personas:     make(map[string]*Persona),
	}
}

// Start performs an initial scan and launches the background polling loop until ctx is canceled.
func (m *Manager) Start(ctx context.Context) error {
	if m.sourceType == "local_dir" {
		if err := os.MkdirAll(m.dir, 0o755); err != nil {
			return fmt.Errorf("ensuring personas dir %s: %w", m.dir, err)
		}
	}
	if _, err := m.Scan(); err != nil {
		slog.Warn("initial persona scan had errors", "err", err)
	}

	if m.interval <= 0 {
		return nil
	}

	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				changed, err := m.Scan()
				if err != nil {
					slog.Warn("periodic persona scan error", "err", err)
				} else if changed {
					slog.Info("personas reloaded after change detected", "source", m.dir)
				}
			}
		}
	}()

	return nil
}

// Scan inspects the personas folder or pulls the in-memory git repo and reloads any added, updated, or removed personas.
// Returns true if any persona or git revision changed.
func (m *Manager) Scan() (bool, error) {
	if m.onReloadHook != nil {
		m.onReloadHook()
	}

	var fsys billy.Filesystem
	var baseDir string
	var gitRev string
	var errs []string

	if m.sourceType == "in_memory_git" {
		var err error
		fsys, baseDir, gitRev, err = m.syncInMemoryGit()
		if err != nil {
			m.mu.Lock()
			m.lastScanAt = time.Now().UTC()
			m.scanErrors = []string{err.Error()}
			m.mu.Unlock()
			return false, err
		}
	} else {
		fsys = osfs.New(m.dir)
		baseDir = "."
		gitRev = readGitRevision(m.dir)
	}

	entries, err := fsys.ReadDir(baseDir)
	if err != nil {
		return false, fmt.Errorf("reading personas dir %s: %w", m.dir, err)
	}

	next := make(map[string]*Persona)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		subDir := fsys.Join(baseDir, entry.Name())
		p, err := LoadFromBillyFS(fsys, subDir, entry.Name())
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", entry.Name(), err))
			continue
		}
		if m.sourceType == "local_dir" {
			p.DirPath = filepath.Join(m.dir, entry.Name())
		} else {
			p.DirPath = "git://" + subDir
		}
		next[p.ID] = p
	}

	now := time.Now().UTC()

	m.mu.Lock()
	defer m.mu.Unlock()

	changed := gitRev != m.gitRevision || len(next) != len(m.personas)
	if !changed {
		for id, p := range next {
			prev, ok := m.personas[id]
			if !ok || prev.ContentHash != p.ContentHash {
				changed = true
				break
			}
		}
	}

	// Preserve LoadedAt timestamp for unchanged personas
	for id, p := range next {
		if prev, ok := m.personas[id]; ok && prev.ContentHash == p.ContentHash {
			p.LoadedAt = prev.LoadedAt
		} else {
			slog.Info("loaded persona", "id", p.ID, "name", p.Name, "source", m.sourceType, "skills", len(p.Skills), "mcp_servers", len(p.MCPServers))
		}
	}

	m.personas = next
	m.gitRevision = gitRev
	m.lastScanAt = now
	m.scanErrors = errs
	if changed || m.lastChangeAt.IsZero() {
		m.lastChangeAt = now
	}

	if len(errs) > 0 {
		return changed, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return changed, nil
}

func (m *Manager) syncInMemoryGit() (billy.Filesystem, string, string, error) {
	m.mu.Lock()
	repo := m.gitRepo
	wtFS := m.worktreeFS
	opts := m.gitOpts
	m.mu.Unlock()

	var auth transport.AuthMethod
	if opts != nil && opts.TokenProvider != nil {
		if token := strings.TrimSpace(opts.TokenProvider()); token != "" {
			auth = &http.BasicAuth{
				Username: "git",
				Password: token,
			}
		}
	}

	if opts != nil && opts.URL != "" {
		if repo == nil || wtFS == nil {
			newRepo, newFS, err := cloneInMemory(opts.URL, opts.Branch, auth)
			if err != nil {
				return nil, "", "", fmt.Errorf("in-memory git clone (%s): %w", opts.URL, err)
			}
			repo = newRepo
			wtFS = newFS
			m.mu.Lock()
			m.gitRepo = repo
			m.worktreeFS = wtFS
			m.mu.Unlock()
		} else {
			wt, err := repo.Worktree()
			if err == nil {
				pullOpts := &git.PullOptions{
					RemoteName: "origin",
					Auth:       auth,
					Force:      true,
				}
				if opts.Branch != "" {
					pullOpts.ReferenceName = plumbing.NewBranchReferenceName(opts.Branch)
					pullOpts.SingleBranch = true
				}
				pullErr := wt.Pull(pullOpts)
				if pullErr != nil && !errors.Is(pullErr, git.NoErrAlreadyUpToDate) {
					// If remote was force-pushed or diverged, re-clone cleanly into memory
					if newRepo, newFS, cloneErr := cloneInMemory(opts.URL, opts.Branch, auth); cloneErr == nil {
						repo = newRepo
						wtFS = newFS
						m.mu.Lock()
						m.gitRepo = repo
						m.worktreeFS = wtFS
						m.mu.Unlock()
					} else {
						slog.Warn("in-memory git pull failed, keeping existing state", "err", pullErr)
					}
				}
			}
		}
	}

	if wtFS == nil {
		return nil, "", "", fmt.Errorf("in-memory git filesystem not initialized")
	}

	var rev string
	if repo != nil {
		if head, err := repo.Head(); err == nil {
			rev = head.Hash().String()
			if len(rev) > 12 {
				rev = rev[:12]
			}
		}
	}

	subDir := "."
	if opts != nil && strings.TrimSpace(opts.SubDir) != "" {
		subDir = strings.TrimSpace(opts.SubDir)
	}
	return wtFS, subDir, rev, nil
}

func cloneInMemory(repoURL, branch string, auth transport.AuthMethod) (*git.Repository, billy.Filesystem, error) {
	storer := memory.NewStorage()
	wtFS := memfs.New()
	cloneOpts := &git.CloneOptions{
		URL:  repoURL,
		Auth: auth,
	}
	if branch != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(branch)
		cloneOpts.SingleBranch = true
	}
	repo, err := git.Clone(storer, wtFS, cloneOpts)
	if err != nil {
		return nil, nil, err
	}
	return repo, wtFS, nil
}

// List returns all loaded personas sorted by ID.
func (m *Manager) List() []*Persona {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*Persona, 0, len(m.personas))
	for _, p := range m.personas {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns a persona by ID.
func (m *Manager) Get(id string) (*Persona, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.personas[id]
	return p, ok
}

// Status returns the current scan status of the personas directory or in-memory git repository.
func (m *Manager) Status() ScanStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var branch string
	if m.gitOpts != nil {
		branch = m.gitOpts.Branch
	}
	return ScanStatus{
		Directory:    m.dir,
		SourceType:   m.sourceType,
		GitBranch:    branch,
		ScanInterval: m.interval.String(),
		LastScanAt:   m.lastScanAt,
		LastChangeAt: m.lastChangeAt,
		GitRevision:  m.gitRevision,
		PersonaCount: len(m.personas),
		ScanErrors:   append([]string(nil), m.scanErrors...),
	}
}

func readGitRevision(dir string) string {
	headPath := filepath.Join(dir, ".git", "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if strings.HasPrefix(head, "ref: ") {
		ref := strings.TrimSpace(strings.TrimPrefix(head, "ref: "))
		refPath := filepath.Join(dir, ".git", filepath.FromSlash(ref))
		if refData, err := os.ReadFile(refPath); err == nil {
			rev := strings.TrimSpace(string(refData))
			if len(rev) > 12 {
				return rev[:12]
			}
			return rev
		}
		return ref
	}
	if len(head) > 12 {
		return head[:12]
	}
	return head
}
