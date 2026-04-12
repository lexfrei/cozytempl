# syntax=docker/dockerfile:1.23

# --- TypeScript bundle stage ---------------------------------------------
# esbuild is pure JS and arch-agnostic, so this stage pins to
# $BUILDPLATFORM and runs natively on the host runner. The output
# (static/dist/bundle.js + theme-early.js) is byte-for-byte identical
# for every target platform and gets copied into the Go builder below.
FROM --platform=$BUILDPLATFORM docker.io/library/node:24.14.1-alpine@sha256:01743339035a5c3c11a373cd7c83aeab6ed1457b55da6a69e014a95ac4e4700b AS webbuilder

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
# natively on the host runner. ARG TARGETOS / TARGETARCH come from buildx
# and drive the cross-compile flags at the final go build call.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26-alpine@sha256:c2a1f7b2095d046ae14b286b18413a05bb82c9bca9b25fe7ff5efef0f0826166 AS builder

ARG VERSION=development
ARG REVISION=development

# Build a minimal /etc/passwd entry for 'nobody'. The final scratch
# image needs this so a process running as UID 65534 has a proper
# name→uid mapping; without it, anything calling user.Current() or
# os/user.LookupId would break. 65534 is the standard nobody UID
# across virtually every Linux distro.
RUN echo 'nobody:x:65534:65534:Nobody:/:' > /tmp/passwd

# templ pinned to the runtime library version from go.mod so the
# generator and the runtime library can never drift. Bump this
# together with the go.mod dependency — Renovate will do so via the
# regex manager in .github/renovate.json5.
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
      go build \
        -ldflags="-s -w -X main.Version=${VERSION} -X main.Revision=${REVISION}" \
        -trimpath \
        -o /cozytempl ./cmd/cozytempl

# --- Runtime stage -------------------------------------------------------
# FROM scratch is the smallest possible base — nothing but our binary
# plus the two files a CGO-less Go HTTPS client needs to talk to the
# k8s API and OIDC provider: ca-certificates (TLS root store) and
# /etc/passwd (UID→name mapping for the non-root user).
FROM scratch

COPY --from=builder /tmp/passwd /etc/passwd
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder --chmod=555 /cozytempl /cozytempl

USER 65534
EXPOSE 8080/tcp
ENTRYPOINT ["/cozytempl"]
