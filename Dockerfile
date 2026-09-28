# syntax=docker/dockerfile:1

# ---------- build ----------
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Tests run in the build so a broken image cannot be produced. This daemon places
# orders; "it compiled" is not a sufficient bar.
RUN CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./...

# CGO_ENABLED=0 is not an optimisation here — the SQLite driver is pure Go
# (modernc.org/sqlite), so the binary is fully static and needs no libc at runtime.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/agent ./cmd/agent

# ---------- runtime ----------
FROM alpine:3.21

# ca-certificates only: required for TLS to Alpaca; without it every API call fails.
#
# tzdata is deliberately NOT installed. Go consults the system zoneinfo database before
# the time/tzdata embed, so installing it here would mask the embed and leave the
# guarantee untested — exactly the situation a review caught. With no system database,
# the embed is the only source, so if someone drops the import the container is
# immediately and visibly wrong rather than silently correct until it is deployed
# somewhere leaner. wget is likewise unnecessary: the healthcheck uses the binary.
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 agent \
    && adduser -D -u 10001 -G agent agent

WORKDIR /app

COPY --from=build /out/agent /app/agent
# A default config is baked in so the image runs unmodified; mount over
# /app/config to tune thresholds without rebuilding.
COPY config/config.yaml /app/config/config.yaml

# Created and owned up front: the daemon runs unprivileged, and both of these must
# be writable. data/ holds the SQLite database that makes a restart recoverable and
# logs/ holds the append-only audit trail — losing either defeats its purpose, so
# both are declared as volumes.
RUN mkdir -p /app/data /app/logs && chown -R agent:agent /app
VOLUME ["/app/data", "/app/logs"]

USER agent

EXPOSE 8080

# Probes /healthz, which returns 503 when the agent has faulted — unlike /api/status,
# which answers 200 in every state and so would report a wedged agent as healthy.
#
# The binary does the probing rather than wget: it reads the port from the same config the
# server binds, so a listen_addr change in the bind-mounted config cannot leave the
# container permanently unhealthy while the daemon is fine.
#
# Note this reports health; it does not act on it. compose's restart: unless-stopped
# ignores health status, so an unhealthy container is surfaced, not restarted.
HEALTHCHECK --interval=30s --timeout=8s --start-period=15s --retries=3 \
  CMD ["/app/agent", "-config", "/app/config/config.yaml", "-healthcheck"]

# No shell form: signals reach the process directly, so SIGTERM triggers the
# graceful shutdown the daemon already implements rather than being swallowed.
ENTRYPOINT ["/app/agent"]
CMD ["-config", "/app/config/config.yaml"]
