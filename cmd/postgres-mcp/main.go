package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/audit"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/authn"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/mcpserver"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/oauth"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/postgres"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.BuildString())
		return
	}

	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "config", "", "path to YAML or JSON configuration")
	flag.BoolVar(&showVersion, "version", false, "print the server version and exit")
	flag.Parse()
	if showVersion {
		fmt.Println(version.BuildString())
		return
	}

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
	oauthProvider, err := oauth.New(cfg.OAuth, authManager)
	if err != nil {
		fatal("initialize OAuth", err)
	}
	var authenticator authn.Authenticator = authManager
	challenge := `Bearer realm="postgres-mcp"`
	if oauthProvider != nil {
		authenticator = authn.NewComposite(authManager, oauthProvider)
		challenge = oauthProvider.Challenge()
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

	mcpHandler := mcpserver.New(cfg, store, authenticator, auditor, logger)
	mux := http.NewServeMux()
	endpoint := cfg.Server.Endpoint
	protected := authn.HTTPMiddlewareWithLoggerAndChallenge(authenticator, mcpHandler, logger, challenge)
	concurrency := make(chan struct{}, cfg.Server.MaxConcurrent)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case concurrency <- struct{}{}:
			defer func() { <-concurrency }()
		default:
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, cfg.Server.MaxBodyBytes)
		if r.URL.Path == "/sources" || r.URL.Path == "/sources/" {
			mcpHandler.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	}))
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{`ok`: true, `version`: version.String()})
	}
	readyHandler := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		err := store.Health(ctx)
		status := http.StatusOK
		response := map[string]any{`ok`: err == nil, `version`: version.String()}
		if err != nil {
			status = http.StatusServiceUnavailable
			response[`error`] = `database unavailable`
			logger.Warn(`readiness check failed`, `error`, err)
		}
		w.Header().Set(`Content-Type`, `application/json`)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(response)
	}
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/readyz", readyHandler)
	if oauthProvider != nil {
		for _, path := range oauthProvider.Paths() {
			mux.Handle(path, oauthProvider)
		}
	}

	addr := net.JoinHostPort(cfg.Server.Host, fmt.Sprintf("%d", cfg.Server.Port))
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeoutDuration(),
		ReadTimeout:       cfg.Server.ReadTimeoutDuration(),
		WriteTimeout:      cfg.Server.WriteTimeoutDuration(),
		IdleTimeout:       cfg.Server.IdleTimeoutDuration(),
		MaxHeaderBytes:    1 << 20,
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

func runHealthcheck() int {
	url := os.Getenv("MCP_HEALTHCHECK_URL")
	if url == "" {
		url = "http://127.0.0.1:8080/readyz"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return 1
	}
	return 0
}
