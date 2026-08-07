# Docker

This folder contains the Docker image and compose stack for the Postgres MCP server.

## Files

| File | Purpose |
|------|---------|
| `Dockerfile` | Multi-stage build with distroless |
| `docker-compose.yaml` | Local development (Docker Desktop) |
| `docker-compose.swarm.yaml` | Docker Swarm / Portainer deploy |
| `config.docker.yaml` | MCP server config for Docker |

## Build Image

```powershell
.\scrips_dev\windows.ps1
```

Use option `1` in the menu.

## Run (Local Development)

```powershell
.\scrips_dev\windows.ps1 -DatabaseUrl "postgres://user:password@host.docker.internal:5432/postgres?sslmode=disable"
```

The MCP endpoint will be available at:

```text
http://localhost:8080/mcp
```

This stack does not create a Postgres database or initialize schemas. The MCP server connects to the Postgres DSN you provide and introspects the schemas allowed in `config.docker.yaml`.

For a database running on your Windows host, use `host.docker.internal` in the DSN.

## Docker Swarm / Portainer

For Swarm deploy, use `docker-compose.swarm.yaml`:

```bash
docker stack deploy -c Docker/docker-compose.swarm.yaml mcp-stack
```

The Swarm file includes:
- `deploy` section with replicas, update/rollback config
- `overlay` network for multi-node communication
- Restart policy on failure

## Portainer Stack Variables

The stack accepts Portainer environment variables because every required value is
referenced under `services.postgres-mcp.environment` in `docker-compose.yaml`.

Set at least:

```text
DATABASE_URL=postgres://user:password@postgres-host:5432/postgres?sslmode=disable
MCP_API_KEY=your-admin-token
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

Use option `4` in `scrips_dev\windows.ps1`.

Use option `15` to stop the stack and remove volumes.
