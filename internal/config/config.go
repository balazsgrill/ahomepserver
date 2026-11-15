package config

import (
	"bufio"
	"flag"
	"os"
	"strings"
	"sync"
	"time"
)

// Config holds the server configuration.
type Config struct {
	ListenAddr          string
	DatabaseURL         string
	PersonasDir         string
	PersonasGitURL      string
	PersonasGitBranch   string
	PersonasGitSubdir   string
	PersonasGitTokenKey string
	ScanInterval        time.Duration
	TokensFile          string
	APIKey              string
	MaxAgentTurn        int
}

// Load parses command-line flags and environment variables into Config.
func Load() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.ListenAddr, "listen", getEnv("AHP_LISTEN_ADDR", ":8080"), "HTTP listen address")
	flag.StringVar(&cfg.DatabaseURL, "db", getEnv("AHP_DATABASE_URL", "postgres://postgres:postgres@localhost:5432/ahomepserver?sslmode=disable"), "PostgreSQL connection URL (or memory:// for in-memory store)")
	flag.StringVar(&cfg.PersonasDir, "personas-dir", getEnv("AHP_PERSONAS_DIR", "./personas"), "Directory containing agent persona subfolders (when not using -personas-git-url)")
	flag.StringVar(&cfg.PersonasGitURL, "personas-git-url", getEnv("AHP_PERSONAS_GIT_URL", ""), "Optional Git repository URL to clone and sync into an in-memory go-git repository")
	flag.StringVar(&cfg.PersonasGitBranch, "personas-git-branch", getEnv("AHP_PERSONAS_GIT_BRANCH", ""), "Optional Git branch to track for the in-memory persona repository")
	flag.StringVar(&cfg.PersonasGitSubdir, "personas-git-subdir", getEnv("AHP_PERSONAS_GIT_SUBDIR", ""), "Optional subdirectory inside the Git repository containing persona subfolders")
	flag.StringVar(&cfg.PersonasGitTokenKey, "personas-git-token-key", getEnv("AHP_PERSONAS_GIT_TOKEN_KEY", ""), "Optional token_key for authenticating against the Git repository over HTTPS")
	flag.DurationVar(&cfg.ScanInterval, "scan-interval", getDurationEnv("AHP_SCAN_INTERVAL", 30*time.Second), "Cadence for scanning the personas folder or pulling the in-memory git repo")
	flag.StringVar(&cfg.TokensFile, "tokens-file", getEnv("AHP_TOKENS_FILE", ""), "Optional KEY=VALUE file containing API tokens referenced by token_key")
	flag.StringVar(&cfg.APIKey, "api-key", getEnv("AHP_API_KEY", ""), "Optional API key for authenticating HTTP requests")
	flag.IntVar(&cfg.MaxAgentTurn, "max-turns", getIntEnv("AHP_MAX_AGENT_TURNS", 10), "Maximum tool-calling iterations per agent turn")
	flag.Parse()

	return cfg
}

// TokenResolver resolves logical token keys (from persona configs) into secret values.
type TokenResolver struct {
	mu         sync.RWMutex
	tokensFile string
	fileTokens map[string]string
}

// NewTokenResolver creates a resolver that reads from an optional tokens file and environment variables.
func NewTokenResolver(tokensFile string) *TokenResolver {
	tr := &TokenResolver{
		tokensFile: tokensFile,
		fileTokens: make(map[string]string),
	}
	tr.Reload()
	return tr
}

// Reload refreshes the token map from the tokens file if configured.
func (tr *TokenResolver) Reload() {
	if tr.tokensFile == "" {
		return
	}
	f, err := os.Open(tr.tokensFile)
	if err != nil {
		return
	}
	defer f.Close()

	loaded := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
			loaded[k] = v
		}
	}

	tr.mu.Lock()
	tr.fileTokens = loaded
	tr.mu.Unlock()
}

// Resolve looks up a token key in the tokens file, then AHP_TOKEN_<KEY> env var, then <KEY> env var.
func (tr *TokenResolver) Resolve(tokenKey string) string {
	tokenKey = strings.TrimSpace(tokenKey)
	if tokenKey == "" {
		return ""
	}
	tr.mu.RLock()
	if val, ok := tr.fileTokens[tokenKey]; ok && val != "" {
		tr.mu.RUnlock()
		return val
	}
	tr.mu.RUnlock()

	if val := os.Getenv("AHP_TOKEN_" + tokenKey); val != "" {
		return val
	}
	return os.Getenv(tokenKey)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getDurationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func getIntEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		for _, ch := range v {
			if ch < '0' || ch > '9' {
				return fallback
			}
			n = n*10 + int(ch-'0')
		}
		if n > 0 {
			return n
		}
	}
	return fallback
}

