# Postgres MCP Server

Generic Postgres MCP server written in Go. It exposes schema discovery, table descriptions, masked row samples, SQL validation, read-only SELECT execution, EXPLAIN, DML execution and DDL execution over MCP Streamable HTTP.

## Quick Start

```powershell
go mod tidy
$env:DATABASE_URL="postgres://user:password@localhost:5432/postgres?sslmode=disable"
$env:MCP_API_KEY="change-me-reader"
$env:MCP_WRITER_API_KEY="change-me-writer"
$env:MCP_DDL_API_KEY="change-me-ddl"
go run ./cmd/postgres-mcp
```

MCP endpoint:

```text
http://127.0.0.1:8080/mcp
```

Every MCP request must include:

```text
Authorization: Bearer change-me-reader
```

`configs/example.yaml` is the default configuration file when no `-config` or `POSTGRES_MCP_CONFIG` is provided.

For a single local connection without a config file, run from a folder that does not have `configs/example.yaml`, or pass a different config path:

```powershell
$env:DATABASE_URL="postgres://user:password@localhost:5432/postgres?sslmode=disable"
$env:MCP_API_KEY="local-dev-token"
go run ./cmd/postgres-mcp
```

## Tools

- `list_connections`
- `list_schemas`
- `list_tables`
- `describe_table`
- `sample_rows`
- `validate_sql`
- `execute_sql`
- `execute_dml`
- `execute_ddl`
- `explain_sql`
- `refresh_schema_cache`

## Resources

- `postgres://connections`
- `postgres://{connection}/schema`
- `postgres://{connection}/schemas`
- `postgres://{connection}/tables`
- `postgres://{connection}/table/{schema}/{table}`

## Safety Defaults

- Bearer token required for `/mcp`.
- Token scopes are `read`, `write`, `ddl` and `admin`.
- Connections are named and access is restricted per token.
- Schemas are allowlisted per connection.
- `execute_sql` only runs `SELECT` and wraps missing `LIMIT` with the configured `max_rows`.
- `execute_dml` only runs `INSERT`, `UPDATE` or `DELETE` when a matching DML policy exists.
- `execute_ddl` runs `CREATE`, `ALTER`, `DROP` and `TRUNCATE` when `ddl_enabled` is true and token has `ddl` scope.
- Sample/query results are masked using default sensitive column patterns.

## Notes

`pg_query_go` is included for parser-backed validation when cgo is enabled. With `CGO_ENABLED=0`, the server uses a conservative non-cgo statement splitter plus operation/table policy checks so the project still builds cleanly on Windows.
