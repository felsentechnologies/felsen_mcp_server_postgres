package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadYAMLConfig(t *testing.T) {
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
}

func TestLoadUsesExampleConfigByDefault(t *testing.T) {
	t.Setenv("POSTGRES_MCP_CONFIG", "")
	t.Setenv("CRM_DATABASE_URL", "postgres://user:pass@localhost:5432/crm")

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
	if _, ok := cfg.Connections["crm"]; !ok {
		t.Fatalf("expected default config to load crm connection: %#v", cfg.Connections)
	}
}
