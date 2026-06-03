package sqlguard

import (
	"testing"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

func testConnectionConfig() config.ConnectionConfig {
	return config.ConnectionConfig{
		Schemas: []string{"public"},
		MaxRows: 50,
		DMLPolicies: []config.DMLPolicy{{
			Schema:     "public",
			Table:      "users",
			Operations: []string{"insert", "update"},
		}},
	}
}

func TestValidateReadOnlySelect(t *testing.T) {
	result := Validate("select id, email from public.users", testConnectionConfig(), ModeRead)
	if !result.Valid || !result.ReadOnly {
		t.Fatalf("expected valid read-only select: %#v", result)
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected missing LIMIT warning")
	}
}

func TestValidateBlocksDisallowedSchema(t *testing.T) {
	result := Validate("select * from private.users limit 10", testConnectionConfig(), ModeRead)
	if result.Valid {
		t.Fatalf("expected schema block: %#v", result)
	}
}

func TestValidateBlocksDDL(t *testing.T) {
	for _, sql := range []string{
		"drop table public.users",
		"alter table public.users add column name text",
		"truncate table public.users",
		"create table public.users(id int)",
	} {
		result := Validate(sql, testConnectionConfig(), ModeRead)
		if result.Valid {
			t.Fatalf("expected DDL to be blocked for %q", sql)
		}
	}
}

func TestValidateDMLPolicy(t *testing.T) {
	allowed := Validate("update public.users set email = 'x' where id = 1", testConnectionConfig(), ModeDML)
	if !allowed.Valid || allowed.Operation != "update" {
		t.Fatalf("expected update to be allowed: %#v", allowed)
	}
	blocked := Validate("delete from public.users where id = 1", testConnectionConfig(), ModeDML)
	if blocked.Valid {
		t.Fatalf("expected delete to be denied: %#v", blocked)
	}
}

func TestValidateMultipleStatements(t *testing.T) {
	result := Validate("select * from public.users; select * from public.accounts", testConnectionConfig(), ModeRead)
	if result.Valid {
		t.Fatalf("expected multiple statements to be denied: %#v", result)
	}
}
