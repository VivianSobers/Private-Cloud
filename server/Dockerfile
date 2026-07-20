# syntax=docker/dockerfile:1
#
# Multi-stage build for the private-cloud API.
#
# Build context is server/ — see the compose service definition.

# --------------------------------------------------------------------------
# build
# --------------------------------------------------------------------------
FROM golang:1.23-alpine AS build

WORKDIR /src

# Dependencies are copied and downloaded first so the (slow) module fetch is
# cached and only re-runs when go.mod/go.sum actually change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none

# CGO_ENABLED=0 produces a genuinely static binary, which is what lets the
# final stage be a distroless image with no libc at all.
# -trimpath strips local filesystem paths from the binary (smaller, and it
# avoids leaking the build machine's directory layout).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/api ./cmd/api

# --------------------------------------------------------------------------
# runtime
# --------------------------------------------------------------------------
# distroless/static holds no shell, no package manager and no libc — there is
# essentially nothing for an attacker who achieves RCE to pivot with. The
# :nonroot tag runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /api

USER nonroot:nonroot
EXPOSE 8080

# No shell in the image, so this must be exec form; there is nothing to
# interpret a shell string.
ENTRYPOINT ["/api"]
