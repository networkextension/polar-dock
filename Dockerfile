# Multi-stage dock image. Final image carries:
#   /app/dock                        the dock binary for $TARGETPLATFORM
#   /app/dist/agents/                mount point for polar-agent binaries
#                                    (agent extracted to its own repo; stage/
#                                     mount here so /api/agent/binary serves them)
#
# UI is no longer baked in — the dock binary serves /api only. The
# static UI lives in its own repo (networkextension/polar-dock-ui)
# and is fronted by nginx (or any static server) at deploy time.
# See README "UI" section for the rsync/mount recipe.
#
# POLAR_AGENT_DIST_DIR points at /app/dist/agents (empty mount point).
# Stage agent binaries there at deploy, or pull from the agent's own
# image/releases (ghcr.io/networkextension/polar-agent).
#
# Build (multi-arch):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t ghcr.io/<org>/polar-dock:latest --push .
#
# Run:
#   docker run -d --name polar-dock \
#     -p 8080:8080 \
#     -e POSTGRES_DSN=... -e REDIS_ADDR=... \
#     ghcr.io/<org>/polar-dock:latest

# ──────────────────────────────────────────────────────────────────
# Stage 1: cross-compile polar-agent for every supported target
#
# NOTE: polar-agent was extracted to its own repo (networkextension/polar-agent)
# — its source no longer lives here, so the dock image no longer builds/bundles
# agent binaries. /api/agent/binary is served from POLAR_AGENT_DIST_DIR, now a
# mount point (stage agents at deploy, or pull from ghcr.io/.../polar-agent).
# ──────────────────────────────────────────────────────────────────
# Stage 2: build dock binary for $TARGETPLATFORM
# ──────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS dock-builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /work
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags='-s -w' -o /dock ./cmd/dock

# ──────────────────────────────────────────────────────────────────
# Final: minimal runtime image
#
# alpine (not distroless) so the operator can `docker exec` for
# debugging — saves a lot of "what's the env in there?" friction
# for early adopters. Switch to distroless when prod-stable.
# ──────────────────────────────────────────────────────────────────
FROM alpine:3.20 AS runtime

# tini for proper signal handling (Ctrl-C / SIGTERM through to dock).
# ca-certificates for outbound HTTPS (LLM APIs, Apple ASC).
RUN apk add --no-cache tini ca-certificates tzdata curl

WORKDIR /app

COPY --from=dock-builder /dock                 /app/dock
RUN mkdir -p /app/dist/agents   # mount point — agents staged here at deploy

# Env defaults: dock listens 0.0.0.0:8080, agents served from baked-in
# dir, upload + markdown dirs are volume mount points so they survive
# container restarts.
ENV ADDR=":8080" \
    UPLOAD_DIR=/data/uploads \
    MARKDOWN_DIR=/data/markdown \
    POLAR_AGENT_DIST_DIR=/app/dist/agents

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/app/dock"]
