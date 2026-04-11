# cozytempl

Web UI for [Cozystack](https://cozystack.io/) platform management.

Built with Go + [templ](https://templ.guide/) + [htmx](https://htmx.org/) + a small TypeScript bundle. No client-side framework — every page is server-rendered; htmx handles SPA navigation, partial swaps, and SSE; TypeScript only covers client-only concerns (modals, clipboard, progress bar, SSE reducer).

## Architecture

```text
Browser (htmx + EventSource + ~4 KB TypeScript)
        │
        ▼
Go HTTP Server
├── GET  /                       → Dashboard page (templ)
├── GET  /marketplace            → Application catalog
├── GET  /tenants                → Tenant management
├── POST /tenants                → create tenant
├── PUT  /tenants/{name}?ns=X    → edit tenant spec
├── DELETE /tenants/{name}?ns=X  → delete tenant
├── GET  /tenants/{tenant}       → tenant detail (apps + events + sub-tenants)
├── GET  /tenants/{tenant}/apps/{name}  → app detail (tabs: overview/connection/conditions/events/values)
├── POST /tenants/{tenant}/apps  → create app
├── PUT  /tenants/{tenant}/apps/{name}  → edit app spec
├── DELETE /tenants/{tenant}/apps/{name}  → delete app
├── GET  /profile                → current-user profile (impersonated identity)
├── GET  /fragments/*            → htmx partial swaps (app-table, marketplace grid, schema-fields, tenant-edit, app-edit)
├── GET  /api/tenants            → JSON
├── GET  /api/events?tenant=X    → SSE stream (authorized per tenant)
├── /auth/*                      → OIDC flow (prod) or dev bypass (opt-in)
└── /static/*                    → embedded css + TS bundle + source map
        │
        ▼
Kubernetes API (dynamic client, user impersonation on every call)
```

- **Backend**: thin impersonated proxy to Kubernetes. Every call uses `Impersonate-User` / `Impersonate-Group` headers so the API server enforces RBAC as the real user, not the service account running cozytempl.
- **Frontend**: server-rendered templ pages. htmx wires navigation (`hx-get`, `hx-target`, `hx-push-url`) and mutations (`hx-post`, `hx-put`, `hx-delete`, `hx-swap="delete swap:500ms"`). A ~4 KB TypeScript bundle drives the top progress bar, modal open/close, clipboard copy, toast dismissal, and a unified SSE resource-change reducer.
- **Auth**: OIDC (Keycloak) with encrypted cookie sessions in production; an explicit `COZYTEMPL_DEV_MODE=true` opt-in disables auth and grants `dev-admin` + `system:masters`. Dev mode is never silently activated by a missing env var.
- **Real-time**: Server-Sent Events for HelmRelease changes. The server emits a unified `{op, name, html}` message for added/modified and `{op, name}` for removed; the client runs one upsert/delete reducer keyed by a stable `row-{name}` id, so create / update / delete go through the same DOM path regardless of whether htmx or SSE triggers the change.
- **Deployment**: single static binary, distroless container (~17 MB), all CSS + TS assets embedded via `go:embed` with a SHA-256 cache-busting query string.

## Features

### Multitenancy
- Recursive tenant sidebar — walks the whole hierarchy, not just the first level.
- Tenant create with DNS-1123 name validation; root tenant is protected from deletion.
- Tenant edit modal with every top-level spec field pre-populated from the current CR.
- Tenant delete with explicit cascade warning.
- Sub-tenants card on the tenant detail page, showing children navigable in one click.
- Back-to-parent breadcrumb and button on non-root tenant pages.

### Applications
- Full CRUD for every Cozystack application type (Postgres, Redis, Kafka, Kubernetes clusters, VMs, Minecraft servers, etc.) via schema-driven forms generated from each ApplicationDefinition's `openAPISchema`.
- App edit modal with current values pre-loaded.
- Tab-based detail view: Overview, Connection info (with clipboard copy), Conditions, Events, Values.
- Recent activity card on each tenant page surfacing the 15 newest Kubernetes Events in that namespace.

### Platform
- Marketplace catalog with category pills, tag filtering, and live search.
- Dashboard with stats + recent applications.
- Profile page showing the impersonated username and groups so operators can verify what RBAC scope the current session is running under.
- Dark theme, responsive layout, mobile burger menu.
- Global top progress bar on every htmx request; per-button spinner overlays; row fade-out animation on delete.

### Security
- Every Kubernetes call uses user impersonation, so browser-visible data respects cluster RBAC.
- SSE subscriptions are authorized per tenant before the watcher adds the subscriber — an authenticated user who guesses another tenant's name cannot receive its event stream.
- Label-selector injection guard on all application name parameters (DNS-1123 alphanumeric + dash/underscore/dot only).
- `SESSION_SECRET` is required in production; a placeholder or missing value is a fatal load error. Dev mode generates a per-process random secret via `crypto/rand`.
- Security headers on every response: `Content-Security-Policy`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, `Permissions-Policy`.
- `Cache-Control: no-store` on app detail pages so the connection-info tab's credentials never hit an intermediate cache.
- Per-request `context.WithTimeout` so a hung K8s call cannot park a handler goroutine.

## Prerequisites

- Go 1.24+
- [templ](https://templ.guide/quick-start/installation/) CLI
- `esbuild` (via npm — `make install-tools`)
- Access to a Cozystack cluster (via `KUBECONFIG` or in-cluster)
- OIDC provider for production (Keycloak, Dex, etc.)

## Configuration

All configuration is via environment variables:

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `OIDC_ISSUER_URL` | prod only | | OIDC discovery URL (e.g. Keycloak realm) |
| `OIDC_CLIENT_ID` | prod only | | OAuth2 client ID |
| `OIDC_CLIENT_SECRET` | prod only | | OAuth2 client secret |
| `OIDC_REDIRECT_URL` | prod only | | Callback URL (`https://host/auth/callback`) |
| `SESSION_SECRET` | prod only | | Cookie signing key; any non-empty non-placeholder value |
| `COZYTEMPL_DEV_MODE` | no | `false` | Set to `true` to bypass OIDC and run as `dev-admin` + `system:masters`. Fails startup if OIDC is unset and this flag is not `true`. |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address |
| `LOG_LEVEL` | no | `info` | Log level (`debug`, `info`, `warn`, `error`) |

## Development

```bash
# Install tools (templ, air, eslint)
make install-tools

# Dev mode: no OIDC required, runs as dev-admin + system:masters
export COZYTEMPL_DEV_MODE=true
export KUBECONFIG=/path/to/kubeconfig

# Live reload via air
make dev
```

## Build

```bash
make build   # templ generate + esbuild + go build
make test    # generate + go test with -race
make lint    # golangci-lint + eslint
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
cmd/cozytempl/           Entry point, DI wiring
internal/
  api/                   JSON API + SSE endpoint + router with security
                         headers and request-timeout middleware
  auth/                  OIDC, sessions, dev-mode bypass, middleware
  config/                Environment-based configuration with strict
                         production validation
  handler/               HTML page handlers that render templ pages
  k8s/                   Impersonated Kubernetes client:
                         tenant service, application service, schema
                         service (with List-fallback for hyphenated
                         CRD names), event service, usage collector,
                         HelmRelease watcher
  view/
    layout/              Base + app shell templates
    page/                Full-page templates (dashboard, marketplace,
                         tenant list, tenant detail, app detail,
                         profile, etc.)
    fragment/            htmx partial templates (schema fields,
                         tenant-edit modal, app-edit modal,
                         marketplace grid, app-table rows)
    partial/             Shared components (header, sidebar, toast,
                         badge, breadcrumb)
static/
  css/                   Dark theme + htmx feedback states
  ts/                    TypeScript source (main, sse, htmx progress
                         bar, modal, toast, clipboard)
  dist/                  esbuild output — bundled & minified
```

## License

Apache-2.0
