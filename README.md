# Postgres MCP Server

Generic Postgres MCP server written in Go. It exposes schema discovery, table descriptions, masked samples, read-only SQL validation/execution, EXPLAIN, and explicitly allowlisted write tools over MCP Streamable HTTP.

## Quick start

Use a real random token even for local development:

```powershell
$env:POSTGRES_MCP_CONFIG = "configs/example.yaml"
$env:DATABASE_URL = "postgres://user:password@localhost:5432/postgres?sslmode=disable"
$bytes = [byte[]]::new(32)
[Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
$env:MCP_API_KEY = [Convert]::ToHexString($bytes)
$citationBytes = [byte[]]::new(32)
[Security.Cryptography.RandomNumberGenerator]::Fill($citationBytes)
$env:MCP_CITATION_SIGNING_KEY = [Convert]::ToBase64String($citationBytes)
go run ./cmd/postgres-mcp
```

The default local endpoint is:

```text
http://127.0.0.1:8080/mcp
```

Every MCP request must send Authorization: Bearer <MCP_API_KEY>. The reader token remains read-only; the example configuration enables DDL and wildcard DML for the configured schemas so a write-scoped principal can manage tables, indexes, and rows. Set `MCP_DDL_ENABLED=false` or `MCP_DML_ENABLED=false` to disable either capability. Do not put tokens directly in YAML or commit real credentials.

`server.public_base_url` is required because search and fetch return absolute, user-openable citation URLs. The local Docker stack defaults it to `http://localhost:8080`; for a published deployment, set `MCP_PUBLIC_BASE_URL` to the public HTTPS origin. The example signs source URLs with `MCP_CITATION_SIGNING_KEY` for 15 minutes; keep that key secret.

## MCP/OpenAI integration

Expose a stable HTTPS MCP endpoint, preserve bearer authentication, and use the official OpenAI MCP integration fields server_url, allowed_tools, and require_approval. A safe read-only configuration starts with:

```json
{
  "type": "mcp",
  "server_label": "postgres",
  "server_url": "https://mcp.example.com/mcp",
  "allowed_tools": ["search", "fetch", "list_schemas", "describe_table"],
  "require_approval": "never"
}
```

Keep require_approval enabled for execute_dml and execute_ddl. See the [OpenAI MCP API documentation](https://developers.openai.com/api/docs/mcp) and [MCP server guidance for plugins](https://developers.openai.com/plugins/build/mcp-server).

### ChatGPT connector OAuth

The server can expose an embedded OAuth 2.1 provider for the ChatGPT connector. Enable it only with a public HTTPS origin:

```text
MCP_OAUTH_ENABLED=true
MCP_OAUTH_ISSUER=https://mcp.example.com
MCP_OAUTH_RESOURCE=https://mcp.example.com
MCP_OAUTH_SIGNING_KEY=<long-random-secret-at-least-32-characters>
MCP_OAUTH_USERNAME=<connector-login>
MCP_OAUTH_PASSWORD=<long-random-password>
MCP_OAUTH_PRINCIPAL=admin
MCP_OAUTH_CALLBACK_URL=https://chatgpt.com/connector/oauth/<callback-id>
MCP_OAUTH_DEFAULT_SCOPES=read,write,ddl,admin
MCP_OAUTH_BASE_SCOPES=read,write,ddl,admin
```

Set `MCP_ADMIN_API_KEY` to create the full-access OAuth principal. Use
`MCP_OAUTH_PRINCIPAL=reader` with `read` scopes for a read-only connector.

The protected-resource metadata, authorization-server metadata, DCR, authorization, and token endpoints are respectively exposed at:

```text
/.well-known/oauth-protected-resource
/.well-known/oauth-authorization-server
/oauth/register
/oauth/authorize
/oauth/token
```

In ChatGPT's advanced authentication settings, use Dynamic Client Registration after the registration endpoint is discovered. If using User-Defined OAuth Client, use client ID `felsen-chatgpt`, leave the client secret empty, select token endpoint auth method `none`, and use the configured default/base scopes. Set `MCP_OAUTH_CALLBACK_URL` in `.env` to the exact callback URL shown by ChatGPT, then restart the server. This value is required when OAuth is enabled, and the static client compares `redirect_uri` exactly against it; no callback domain or path is hardcoded. Dynamic registrations retain the exact callback URL submitted by ChatGPT. OIDC remains intentionally disabled because this bootstrap provider does not claim an email identity.

`MCP_OAUTH_PRINCIPAL` is the name of an entry under `auth.api_keys`, not a scope. The shipped Docker configuration has a read-only `reader` principal and adds `writer`, `ddl`, or `admin` when the corresponding environment token is set. For full OAuth scope access, set `MCP_ADMIN_API_KEY`, `MCP_OAUTH_PRINCIPAL=admin`, and request `read,write,ddl,admin`. The `admin` scope does not bypass the SQL safety guard, DML policies, row limits, or schema restrictions.

Mutation defaults are controlled globally by `MCP_DDL_ENABLED` and
`MCP_DML_ENABLED`, both defaulting to `true` in the shipped Docker and local
configurations. DML uses the default policy `{schema: "*", table: "*", operations:
[insert, update, delete]}` within the configured schemas. Each connection still
enforces `max_affected_rows` (100 by default). Add narrower `dml_policies` under
`connections.<name>` in the selected YAML file, or set the flags to `false` for
a restricted deployment.

The MCP endpoint still returns a standards-compliant `401` with `resource_metadata` when no bearer is present. ChatGPT uses that challenge to discover OAuth and then calls `initialize`/`tools/list` with the access token; `tools/list` is not made public because exposing discovery must not bypass database authorization. Existing API-key bearer clients continue to work.

The embedded login is a bootstrap identity provider, not a replacement for an enterprise IdP. For production deployments with multiple users, prefer placing an external OAuth/OIDC provider in front of the MCP server and map its claims to scoped principals.

`search` follows the standard single-argument contract `{query}` and searches every connection authorized for the bearer token. It returns `{results:[{id,title,url}]}`; `fetch` accepts only `{id}` and returns `{id,title,text,url,metadata}`. IDs are opaque `pg:v1:` connection-scoped identifiers generated by the server—pass them back unchanged instead of constructing `schema.table` IDs. This keeps identical objects in different connections unambiguous. URLs point to a short-lived signed `/sources` endpoint. Bearer authentication remains accepted for direct source access.

## Versioning and releases

The repository root `VERSION` file is the SemVer source of truth. It is advertised through the MCP implementation metadata, the `version`/`--version` CLI commands, and Docker build metadata. On `main`, a push that changes `VERSION` runs the release workflow, validates `X.Y.Z`, and creates the matching annotated tag `vX.Y.Z` without overwriting an existing tag. The Windows development script also exposes local tag creation as menu option 19.

## Tools

- list_connections
- list_schemas
- list_tables
- describe_table
- sample_rows
- validate_sql
- execute_sql
- execute_dml
- execute_ddl
- explain_sql
- refresh_schema_cache
- search
- fetch

## Resources

- postgres://connections
- postgres://{connection}/schema
- postgres://{connection}/schemas
- postgres://{connection}/tables
- postgres://{connection}/table/{schema}/{table}

## Safety and operational defaults

- Bearer authentication is required for MCP and SSE; source URLs use short-lived signatures or Bearer authentication, and bare tokens are rejected. When OAuth is enabled, the 401 challenge advertises the protected-resource metadata URL.
- API keys must declare read, write, ddl, or admin scopes and allowed connections.
- YAML/JSON unknown fields fail configuration loading.
- The SQL guard is conservative and denies comments, dollar-quoted strings, multiple statements/relation lists, row-locking clauses, dangerous functions, CTEs, and non-allowlisted DDL. It supports common schema-management forms including INSERT ... ON CONFLICT, CREATE INDEX, table definitions with FOREIGN KEY ... REFERENCES, and foreign-key ON DELETE/UPDATE actions.
- Read SQL runs in a PostgreSQL READ ONLY transaction.
- SQL result rows are capped server-side, including queries that already contain a larger LIMIT.
- DML is enabled by the shipped wildcard policy for configured schemas and rolls back when `max_affected_rows` (100 by default) is exceeded; DDL is enabled by default in the shipped configurations, supports common table/index/constraint operations, rejects destructive CASCADE and multi-object operations, and permits CASCADE only as a foreign-key referential action.
- Results are masked using sensitive-column patterns unless explicitly allowed by connection configuration.
- HTTP body size, request concurrency, read/write/idle timeouts, readiness checks, and audit write errors are enforced.

## Docker

Copy .env.example to .env, replace every replace-with-* value, and set MCP_PUBLIC_BASE_URL. Compose fails closed when database credentials, the reader token, or the citation signing key are missing. The Docker configuration enables mutation tools for write-scoped principals, remains restricted to the public schema by default, and keeps masking enabled.

```powershell
Copy-Item .env.example .env
docker compose --file Docker/docker-compose.yaml up --build
```

The container image is distroless and includes a native healthcheck command. The Swarm stack does not publish the Postgres port externally.

## Verification

```powershell
$env:CGO_ENABLED = "0"
go test ./...
go vet ./...
go mod verify
go build -trimpath ./cmd/postgres-mcp
```

CI also runs the race detector, formatting checks, and a Docker build. When using CGO_ENABLED=0, the project uses the conservative non-CGO statement lexer; CI validates the CGO parser-backed build as well.
