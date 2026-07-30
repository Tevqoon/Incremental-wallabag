# Build stage. The full Go toolchain lives here and is discarded afterwards.
FROM golang:1.26-alpine AS build

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
#   -trimpath   strips local filesystem paths from the binary
#   -ldflags -s -w  drops the symbol table and DWARF data, ~30% smaller
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /increader ./cmd/increader

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
