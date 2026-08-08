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
	upsert := Validate("insert into public.users (id, email) values (1, 'x') on conflict (id) do update set email = excluded.email", testConnectionConfig(), ModeDML)
	if !upsert.Valid {
		t.Fatalf("expected upsert to be allowed when insert and update are both allowlisted: %#v", upsert)
	}
	nothing := Validate("insert into public.users (id, email) values (1, 'x') on conflict (id) do nothing", testConnectionConfig(), ModeDML)
	if !nothing.Valid {
		t.Fatalf("expected ON CONFLICT DO NOTHING to be allowed: %#v", nothing)
	}
	upsertWithoutUpdate := testConnectionConfig()
	upsertWithoutUpdate.DMLPolicies[0].Operations = []string{"insert"}
	blockedUpsert := Validate("insert into public.users (id, email) values (1, 'x') on conflict (id) do update set email = excluded.email", upsertWithoutUpdate, ModeDML)
	if blockedUpsert.Valid {
		t.Fatalf("expected ON CONFLICT DO UPDATE to require an update policy: %#v", blockedUpsert)
	}
	deleteCfg := testConnectionConfig()
	deleteCfg.DMLPolicies[0].Operations = append(deleteCfg.DMLPolicies[0].Operations, "delete")
	deleteUsing := Validate("delete from public.users using private.accounts where public.users.id = private.accounts.id", deleteCfg, ModeDML)
	if deleteUsing.Valid {
		t.Fatalf("expected DELETE USING to enforce every referenced schema: %#v", deleteUsing)
	}
}

func TestValidateCommonDatabaseManagementDDL(t *testing.T) {
	cfg := testConnectionConfig()
	ddlEnabled := true
	cfg.DDLEnabled = &ddlEnabled
	cfg.Schemas = []string{"public"}

	for _, sql := range []string{
		"create index users_email_idx on public.users (email)",
		"create unique index if not exists users_email_idx on public.users using btree (email)",
		"create index on only public.users (email)",
		"create table public.accounts (id bigint primary key)",
		"create table public.orders (id bigint primary key, account_id bigint references public.accounts(id) on delete cascade on update cascade)",
		"alter table public.orders add constraint orders_account_fk foreign key (account_id) references public.accounts(id) on delete cascade",
		"alter table only public.orders add constraint orders_account_fk_2 foreign key (account_id) references public.accounts(id)",
	} {
		result := Validate(sql, cfg, ModeDDL)
		if !result.Valid {
			t.Fatalf("expected common database DDL to be allowed for %q: %#v", sql, result)
		}
	}
}

func TestValidateDDLStillHonorsDisabledFlag(t *testing.T) {
	cfg := testConnectionConfig()
	ddlEnabled := false
	cfg.DDLEnabled = &ddlEnabled
	result := Validate("create index users_email_idx on public.users (email)", cfg, ModeDDL)
	if result.Valid || result.Reason != "DDL is not enabled for this connection" {
		t.Fatalf("expected disabled DDL to remain blocked: %#v", result)
	}
}

func TestValidateMultipleStatements(t *testing.T) {
	result := Validate("select * from public.users; select * from public.accounts", testConnectionConfig(), ModeRead)
	if result.Valid {
		t.Fatalf("expected multiple statements to be denied: %#v", result)
	}
}

func TestValidateRejectsParserBypasses(t *testing.T) {
	for _, sql := range []string{
		"select * from public.users -- from private.users",
		"select * from public.users /* from private.users */",
		"select * from public.users for update",
		"select * from public.users, private.accounts",
		"select * from public.users where id in (select id from private.accounts)",
		"delete from public.users using private.accounts where public.users.id = private.accounts.id",
		"select private.get_secret()",
		"select pg_read_file('/etc/passwd')",
		"select * from public.users; delete from public.users",
	} {
		result := Validate(sql, testConnectionConfig(), ModeRead)
		if result.Valid {
			t.Fatalf("expected unsafe SQL to be denied for %q: %#v", sql, result)
		}
	}
	if result := Validate("select pg_catalog.now()", testConnectionConfig(), ModeRead); !result.Valid {
		t.Fatalf("pg_catalog built-in functions should remain usable: %#v", result)
	}
}

func TestValidateRejectsUnallowlistedDDL(t *testing.T) {
	cfg := testConnectionConfig()
	ddlEnabled := true
	cfg.DDLEnabled = &ddlEnabled
	for _, sql := range []string{
		"create schema public",
		"create table public.copy (like private.users)",
		"create table public.copy as table private.users",
		"create table public.copy inherits (private.users)",
		"create table public.copy partition of private.users",
		"drop table public.users cascade",
		"alter table public.users set schema private",
		"truncate table public.users, public.accounts",
	} {
		result := Validate(sql, cfg, ModeDDL)
		if result.Valid {
			t.Fatalf("expected DDL to be denied for %q: %#v", sql, result)
		}
	}
}

func TestApplySelectLimitAlwaysCapsExistingLimit(t *testing.T) {
	sql := ApplySelectLimit("select * from public.users limit 100000", 50)
	if sql == "select * from public.users limit 100000" {
		t.Fatal("existing LIMIT must still be capped by the configured limit")
	}
	if !HasLimit(sql) {
		t.Fatalf("wrapped query must retain a LIMIT: %q", sql)
	}
}
