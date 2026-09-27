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

# ca-certificates: required for TLS to Alpaca; without it every API call fails.
# tzdata: the binary embeds its own copy (see internal/scheduler), so this is only
# for anything that shells in and wants a sane local time.
RUN apk add --no-cache ca-certificates tzdata wget \
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

# Hits the status endpoint the page already serves. It reports the agent's own view
# of itself, so this catches a wedged daemon, not just a dead process.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O- http://127.0.0.1:8080/api/status >/dev/null || exit 1

# No shell form: signals reach the process directly, so SIGTERM triggers the
# graceful shutdown the daemon already implements rather than being swallowed.
ENTRYPOINT ["/app/agent"]
CMD ["-config", "/app/config/config.yaml"]
