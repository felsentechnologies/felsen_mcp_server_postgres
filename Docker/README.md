# Docker

This folder contains the Docker image and compose stack for the Postgres MCP server.

## Build Image

```powershell
.\scrips_dev\postgres-mcp-docker-dev.ps1 -ImageName mcp-postgres
```

Use option `1` in the menu.

## Run

```powershell
.\scrips_dev\postgres-mcp-docker-dev.ps1 `
  -DatabaseUrl "postgres://user:password@host.docker.internal:5432/crm?sslmode=disable" `
  -BearerToken "local-dev-token"
```

The MCP endpoint will be available at:

```text
http://localhost:8080/mcp
```

This stack does not create a Postgres database or initialize schemas. The MCP server connects to the Postgres DSN you provide and introspects the schemas allowed in `config.docker.yaml`.

For a database running on your Windows host, use `host.docker.internal` in the DSN.

## Portainer Stack Variables

The stack accepts Portainer environment variables because every required value is
referenced under `services.postgres-mcp.environment` in `docker-compose.yaml`.

Set at least:

```text
CRM_DATABASE_URL=postgres://user:password@postgres-host:5432/crm?sslmode=disable
MCP_API_KEY=reader-token
MCP_WRITER_API_KEY=writer-token
HTTP_PORT=8080
```

Do not copy the local `POSTGRES_MCP_CONFIG=configs/example.yaml` value into
Portainer. Leave `POSTGRES_MCP_CONFIG` unset, or set it to:

```text
/app/Docker/config.docker.yaml
```

The Docker config binds the server to `0.0.0.0`, which is required for the
published Portainer/Docker port to reach the service.

## Stop Stack

Use option `3` in `scrips_dev\postgres-mcp-docker-dev.ps1`.

Use option `12` to stop the stack and remove volumes, if Docker created any anonymous volumes.
