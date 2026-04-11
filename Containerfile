# syntax=docker/dockerfile:1.7

# Multi-arch builds run through a classic cross-compile pattern:
# every builder stage runs on $BUILDPLATFORM (the host's native arch,
# so no QEMU/Rosetta emulation), and only the final binary is targeted
# at $TARGETPLATFORM via GOOS/GOARCH. This is the only way to avoid
# the Rosetta + Go-runtime panics that fire on Apple Silicon hosts
# whenever amd64 tries to run `go install` under emulation.

# --- TypeScript bundle stage ---------------------------------------------
# Runs on the native host arch. esbuild is pure JS, arch-independent,
# and the output is byte-for-byte the same for every target platform —
# so it's safe to build once and reuse.
FROM --platform=$BUILDPLATFORM node:20-alpine AS webbuilder

WORKDIR /src
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --fund=false

COPY static ./static
COPY tsconfig.json ./
RUN npx esbuild static/ts/main.ts --bundle --outfile=static/dist/bundle.js --minify --sourcemap \
 && npx esbuild static/ts/theme-early.ts --bundle --outfile=static/dist/theme-early.js --minify

# --- Go builder stage ----------------------------------------------------
# Pinned to $BUILDPLATFORM so the Go toolchain and templ generator run
# natively. ARG TARGETOS / TARGETARCH come from buildx and drive the
# cross-compile flags below.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

RUN apk add --no-cache git

# templ is pinned to the same version go.mod uses for the runtime
# library so the generator and the runtime never drift. Bumping this
# is a single grep across go.mod + Containerfile.
RUN go install github.com/a-h/templ/cmd/templ@v0.3.1001

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod \
    go mod download

COPY . .
COPY --from=webbuilder /src/static/dist ./static/dist

RUN templ generate

ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -ldflags="-s -w" -o /cozytempl ./cmd/cozytempl

# --- Runtime stage -------------------------------------------------------
# Distroless/static is published as a multi-arch manifest list, so
# buildx selects the matching image for $TARGETPLATFORM automatically.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /cozytempl /cozytempl

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/cozytempl"]
