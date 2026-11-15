package persona

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

func TestManagerScanAndReload(t *testing.T) {
	tmpDir := t.TempDir()

	pDir := filepath.Join(tmpDir, "ops-agent")
	if err := os.MkdirAll(filepath.Join(pDir, "skills", "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgYAML := `
id: ops-agent
name: Ops Agent
description: Automated operations agent
llm:
  endpoint: https://api.openai.com/v1
  model: gpt-4o
  token_key: OPS_LLM_KEY
mcp_servers:
  - name: k8s
    url: http://localhost:9000/mcp
    token_key: K8S_MCP_KEY
`
	if err := os.WriteFile(filepath.Join(pDir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pDir, "prompt.md"), []byte("You are Ops Agent."), 0o644); err != nil {
		t.Fatal(err)
	}
	skillMd := "---\nname: k8s-deploy\ndescription: Rollout verification skill\n---\nCheck pods before completing rollout."
	if err := os.WriteFile(filepath.Join(pDir, "skills", "deploy", "SKILL.md"), []byte(skillMd), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(tmpDir, 50*time.Millisecond, nil)
	changed, err := mgr.Scan()
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on initial scan")
	}

	p, ok := mgr.Get("ops-agent")
	if !ok {
		t.Fatalf("expected ops-agent to be loaded")
	}
	if p.LLM.TokenKey != "OPS_LLM_KEY" {
		t.Errorf("unexpected LLM token key: %s", p.LLM.TokenKey)
	}
	if len(p.MCPServers) != 1 || p.MCPServers[0].TokenKey != "K8S_MCP_KEY" {
		t.Errorf("unexpected MCP servers: %+v", p.MCPServers)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "k8s-deploy" {
		t.Errorf("unexpected skills: %+v", p.Skills)
	}

	// Second scan without file modifications should report changed=false
	changed, err = mgr.Scan()
	if err != nil {
		t.Fatalf("second Scan failed: %v", err)
	}
	if changed {
		t.Errorf("expected changed=false when nothing changed")
	}

	// Modify prompt.md (simulating git pull / cron update)
	if err := os.WriteFile(filepath.Join(pDir, "prompt.md"), []byte("You are Ops Agent v2."), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = mgr.Scan()
	if err != nil {
		t.Fatalf("third Scan failed: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true after prompt.md update")
	}
	p2, _ := mgr.Get("ops-agent")
	if p2.Instructions != "You are Ops Agent v2." {
		t.Errorf("expected updated instructions, got %q", p2.Instructions)
	}
}

func TestInMemoryGitRepoManager(t *testing.T) {
	// 1. Initialize a pure in-memory go-git repository
	storer := memory.NewStorage()
	wtFS := memfs.New()
	repo, err := git.Init(storer, wtFS)
	if err != nil {
		t.Fatalf("git.Init failed: %v", err)
	}

	writeBillyFile(t, wtFS, "personas/sec-bot/config.yaml", `
id: sec-bot
name: Security Bot
llm:
  endpoint: https://api.openai.com/v1
  model: gpt-4o-mini
  token_key: SEC_LLM_TOKEN
`)
	writeBillyFile(t, wtFS, "personas/sec-bot/prompt.md", "You are Security Bot v1.")
	writeBillyFile(t, wtFS, "personas/sec-bot/skills/audit.md", "Audit logs carefully.")

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("repo.Worktree failed: %v", err)
	}
	if _, err := wt.Add("personas"); err != nil {
		t.Fatalf("wt.Add failed: %v", err)
	}
	sig := &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}
	commit1, err := wt.Commit("initial personas commit", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatalf("wt.Commit failed: %v", err)
	}

	mgr := NewInMemoryGitRepoManager(repo, wtFS, "personas", 0, nil)
	changed, err := mgr.Scan()
	if err != nil {
		t.Fatalf("Scan failed: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on initial scan")
	}

	p, ok := mgr.Get("sec-bot")
	if !ok {
		t.Fatalf("expected sec-bot to be loaded from in-memory git repo")
	}
	if p.Instructions != "You are Security Bot v1." {
		t.Errorf("unexpected instructions: %q", p.Instructions)
	}
	if len(p.Skills) != 1 || p.Skills[0].Name != "audit" {
		t.Errorf("unexpected skills: %+v", p.Skills)
	}
	st := mgr.Status()
	if st.SourceType != "in_memory_git" || st.GitRevision != commit1.String()[:12] {
		t.Errorf("unexpected status: %+v", st)
	}

	// 2. Commit an update in the in-memory git repo and verify Scan() detects it
	writeBillyFile(t, wtFS, "personas/sec-bot/prompt.md", "You are Security Bot v2.")
	_, _ = wt.Add("personas/sec-bot/prompt.md")
	commit2, err := wt.Commit("update prompt to v2", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatalf("second commit failed: %v", err)
	}

	changed, err = mgr.Scan()
	if err != nil {
		t.Fatalf("second Scan failed: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true after new commit in in-memory git repo")
	}
	pUpdated, _ := mgr.Get("sec-bot")
	if pUpdated.Instructions != "You are Security Bot v2." {
		t.Errorf("expected updated instructions, got %q", pUpdated.Instructions)
	}
	if mgr.Status().GitRevision != commit2.String()[:12] {
		t.Errorf("expected git revision %s, got %s", commit2.String()[:12], mgr.Status().GitRevision)
	}
}

func TestGitManagerCloneAndPullInMemory(t *testing.T) {
	// Create an upstream git repo on disk to serve as the remote origin, and verify NewGitManager clones & pulls it into memory.
	remoteDir := t.TempDir()
	remoteRepo, err := git.PlainInit(remoteDir, false)
	if err != nil {
		t.Fatalf("PlainInit failed: %v", err)
	}
	botDir := filepath.Join(remoteDir, "chat-bot")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "config.yaml"), []byte("id: chat-bot\nname: Chat Bot\nllm:\n  endpoint: https://api.openai.com/v1\n  model: gpt-4o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "prompt.md"), []byte("Remote v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	rwt, _ := remoteRepo.Worktree()
	_, _ = rwt.Add("chat-bot")
	sig := &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()}
	_, err = rwt.Commit("v1", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatalf("remote commit v1 failed: %v", err)
	}

	mgr := NewGitManager(GitOptions{
		URL: remoteDir,
	}, 0, nil)

	changed, err := mgr.Scan()
	if err != nil {
		t.Fatalf("initial in-memory clone Scan failed: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on initial clone")
	}
	p, ok := mgr.Get("chat-bot")
	if !ok || p.Instructions != "Remote v1" {
		t.Fatalf("unexpected persona after clone: %+v", p)
	}

	// Commit v2 to remote repo and verify Scan() pulls it into the in-memory go-git repo
	if err := os.WriteFile(filepath.Join(botDir, "prompt.md"), []byte("Remote v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = rwt.Add("chat-bot/prompt.md")
	_, err = rwt.Commit("v2", &git.CommitOptions{Author: sig})
	if err != nil {
		t.Fatalf("remote commit v2 failed: %v", err)
	}

	changed, err = mgr.Scan()
	if err != nil {
		t.Fatalf("pull Scan failed: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true after remote commit")
	}
	p2, _ := mgr.Get("chat-bot")
	if p2.Instructions != "Remote v2" {
		t.Errorf("expected Remote v2 after pull, got %q", p2.Instructions)
	}
}

func writeBillyFile(t *testing.T, fsys billy.Filesystem, path, content string) {
	t.Helper()
	if err := fsys.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	f, err := fsys.Create(path)
	if err != nil {
		t.Fatalf("Create(%s): %v", path, err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		f.Close()
		t.Fatalf("Write(%s): %v", path, err)
	}
	f.Close()
}
