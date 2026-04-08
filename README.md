# cozytempl

Web UI for [Cozystack](https://cozystack.io/) platform management.

Built with Go + [templ](https://templ.guide/) + [htmx](https://htmx.org/) + [Alpine.js](https://alpinejs.dev/).

## Architecture

```text
Browser (Alpine.js + htmx + SSE)
        │
        ▼
Go HTTP Server
├── GET /              → SPA shell (templ)
├── GET /api/tenants   → JSON
├── GET /api/apps      → JSON
├── GET /api/schemas   → JSON (cached)
├── GET /api/events    → SSE stream
├── /auth/*            → OIDC flow
└── /static/*          → embedded assets
        │
        ▼
Kubernetes API (dynamic client, user impersonation)
```

- **Backend**: thin JSON API proxy to Kubernetes, no business logic duplication
- **Frontend**: Alpine.js handles all rendering, filtering, sorting, form generation
- **Auth**: OIDC (Keycloak), session cookies, K8s RBAC via user impersonation
- **Real-time**: Server-Sent Events for HelmRelease status updates
- **Deployment**: single static binary, distroless container (~15MB)

## Features

- Tenant hierarchy management (tree view, create/delete)
- Application CRUD for all Cozystack app types (Postgres, Redis, Kafka, Kubernetes clusters, VMs, etc.)
- Schema-driven forms generated from `values.schema.json`
- Real-time status badges via SSE
- Client-side caching with retry and offline resilience
- Dark theme, responsive layout

## Prerequisites

- Go 1.24+
- [templ](https://templ.guide/quick-start/installation/) CLI
- Access to a Cozystack cluster (via KUBECONFIG or in-cluster)
- OIDC provider (Keycloak)

## Configuration

All configuration is via environment variables:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OIDC_ISSUER_URL` | yes | | Keycloak realm URL |
| `OIDC_CLIENT_ID` | yes | | OAuth2 client ID |
| `OIDC_CLIENT_SECRET` | yes | | OAuth2 client secret |
| `OIDC_REDIRECT_URL` | yes | | Callback URL (`http://localhost:8080/auth/callback`) |
| `SESSION_SECRET` | yes | | Secret for cookie encryption (32+ chars) |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address |
| `LOG_LEVEL` | no | `info` | Log level |

## Development

```bash
# Install tools
make install-tools

# Run with live reload
export OIDC_ISSUER_URL=https://keycloak.example.com/realms/cozystack
export OIDC_CLIENT_ID=cozytempl
export OIDC_CLIENT_SECRET=your-secret
export OIDC_REDIRECT_URL=http://localhost:8080/auth/callback
export SESSION_SECRET=your-session-secret-at-least-32-chars

make dev
```

## Build

```bash
# Build binary
make build

# Run tests
make test

# Lint
make lint
```

## Container

```bash
docker build --file Containerfile --tag cozytempl .
docker run --publish 8080:8080 \
  --env OIDC_ISSUER_URL=... \
  --env OIDC_CLIENT_ID=... \
  --env OIDC_CLIENT_SECRET=... \
  --env OIDC_REDIRECT_URL=... \
  --env SESSION_SECRET=... \
  cozytempl
```

## Project Structure

```text
cmd/cozytempl/         Entry point, DI wiring
internal/
  api/                 HTTP handlers (JSON API)
  auth/                OIDC, sessions, middleware
  config/              Environment-based configuration
  k8s/                 Kubernetes client (tenants, apps, schemas, watcher)
  view/                templ SPA shell
static/
  css/                 Custom CSS (dark theme)
  js/                  Alpine.js components, API client with caching
```

## License

Apache-2.0
