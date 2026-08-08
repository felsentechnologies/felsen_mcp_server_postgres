package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLConfig(t *testing.T) {
	t.Setenv("MCP_PUBLIC_BASE_URL", "")
	t.Setenv("MCP_OAUTH_ENABLED", "false")
	t.Setenv("CRM_DSN", "postgres://user:pass@localhost:5432/crm")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  port: 9000
auth:
  api_keys:
    - name: reader
      token_env: TEST_MCP_TOKEN
      scopes: [read]
      connections: [crm]
connections:
  crm:
    dsn_env: CRM_DSN
    schemas: [public, billing]
    dml_policies:
      - schema: public
        table: customers
        operations: [insert, update]
`)
	t.Setenv("TEST_MCP_TOKEN", "test-token")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Endpoint != defaultEndpoint || cfg.Server.Port != 9000 {
		t.Fatalf("unexpected server defaults: %#v", cfg.Server)
	}
	if got := cfg.Connections["crm"].DSN; got != "postgres://user:pass@localhost:5432/crm" {
		t.Fatalf("dsn env not resolved: %q", got)
	}
	if got := cfg.Auth.APIKeys[0].Token; got != "test-token" {
		t.Fatalf("token env not resolved: %q", got)
	}
	if !cfg.Connections["crm"].DMLAllowed("public", "customers", "update") {
		t.Fatal("expected DML policy to allow public.customers update")
	}
	if got := cfg.Connections["crm"].MaxAffectedRows; got != defaultMaxAffected {
		t.Fatalf("unexpected default max_affected_rows: %d", got)
	}
}

func TestPublicBaseURLEnvironmentOverridesFile(t *testing.T) {
	t.Setenv("MCP_OAUTH_ENABLED", "false")
	t.Setenv("MCP_PUBLIC_BASE_URL", "https://public.example.com")
	t.Setenv("TEST_MCP_TOKEN", "test-token")
	t.Setenv("CRM_DSN", "postgres://user:pass@localhost:5432/crm")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  public_base_url: http://localhost:9000
auth:
  api_keys:
    - name: reader
      token_env: TEST_MCP_TOKEN
      scopes: [read]
      connections: [crm]
connections:
  crm:
    dsn_env: CRM_DSN
    schemas: [public]
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Server.PublicBaseURL; got != "https://public.example.com" {
		t.Fatalf("environment public base URL did not override file value: %q", got)
	}
}

func TestDockerConfigHasLocalPublicBaseFallback(t *testing.T) {
	t.Setenv("MCP_PUBLIC_BASE_URL", "")
	t.Setenv("MCP_OAUTH_ENABLED", "false")
	t.Setenv("DATABASE_URL", "postgres://user:pass@postgres:5432/mcp")
	t.Setenv("MCP_API_KEY", "docker-config-test-reader-token")
	t.Setenv("MCP_CITATION_SIGNING_KEY", "docker-config-test-citation-key-with-32-chars")
	path := filepath.Join("..", "..", "Docker", "config.docker.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Server.PublicBaseURL; got != "http://localhost:8080" {
		t.Fatalf("unexpected Docker local public base URL: %q", got)
	}
}

func TestLoadUsesExampleConfigByDefault(t *testing.T) {
	t.Setenv("POSTGRES_MCP_CONFIG", "")
	t.Setenv("MCP_OAUTH_ENABLED", "false")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/postgres")
	t.Setenv("MCP_API_KEY", "example-test-token")
	t.Setenv("MCP_CITATION_SIGNING_KEY", "example-test-citation-signing-key-with-32-chars")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	if err := os.Chdir(projectRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatal(err)
		}
	})

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Connections["default"]; !ok {
		t.Fatalf("expected default config to load default connection: %#v", cfg.Connections)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	t.Setenv("MCP_OAUTH_ENABLED", "false")
	t.Setenv("TEST_MCP_TOKEN", "test-token")
	t.Setenv("CRM_DSN", "postgres://user:pass@localhost:5432/crm")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  public_base_url: http://localhost:9000
  unknown_field: true
auth:
  api_keys:
    - name: reader
      token_env: TEST_MCP_TOKEN
      scopes: [read]
      connections: [crm]
connections:
  crm:
    dsn_env: CRM_DSN
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown configuration field to fail")
	}
}

func TestValidateRejectsPlaceholderToken(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{
			Host: "127.0.0.1", Port: 8080, Endpoint: "/mcp", PublicBaseURL: "http://localhost:8080",
			CitationTTL: "1m", MaxRows: 10, MaxSearchResults: 10, MaxBodyBytes: 1024, MaxConcurrent: 1,
			QueryTimeout: "1s", SessionTimeout: "1m", ReadHeaderTimeout: "1s", ReadTimeout: "1s", WriteTimeout: "1s", IdleTimeout: "1s",
		},
		Auth: AuthConfig{APIKeys: []APIKeyConfig{{
			Name: "reader", Token: "change-me-reader", Scopes: []string{"read"}, Connections: []string{"default"},
		}}},
		Connections: map[string]ConnectionConfig{"default": {
			DSN: "postgres://user:pass@localhost:5432/db", Schemas: []string{"public"},
			MaxRows: 10, MaxAffectedRows: 10, QueryTimeout: "1s",
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected placeholder token to fail validation")
	}
}

func TestLoadRejectsMissingCitationSigningKey(t *testing.T) {
	t.Setenv("MCP_OAUTH_ENABLED", "false")
	t.Setenv("TEST_MCP_TOKEN", "test-token")
	t.Setenv("CRM_DSN", "postgres://user:pass@localhost:5432/crm")
	t.Setenv("TEST_CITATION_KEY", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  public_base_url: https://mcp.example.com
  citation_signing_key_env: TEST_CITATION_KEY
auth:
  api_keys:
    - name: reader
      token_env: TEST_MCP_TOKEN
      scopes: [read]
      connections: [crm]
connections:
  crm:
    dsn_env: CRM_DSN
    schemas: [public]
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected missing citation signing key to fail validation")
	}
}

func TestLoadBuildsIPv6PublicBaseURL(t *testing.T) {
	t.Setenv("MCP_OAUTH_ENABLED", "false")
	t.Setenv("TEST_MCP_TOKEN", "test-token")
	t.Setenv("CRM_DSN", "postgres://user:pass@localhost:5432/crm")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  host: "::1"
auth:
  api_keys:
    - name: reader
      token_env: TEST_MCP_TOKEN
      scopes: [read]
      connections: [crm]
connections:
  crm:
    dsn_env: CRM_DSN
    schemas: [public]
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Server.PublicBaseURL; got != "http://[::1]:8080" {
		t.Fatalf("unexpected IPv6 public base URL: %q", got)
	}
}

func TestLoadOAuthConfigurationFromEnvironment(t *testing.T) {
	t.Setenv("MCP_PUBLIC_BASE_URL", "https://mcp.example.com")
	t.Setenv("MCP_OAUTH_ENABLED", "true")
	t.Setenv("MCP_OAUTH_SIGNING_KEY", "oauth-signing-key-with-at-least-32-characters")
	t.Setenv("MCP_OAUTH_USERNAME", "chatgpt")
	t.Setenv("MCP_OAUTH_PASSWORD", "strong-oauth-password-123")
	t.Setenv("MCP_OAUTH_PRINCIPAL", "reader")
	t.Setenv("MCP_OAUTH_DEFAULT_SCOPES", "read")
	t.Setenv("MCP_OAUTH_BASE_SCOPES", "read")
	t.Setenv("TEST_MCP_TOKEN", "test-token")
	t.Setenv("CRM_DSN", "postgres://user:pass@localhost:5432/crm")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
server:
  public_base_url: https://mcp.example.com
auth:
  api_keys:
    - name: reader
      token_env: TEST_MCP_TOKEN
      scopes: [read]
      connections: [crm]
connections:
  crm:
    dsn_env: CRM_DSN
    schemas: [public]
oauth:
  enabled: false
  signing_key_env: MCP_OAUTH_SIGNING_KEY
  username_env: MCP_OAUTH_USERNAME
  password_env: MCP_OAUTH_PASSWORD
  principal: reader
  client_id: felsen-chatgpt
  default_scopes: [read]
  base_scopes: [read]
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OAuth.Enabled || cfg.OAuth.Issuer != "https://mcp.example.com" || cfg.OAuth.Resource != "https://mcp.example.com" {
		t.Fatalf("OAuth environment/defaults were not applied: %#v", cfg.OAuth)
	}
	if cfg.OAuth.SigningKey == "" || cfg.OAuth.Username != "chatgpt" || cfg.OAuth.Password == "" {
		t.Fatalf("OAuth secret environment values were not resolved: %#v", cfg.OAuth)
	}
}

func TestValidateRejectsOAuthScopeOutsidePrincipal(t *testing.T) {
	cfg := Config{
		Server: ServerConfig{
			Host: "127.0.0.1", Port: 8080, Endpoint: "/mcp", PublicBaseURL: "https://mcp.example.com",
			CitationTTL: "1m", CitationSigningKey: "citation-key-with-at-least-32-characters", MaxRows: 10, MaxSearchResults: 10, MaxBodyBytes: 1024, MaxConcurrent: 1,
			QueryTimeout: "1s", SessionTimeout: "1m", ReadHeaderTimeout: "1s", ReadTimeout: "1s", WriteTimeout: "1s", IdleTimeout: "1s",
		},
		Auth: AuthConfig{APIKeys: []APIKeyConfig{{
			Name: "reader", Token: "api-token", Scopes: []string{"read"}, Connections: []string{"default"},
		}}},
		OAuth: OAuthConfig{
			Enabled: true, Issuer: "https://mcp.example.com", Resource: "https://mcp.example.com",
			SigningKey: "oauth-signing-key-with-at-least-32-characters", Username: "chatgpt", Password: "strong-password",
			Principal: "reader", ClientID: "felsen-chatgpt", DefaultScopes: []string{"write"}, BaseScopes: []string{"read"},
			AccessTokenTTL: "1h", RefreshTokenTTL: "24h", AuthorizationCodeTTL: "5m",
		},
		Connections: map[string]ConnectionConfig{"default": {
			DSN: "postgres://user:pass@localhost:5432/db", Schemas: []string{"public"}, MaxRows: 10, MaxAffectedRows: 10, QueryTimeout: "1s",
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected OAuth scope outside the principal to fail validation")
	}
}
