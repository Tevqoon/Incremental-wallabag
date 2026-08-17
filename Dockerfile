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

# Runtime stage. Alpine, not distroless/static: pdftotext (poppler-utils) is
# a genuine process dependency now, not just a Go one — see
# internal/annotations/pdftext.go for why. A scanned book's OCR text
# routinely lives in a Form XObject that no small pure-Go PDF library
# resolves (rsc.io/pdf included), so recovering a highlight's quote from a
# real scanned PDF needs a full rendering engine behind it; poppler is one,
# distroless has no package manager to install one into, and shelling out
# to a real binary was judged less risky than reimplementing enough of the
# PDF content-stream/Form-XObject machinery ourselves to do the same thing
# in pure Go. This is a deliberate step down from "one static binary,
# nothing to exploit, nothing to keep patched" in exchange for actually
# working on real-world PDFs — not the default, so worth a comment where it
# was decided.
FROM alpine:3.20

RUN apk add --no-cache poppler-utils ca-certificates && \
    addgroup -g 65532 nonroot && \
    adduser -D -H -u 65532 -G nonroot nonroot

COPY --from=build /increader /increader

# 65532 matches distroless's own "nonroot" uid, kept here on purpose: the
# mounted data directory only ever had to be chowned to this uid once, and
# staying on it means an existing deployment's volume permissions do not
# need to change along with the base image.
USER 65532:65532

EXPOSE 8080
ENTRYPOINT ["/increader"]
CMD ["serve"]
