# syntax=docker/dockerfile:1

# Build stage. The full Go toolchain lives here and is discarded afterwards.
FROM golang:1.26-alpine AS build

# The Go toolchain stamps the commit into the binary by shelling out to git,
# and the alpine image has no git binary. Without this the stamp is skipped
# *silently* — the build succeeds and produces a binary that cannot say which
# revision it is, which is exactly the confusion the stamp exists to prevent.
RUN apk add --no-cache git

WORKDIR /src

# Copy the module files first and download dependencies as their own layer.
# Docker caches layers by input, so this step is only re-run when go.mod or
# go.sum change — editing source does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a statically linked binary with no libc dependency.
# This is only possible because the SQLite driver is pure Go (modernc.org/sqlite)
# rather than the usual cgo binding, and it is what lets the final image below
# contain nothing but the binary.
#
#   -trimpath       strips local filesystem paths from the binary
#   -ldflags -s -w  drops the symbol table and DWARF data, ~30% smaller
#   -buildvcs=true  stamps the commit — and *fails* if it cannot, rather than
#                   the default "auto", which quietly omits it. An image that
#                   cannot identify itself is how a stale container goes
#                   unnoticed, so this is worth failing the build over.
#
# -trimpath changes the build ID of every package it touches, which is all of
# them — including the standard library. That invalidates the toolchain's own
# shipped build cache, so without a cache of our own every build recompiles
# the standard library from scratch. The mount is that cache, kept across
# builds by BuildKit rather than baked into an image layer: a rebuild after a
# source change only recompiles what actually changed.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -buildvcs=true -ldflags="-s -w" -o /increader ./cmd/increader

# Runtime stage. distroless/static has no shell, no package manager and no libc:
# only CA certificates (needed for HTTPS to wallabag.it), /etc/passwd and a
# nonroot user. Nothing to exploit and nothing to keep patched.
FROM gcr.io/distroless/static:nonroot

COPY --from=build /increader /increader

# 65532 is distroless's "nonroot" user. The mounted data directory must be
# owned by this uid on the host, or the first write fails.
USER 65532:65532

EXPOSE 8080
ENTRYPOINT ["/increader"]
CMD ["serve"]
