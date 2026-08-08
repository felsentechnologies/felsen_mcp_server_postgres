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

The image reads the SemVer from the repository `VERSION` file. The Windows
script passes the current version, commit, and UTC build time as Docker build
arguments so the running MCP server reports reproducible build metadata.

This stack does not create a Postgres database or initialize schemas. The MCP server connects to the Postgres DSN you provide and introspects only the public schema by default. The Docker configuration grants read-only access, keeps masking enabled, and fails closed when credentials are missing.

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

For local Docker development, `MCP_PUBLIC_BASE_URL` defaults to
`http://localhost:8080`. Set it explicitly for any published deployment.
Set all credential values with real secrets:

```text
POSTGRES_DB=mcp
POSTGRES_USER=postgres
POSTGRES_PASSWORD=replace-with-a-real-password
DATABASE_URL=postgres://user:password@postgres-host:5432/postgres?sslmode=disable
MCP_API_KEY=your-long-random-reader-token
MCP_CITATION_SIGNING_KEY=your-long-random-key-at-least-32-characters
MCP_PUBLIC_BASE_URL=https://mcp.example.com
HTTP_PORT=8080
IMAGE_NAME=mcp-postgres
IMAGE_TAG=v0.3.0
```

For a versioned Swarm deployment, publish the image as `IMAGE_NAME:IMAGE_TAG`
and set the same `IMAGE_TAG` in the stack variables. `latest` is convenient for
local development; release tags should use the immutable `vX.Y.Z` tag created
by the GitHub workflow.

Do not copy the local `POSTGRES_MCP_CONFIG=configs/example.yaml` value into
Portainer. Leave `POSTGRES_MCP_CONFIG` unset, or set it to:

```text
/app/Docker/config.docker.yaml
```

The Docker config binds the server to `0.0.0.0`, which is required for the
published Portainer/Docker port to reach the service.

Do not publish the Postgres port in production. The Swarm file keeps it on the internal overlay network and requires POSTGRES_DB, POSTGRES_USER, POSTGRES_PASSWORD, MCP_API_KEY, MCP_CITATION_SIGNING_KEY, and MCP_PUBLIC_BASE_URL. Citation URLs are HMAC-signed and expire after 15 minutes.

## Stop Stack

Use option `4` in `scrips_dev\windows.ps1`.

Use option `16` to stop the stack and remove volumes.
