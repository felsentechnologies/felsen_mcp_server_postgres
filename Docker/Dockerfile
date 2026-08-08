# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION_OVERRIDE=""
ARG BUILD_COMMIT="unknown"
ARG BUILD_DATE="unknown"

ENV CGO_ENABLED=0
RUN APP_VERSION="${VERSION_OVERRIDE:-$(tr -d '\r\n' < VERSION)}" && \
    go build -trimpath -ldflags="-s -w -X github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/version.Version=${APP_VERSION} -X github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/version.Commit=${BUILD_COMMIT} -X github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/version.Date=${BUILD_DATE}" -o /out/postgres-mcp ./cmd/postgres-mcp

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/postgres-mcp /app/postgres-mcp
COPY configs /app/configs
COPY Docker/config.docker.yaml /app/Docker/config.docker.yaml

ENV POSTGRES_MCP_CONFIG=/app/Docker/config.docker.yaml

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/app/postgres-mcp", "healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/app/postgres-mcp"]
