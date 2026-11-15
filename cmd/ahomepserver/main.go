package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/balazsgrill/ahomepserver/internal/agent"
	"github.com/balazsgrill/ahomepserver/internal/config"
	"github.com/balazsgrill/ahomepserver/internal/persona"
	"github.com/balazsgrill/ahomepserver/internal/server"
	"github.com/balazsgrill/ahomepserver/internal/store"
)

func main() {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tokens := config.NewTokenResolver(cfg.TokensFile)

	// Initialize store (PostgreSQL by default, or memory:// for local testing)
	var st store.Store
	if strings.HasPrefix(cfg.DatabaseURL, "memory://") {
		slog.Info("using in-memory store")
		st = store.NewMemoryStore()
	} else {
		pgStore, err := store.NewPostgresStore(ctx, cfg.DatabaseURL)
		if err != nil {
			slog.Error("failed to initialize PostgreSQL store", "err", err)
			os.Exit(1)
		}
		slog.Info("connected to PostgreSQL store")
		st = pgStore
	}
	defer st.Close()

	// Initialize persona directory or in-memory git manager with periodic rescanning
	var personaMgr *persona.Manager
	if strings.TrimSpace(cfg.PersonasGitURL) != "" {
		personaMgr = persona.NewGitManager(persona.GitOptions{
			URL:    cfg.PersonasGitURL,
			Branch: cfg.PersonasGitBranch,
			SubDir: cfg.PersonasGitSubdir,
			TokenProvider: func() string {
				return tokens.Resolve(cfg.PersonasGitTokenKey)
			},
		}, cfg.ScanInterval, func() {
			tokens.Reload()
		})
	} else {
		personaMgr = persona.NewManager(cfg.PersonasDir, cfg.ScanInterval, func() {
			tokens.Reload()
		})
	}
	if err := personaMgr.Start(ctx); err != nil {
		slog.Error("failed to start persona manager", "err", err)
		os.Exit(1)
	}

	runner := agent.NewRunner(tokens, cfg.MaxAgentTurn)
	srv := server.New(cfg, tokens, personaMgr, st, runner)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("AHP server listening",
			"addr", cfg.ListenAddr,
			"personas_dir", cfg.PersonasDir,
			"scan_interval", cfg.ScanInterval.String(),
		)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down AHP server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

