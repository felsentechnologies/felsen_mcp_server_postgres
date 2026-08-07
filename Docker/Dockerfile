# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/postgres-mcp ./cmd/postgres-mcp

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/postgres-mcp /app/postgres-mcp
COPY configs /app/configs
COPY Docker/config.docker.yaml /app/Docker/config.docker.yaml

ENV POSTGRES_MCP_CONFIG=/app/Docker/config.docker.yaml
ENV MCP_API_KEY=change-me-reader
ENV MCP_WRITER_API_KEY=change-me-writer
ENV MCP_DDL_API_KEY=change-me-ddl

EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/app/postgres-mcp"]
