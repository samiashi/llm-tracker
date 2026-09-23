# syntax=docker/dockerfile:1

# The server is one static binary with the dashboard compiled into it, so the
# image is the binary, a CA bundle, timezone data and nothing else.

# ---- dashboard -------------------------------------------------------------
# Vite writes into ../server/internal/web/dist, where //go:embed all:dist looks,
# so the server embeds this build and never one left in a developer's tree.
FROM --platform=$BUILDPLATFORM node:24-alpine AS dashboard
WORKDIR /src/dashboard
COPY dashboard/package.json dashboard/package-lock.json ./
RUN npm ci
COPY dashboard/ ./
RUN npm run build

# ---- server ----------------------------------------------------------------
# Both build stages run on the build platform and cross-compile: under QEMU an
# arm64 image turns a one-minute build into ten.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

# The server module alone, not the workspace: server/go.mod replaces schema
# with ../schema, so an agent-only change does not rebuild the image.
WORKDIR /src/server
COPY schema/ /src/schema/
COPY server/ /src/server/
COPY --from=dashboard /src/server/internal/web/dist internal/web/dist

# CGO_ENABLED=0 with a pure-Go SQLite driver gives a static binary, which the
# runtime stage needs: it has no libc to link against.
ARG VERSION=dev
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/llm-tracker-server .

# /data belongs to the runtime user, so a volume Docker creates for it -- the
# anonymous one VOLUME declares, or a named one -- starts with that owner and
# the nonroot server can open its database. A bind mount keeps its host
# directory's owner instead, which is why startup.sh sets that.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# ---- runtime ---------------------------------------------------------------
# distroless/static rather than scratch: this needs a CA bundle to complete the
# GitHub OAuth exchange, and zoneinfo because the activity heatmap renders in
# the server's local time. On scratch the first fails at login and the second
# fails silently, reporting every hour in UTC.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/llm-tracker-server /llm-tracker-server
COPY --from=build --chown=65532:65532 /out/data /data

# Declared so a `docker run` with no mount keeps the database in a volume,
# which outlives the container, not in the container's own layer, which is
# removed with it.
VOLUME ["/data"]

EXPOSE 8790

# The binary probes itself: distroless has no shell and no curl, so a
# HEALTHCHECK has nothing else to call.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/llm-tracker-server", "-healthcheck"]

USER nonroot:nonroot
# 0.0.0.0 because that is the only address reachable from outside the
# container's own network namespace.
ENTRYPOINT ["/llm-tracker-server"]
CMD ["-addr", "0.0.0.0:8790", "-db", "/data/llm-tracker.db"]
