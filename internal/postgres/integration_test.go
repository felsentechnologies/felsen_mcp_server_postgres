package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

func TestExecuteSQLEnforcesServerRowCap(t *testing.T) {
	dsn := os.Getenv("POSTGRES_MCP_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_MCP_TEST_DSN is not configured")
	}
	cfg := &config.Config{
		Connections: map[string]config.ConnectionConfig{
			"default": {
				DSN:          dsn,
				Schemas:      []string{"public"},
				MaxRows:      3,
				MaxConns:     1,
				QueryTimeout: "5s",
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := NewStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	result, validation, err := store.ExecuteSQL(ctx, "default", "select generate_series(1, 10)")
	if err != nil {
		t.Fatal(err)
	}
	if !validation.Valid || result.RowCount != 3 {
		t.Fatalf("expected a valid query capped at 3 rows, validation=%#v result=%#v", validation, result)
	}
}
