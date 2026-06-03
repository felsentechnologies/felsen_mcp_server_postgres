package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/audit"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/authn"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/mcpserver"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/postgres"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to YAML or JSON configuration")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		fatal("load config", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	authManager, err := authn.NewManager(cfg.Auth)
	if err != nil {
		fatal("initialize auth", err)
	}

	auditor, err := audit.New(cfg.Audit, logger)
	if err != nil {
		fatal("initialize audit", err)
	}
	defer auditor.Close()

	store, err := postgres.NewStore(ctx, cfg)
	if err != nil {
		fatal("connect postgres", err)
	}
	defer store.Close()

	mcpHandler := mcpserver.New(cfg, store, authManager, auditor, logger)
	mux := http.NewServeMux()
	endpoint := cfg.Server.Endpoint
	mux.Handle(endpoint, authn.HTTPMiddleware(authManager, mcpHandler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("postgres MCP server listening", "addr", addr, "endpoint", endpoint)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal("listen", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown failed", "error", err)
	}
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	os.Exit(1)
}
