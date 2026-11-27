# syntax=docker/dockerfile:1.7
# ==============================================================================
# Flowsk Multi-Stage Container Image (module github.com/mindmain/flowsk)
# ==============================================================================
# Builds the unified `flowsk` binary (CLI, ConnectRPC API server, and MCP server)
# as a static pure-Go executable (`CGO_ENABLED=0`, backed by `modernc.org/sqlite`
# and `jackc/pgx/v5`) and packages it into a hardened, minimal non-root runtime
# image mounting `/data/flowsk` for shared SQLite persistence and snapshots.
# ==============================================================================

ARG GO_VERSION=1.26
ARG NODE_VERSION=22
ARG ALPINE_VERSION=3.21

# ------------------------------------------------------------------------------
# Stage 1: Web UI Builder
# Compiles the Angular 21 + Tailwind v4 + SignalStore SPA into `dist/` for Go embed.
# ------------------------------------------------------------------------------
FROM node:${NODE_VERSION}-alpine AS ui-builder

RUN corepack enable

WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml web/.postcssrc.json web/angular.json web/tsconfig*.json ./
RUN pnpm install --frozen-lockfile

COPY web/public ./public
COPY web/src ./src
RUN mkdir -p /src/internal/adapter/webui/dist && pnpm run build

# ------------------------------------------------------------------------------
# Stage 2: Go Builder
# Compiles the static Flowsk binary with trimmed filesystem paths and stripped
# debug symbols for minimal attack surface and deterministic builds.
# ------------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder

ARG VERSION=0.2.0
ARG COMMIT=dev
ARG BUILD_DATE=unknown

# Allow Go toolchain auto-selection if go.mod specifies a newer patch release.
ENV GOTOOLCHAIN=auto \
    CGO_ENABLED=0 \
    GOOS=linux

# Install root CA certificates and git (needed only if fetching VCS metadata).
RUN apk add --no-cache ca-certificates tzdata git

WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum* ./
RUN go mod download

# Copy full workspace source (generated stubs, migrations, domain, adapters, cmd)
# and overlay compiled Angular Web UI assets for `//go:embed all:dist`.
COPY . .
COPY --from=ui-builder /src/internal/adapter/webui/dist ./internal/adapter/webui/dist

# Build a static, reproducible binary into /out/flowsk.
RUN go build \
    -trimpath \
    -ldflags="-s -w -X github.com/mindmain/flowsk/internal/version.Version=${VERSION} -X github.com/mindmain/flowsk/internal/version.Commit=${COMMIT} -X github.com/mindmain/flowsk/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/flowsk \
    ./cmd/flowsk

# ------------------------------------------------------------------------------
# Stage 3: Runtime
# Minimal non-root Alpine image pre-configured with `/data/flowsk` volume
# ownership and SQLite out-of-the-box server defaults.
# ------------------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION} AS runtime

LABEL org.opencontainers.image.title="flowsk" \
      org.opencontainers.image.description="Flowsk project/objective/task server, embedded Web UI, Cloudflare Code Mode JS engine, CLI, and MCP server" \
      org.opencontainers.image.source="https://github.com/mindmain/flowsk"

# Install TLS CA bundle and timezone database for outbound HTTPS / timestamptz.
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 10001 -S flowsk && \
    adduser -u 10001 -S -G flowsk -h /data/flowsk -s /sbin/nologin flowsk && \
    mkdir -p /data/flowsk /data/flowsk/snapshots /data/flowsk/locks /data/flowsk/projects /data/flowsk/tasks && \
    chown -R flowsk:flowsk /data/flowsk

# Copy compiled unified binary into /usr/local/bin/flowsk.
COPY --from=builder /out/flowsk /usr/local/bin/flowsk

# Default server configuration:
# - SQLite primary engine and Markdown docs stored directly inside `/data/flowsk` (`FLOWSK_HOME`).
# - Embedded Angular Web UI enabled out-of-the-box on `:8080` (`FLOWSK_UI=true`).
# - Switch seamlessly to PostgreSQL at runtime by setting `FLOWSK_DB_DRIVER=postgres`
#   and `FLOWSK_DB_DSN=postgres://...` (or `FLOWSK_POSTGRES_DSN`) without rebuilding this image.
ENV HOME="/data/flowsk" \
    FLOWSK_HOME="/data/flowsk" \
    FLOWSK_LISTEN=":8080" \
    FLOWSK_HTTP_ADDR=":8080" \
    FLOWSK_UI="true" \
    FLOWSK_DB_DRIVER="sqlite" \
    FLOWSK_DB_DSN="/data/flowsk/flowsk.db" \
    FLOWSK_SQLITE_PATH="/data/flowsk/flowsk.db" \
    FLOWSK_VOLUME_DIR="/data/flowsk" \
    FLOWSK_AUTO_MIGRATE="true" \
    FLOWSK_LOG_LEVEL="info" \
    FLOWSK_LOG_FORMAT="json"

# Declare shared persistent volume mount point (inherits 10001:10001 ownership).
VOLUME ["/data/flowsk"]

WORKDIR /data/flowsk
USER flowsk:flowsk

# ConnectRPC HTTP/1.1 + h2c listener port.
EXPOSE 8080

# Unified binary entrypoint; defaults to running the ConnectRPC API server (`serve`).
# Override args with e.g. `mcp serve` or CLI subcommands for ephemeral invocations.
ENTRYPOINT ["/usr/local/bin/flowsk"]
CMD ["serve"]
