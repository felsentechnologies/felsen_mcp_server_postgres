# Security notes

## Trust boundaries

The MCP server is an authenticated gateway to PostgreSQL. API-key scopes and connection/schema policies are application controls; PostgreSQL roles remain the authoritative boundary and must be configured with least privilege.

Use a dedicated read-only PostgreSQL role for read tokens and separate credentials for any write-enabled deployment. Do not grant the application role superuser, database-owner, file-access, replication, or unrestricted schema privileges.

## Production checklist

- Publish the MCP endpoint behind TLS at a stable HTTPS origin.
- Use PostgreSQL TLS verification in production; sslmode=disable is for local Docker-only development.
- Set server.public_base_url to that public origin and inject a unique MCP_CITATION_SIGNING_KEY of at least 32 characters.
- Store API keys and DSNs in a secret manager or environment injection mechanism.
- Keep `MCP_API_KEY`, `MCP_WRITER_API_KEY`, `MCP_DDL_API_KEY`, and `MCP_ADMIN_API_KEY` as separate secrets; the latter grants all application scopes and configured connections.
- Remove all placeholder values before deployment; configuration validation fails closed on known placeholders.
- Keep masking enabled and review sensitive-column allowlists.
- Keep DML and DDL disabled unless a named policy and explicit approval workflow exist.
- Set a conservative per-connection max_affected_rows; DML over that threshold is rolled back in its transaction.
- Set require_approval to never only for read-only tools; keep approval enabled for execute_dml and execute_ddl.
- Restrict schemas and connections explicitly; avoid wildcard schema access in production.
- If using the environment-managed admin principal, review its all-connection allowlist and keep DML/DDL policies explicit; `admin` scopes do not disable SQL safety controls.
- Monitor readiness, audit output, authentication failures, query latency, and database pool saturation.
- Run the CI race detector and container build before publishing an image.
- Treat `VERSION` changes as releases: the `main` branch workflow creates the matching immutable `vX.Y.Z` tag and refuses to move an existing tag.

## Citation URLs

search and fetch return absolute URLs under /sources. When citation signing is configured, each URL contains an HMAC signature and a 15-minute expiration. The signed URL grants access only to the exact connection/schema/table/column encoded in it; it does not expose the signing key and does not make the MCP endpoint anonymous. Bearer authentication remains supported for direct source access. Keep the signing key in a secret manager and rotate it by restarting the service; rotation invalidates previously issued links.

## Reporting

Do not include credentials, DSNs, query results, or production schema details in issue reports. Redact logs and use the repository's private security process when available.
