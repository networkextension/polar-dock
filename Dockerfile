# Multi-stage dock image. Final image carries:
#   /app/dock                        the dock binary for $TARGETPLATFORM
#   /app/ui/dist/                    built UI static + express server
#   /app/dist/agents/                polar-agent binaries for ALL platforms
#                                    (so /api/agent/binary works zero-config)
#
# POLAR_AGENT_DIST_DIR is set to /app/dist/agents so the binary
# endpoint serves them out of the box. Operator can override to
# point at a host-mounted dir if they want fresher builds.
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
# Stage 1: build UI
# ──────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM node:22-alpine AS ui-builder
WORKDIR /work
COPY ui/package.json ui/package-lock.json ./ui/
RUN cd ui && npm ci
COPY ui/ ./ui/
RUN cd ui && npm run build

# ──────────────────────────────────────────────────────────────────
# Stage 2: cross-compile polar-agent for every supported target
#
# Build all 5 (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64,
# windows/amd64) in one go so the final image can serve any of them
# via /api/agent/binary regardless of the dock host's own arch.
# ──────────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS agent-builder
WORKDIR /work
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN mkdir -p /agents \
 && for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
      os="${target%/*}"; arch="${target#*/}"; \
      out="/agents/polar-agent_${os}_${arch}"; \
      [ "$os" = "windows" ] && out="${out}.exe"; \
      echo "  building $out"; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath -ldflags='-s -w' -o "$out" ./cmd/polar-agent; \
    done

# ──────────────────────────────────────────────────────────────────
# Stage 3: build dock binary for $TARGETPLATFORM
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
COPY --from=ui-builder   /work/ui/dist/        /app/ui/dist/
COPY --from=ui-builder   /work/ui/server.js    /app/ui/server.js
COPY --from=ui-builder   /work/ui/node_modules /app/ui/node_modules
COPY --from=ui-builder   /work/ui/package.json /app/ui/package.json
COPY --from=agent-builder /agents              /app/dist/agents

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
