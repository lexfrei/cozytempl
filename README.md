# cozytempl

Web UI for [Cozystack](https://cozystack.io/) platform management.

Go + [templ](https://templ.guide/) + [htmx](https://htmx.org/) + ~22 KB of bundled TypeScript. No SPA framework — every page is server-rendered, htmx handles navigation and mutations, TypeScript only covers genuine client-only concerns (modals, clipboard, progress bar, SSE reducer, click-to-reveal timer).

## Quick start

### Helm (recommended)

```bash
# Dev mode: no OIDC, auth disabled, "dev-admin" with system:masters.
# Use this on a cluster you control locally.
helm install cozytempl deploy/helm/cozytempl \
  --namespace cozy-system --create-namespace \
  --set config.authMode=dev

kubectl --namespace cozy-system port-forward svc/cozytempl 8080:8080
open http://localhost:8080
```

```bash
# Production (recommended: passthrough). OIDC ID tokens are forwarded
# directly to the Kubernetes API; cozytempl itself holds no cluster
# permissions. Requires an OIDC-configured API server.
helm install cozytempl deploy/helm/cozytempl \
  --namespace cozy-system --create-namespace \
  --set config.authMode=passthrough \
  --set config.oidc.issuerURL=https://keycloak.example.com/realms/cozystack \
  --set config.oidc.clientID=cozytempl \
  --set config.oidc.redirectURL=https://cozy.example.com/auth/callback \
  --set config.oidc.clientSecret="$(pass show kc/cozytempl/client-secret)" \
  --set config.oidc.sessionSecret="$(openssl rand -base64 32)"
```

See [`docs/auth-architecture.md`](docs/auth-architecture.md) for a threat-model breakdown of all four auth modes and [`docs/migrating-to-passthrough-auth.md`](docs/migrating-to-passthrough-auth.md) for the upgrade path from the legacy impersonation model.

The chart has its own [README](deploy/helm/cozytempl/README.md) with a full values reference, a `values.schema.json` that catches typos at `helm install --dry-run` time, and a `helm-unittest` suite (`make helm-test`).

### Local binary

```bash
make install-tools   # templ, air, govulncheck, helm-unittest, eslint
make build           # templ generate → esbuild → go build

KUBECONFIG=~/.kube/config COZYTEMPL_DEV_MODE=true ./bin/cozytempl
```

## Architecture

```text
Browser (htmx 2.0.8 bundled + EventSource + ~22 KB TypeScript)
        │
        ▼
Go HTTP Server
├── GET  /                       → Dashboard
├── GET  /marketplace            → Application catalog
├── GET  /tenants                → Tenant management
├── POST /tenants                → create tenant
├── PUT  /tenants/{name}?ns=X    → edit tenant spec (with optimistic locking)
├── DELETE /tenants/{name}?ns=X  → delete tenant
├── GET  /tenants/{tenant}       → tenant detail (apps + events + sub-tenants)
├── GET  /tenants/{tenant}/apps/{name}  → app detail
├── POST /tenants/{tenant}/apps  → create app
├── PUT  /tenants/{tenant}/apps/{name}  → edit app spec
├── DELETE /tenants/{tenant}/apps/{name}  → delete app
├── GET  /fragments/*            → htmx partial swaps (app-table, marketplace,
│                                   schema-fields, tenant-edit, app-edit,
│                                   secrets/reveal)
├── GET  /api/tenants            → JSON API
├── GET  /api/events?tenant=X    → SSE stream (authorized per tenant)
├── GET  /metrics                → Prometheus exposition (unauthenticated,
│                                   protect at network layer)
├── GET  /healthz, /readyz       → k8s liveness / readiness
├── /auth/*                      → OIDC flow (prod) or dev bypass (opt-in)
└── /static/*                    → embedded css + TS bundle + fonts
        │
        ▼
Kubernetes API (dynamic client, user impersonation on every call)
```

- **Backend**: thin credential forwarder to the Kubernetes API. In the recommended `passthrough` mode the user's OIDC ID token is used as a Bearer credential on every k8s call, and the cozytempl pod's ServiceAccount has zero cluster permissions — a compromise of the cozytempl process cannot impersonate anyone. See [`docs/auth-architecture.md`](docs/auth-architecture.md) for the per-mode threat model.
- **Frontend**: server-rendered templ pages. htmx wires navigation and mutations. A small TypeScript bundle drives the top progress bar, modal lifecycle, clipboard copy, toast dismissal, the unified SSE resource-change reducer, and the click-to-reveal auto-hide timer.
- **Auth**: four modes selected via `COZYTEMPL_AUTH_MODE`. `passthrough` (default) forwards the OIDC ID token as a Bearer; `byok` lets the user upload a kubeconfig stored encrypted in the session cookie; `impersonation-legacy` keeps the old Impersonate-headers model (deprecated); `dev` disables auth entirely and prints a loud banner. OIDC ID tokens are refreshed automatically shortly before they expire.
- **Real-time**: Server-Sent Events for HelmRelease changes. The server emits a unified `{op, name, html}` message; the client runs one upsert/delete reducer keyed by a stable `row-{name}` id, so create / update / delete go through the same DOM path regardless of whether htmx or SSE triggers the change.
- **Deployment**: single static binary, distroless container, all CSS + TS bundle + fonts embedded via `go:embed` with a SHA-256 cache-busting query string. The Helm chart is the canonical install path.

## Features

### Multitenancy

- Recursive tenant sidebar — walks the whole hierarchy.
- Tenant create with DNS-1123 name validation; root tenant is protected.
- Tenant edit modal with every top-level spec field pre-populated.
- Sub-tenants card on the tenant detail page.
- Back-to-parent breadcrumb and button on non-root tenant pages.

### Applications

- Full CRUD for every Cozystack application type via schema-driven forms generated from each ApplicationDefinition's `openAPISchema`.
- App edit modal with current values pre-loaded.
- Tab-based detail view: Overview, Connection, Conditions, Events, Logs, Values.
- **Click-to-reveal credentials** on the Connection tab: passwords, tokens and API keys render as placeholder dots until the user explicitly requests disclosure. The real value is fetched on demand, shown for 30 seconds, then auto-hidden. Every reveal emits an audit event.
- Hard cap of 500 applications per tenant list with a visible truncation banner — a 10k-app tenant can't hang the UI or push the k8s API beyond budget.

### Observability

- **Prometheus `/metrics`** with bounded label cardinality: `cozytempl_http_requests_total`, `cozytempl_http_request_duration_seconds`, `cozytempl_http_requests_inflight`, plus Go runtime and process collectors. Path labels are normalised (`/tenants/:ns`, `/tenants/:ns/apps/:name`) so tenant and app names never leak into the label space.
- **Request correlation IDs** on every request (`X-Request-ID` header, honours trusted upstream values, otherwise mints a UUID). Every access-log line and every audit event carries the same ID.
- **Structured access log** — one `http` log line per request with method, path, status, duration and request ID.
- **Structured audit log** — one `audit` log line per mutation (tenant/app create/update/delete) and per `connection.view` on the Connection tab. JSON-serialisable, keyed on stable action strings (`tenant.create`, `app.update`, `secret.view`, ...). Pod logs forwarded to Loki / ELK become the append-only audit store; no new storage dependency.

### Security

- **Four auth modes**: `passthrough` (OIDC ID token → Bearer, cozytempl SA has zero RBAC), `byok` (user uploads a kubeconfig, stored encrypted in the session cookie), `impersonation-legacy` (Impersonate headers, deprecated), `dev` (no auth, loud banner). Threat model in [`docs/auth-architecture.md`](docs/auth-architecture.md); migration steps in [`docs/migrating-to-passthrough-auth.md`](docs/migrating-to-passthrough-auth.md).
- **Automatic OIDC refresh** in passthrough mode. The middleware transparently refreshes the ID token within 60 seconds of expiry, long SSE connections are capped at 60 minutes so reconnect picks up a fresh token, and a revoked refresh token logs the user out on the next request.
- **Watcher SA split**: the HelmRelease watcher used by the SSE stream runs under a dedicated `cozytempl-watcher` ServiceAccount with only `list,watch` on `helmreleases.helm.toolkit.fluxcd.io`. In passthrough and byok modes this is the only k8s RBAC the chart installs.
- Every Kubernetes call uses user-scoped credentials, so browser-visible data respects cluster RBAC.
- Strict CSP (`default-src 'self'`, `script-src 'self'`, `object-src 'none'`, `frame-src 'none'`, `frame-ancestors 'none'`, `base-uri 'self'`, `form-action 'self'`). No third-party origins — htmx and Inter are bundled locally.
- HSTS with a 2-year max-age, `includeSubDomains`, `preload`.
- Session cookie is `HttpOnly`, `Secure`, `SameSite=Lax` — the Lax setting is the documented CSRF defense, no per-form tokens needed.
- Per-user token-bucket **rate limiting** (30 burst, 20 req/s refill) keyed on the authenticated username. A noisy user can't DoS the k8s API through cozytempl.
- **Optimistic locking** on every Update. The edit form echoes the observed `resourceVersion`; a concurrent write by another user produces a visible "please reload and try again" error instead of a silent clobber.
- SSE subscriptions are authorized per tenant before the watcher adds the subscriber.
- Label-selector injection guard on all application-name parameters.
- `SESSION_SECRET` is required in production; a placeholder or missing value is a fatal load error.
- `Cache-Control: no-store` on app detail pages so the Connection tab's credentials never hit an intermediate cache.
- Per-request `context.WithTimeout` plus a k8s client-side transport timeout so a hung control plane can't park a goroutine.
- `govulncheck` in `make lint`, Dependabot for Go + npm + GitHub Actions.

### Platform

- Marketplace catalog with category pills, tag filtering, and live search.
- Dashboard with stats + recent applications.
- Profile page showing the impersonated username and groups.
- Dark theme, responsive layout, mobile burger menu, branded 404 error pages.
- Dev-mode banner — a loud red strip at the top of every page whenever `COZYTEMPL_DEV_MODE=true` so an accidentally-exposed dev instance is impossible to miss.

## Prerequisites

- Go 1.24+
- [templ](https://templ.guide/quick-start/installation/) CLI
- `esbuild` (via npm — `make install-tools`)
- Access to a Cozystack cluster (via `KUBECONFIG` or in-cluster)
- OIDC provider for production (Keycloak, Dex, etc.)

## Configuration

All configuration is via environment variables. The Helm chart exposes the same options under `.Values.config`.

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `COZYTEMPL_AUTH_MODE` | no | `passthrough` when OIDC is set, else error | One of `passthrough`, `byok`, `dev`, `impersonation-legacy`. See [`docs/auth-architecture.md`](docs/auth-architecture.md). |
| `OIDC_ISSUER_URL` | passthrough / legacy | | OIDC discovery URL (e.g. Keycloak realm) |
| `OIDC_INTERNAL_ISSUER_URL` | no | `OIDC_ISSUER_URL` | Backend-only issuer URL for token exchange / JWKS. Typically a cluster-internal Keycloak service (`http://keycloak.cozy-keycloak.svc:8080/realms/cozystack`); the user-facing redirect keeps `OIDC_ISSUER_URL`. |
| `OIDC_CLIENT_ID` | passthrough / legacy | | OAuth2 client ID |
| `OIDC_CLIENT_SECRET` | passthrough / legacy | | OAuth2 client secret |
| `OIDC_REDIRECT_URL` | passthrough / legacy | | Callback URL (`https://host/auth/callback`) |
| `SESSION_SECRET` | every mode except `dev` | | 32+ random bytes; `openssl rand -base64 32` |
| `COZYTEMPL_DEV_MODE` | no | `false` | Legacy opt-in for `dev` mode. Equivalent to `COZYTEMPL_AUTH_MODE=dev` and wins when both are set. |
| `COZYTEMPL_WATCHER_KUBECONFIG` | no | unset | Path to a kubeconfig the HelmRelease watcher uses. Unset means the watcher reuses the primary kubeconfig; the Helm chart sets this to the projected token of the `cozytempl-watcher` ServiceAccount. |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address |
| `LOG_LEVEL` | no | `info` | Log level (`debug`, `info`, `warn`, `error`) |

## RBAC

RBAC shape depends on the active `authMode`:

| Mode | Main cozytempl SA | Watcher SA |
| --- | --- | --- |
| `passthrough` | No ClusterRole. The OIDC ID token is the only credential k8s sees. | `list`, `watch` on `helmreleases.helm.toolkit.fluxcd.io` only. |
| `byok` | No ClusterRole. The uploaded kubeconfig is the only credential. | Same narrow list+watch as passthrough. |
| `impersonation-legacy` | `impersonate` on `users`, `groups`, `serviceaccounts`, `userextras/scopes`. Deprecated. | Same narrow list+watch (belt and suspenders). |
| `dev` | None. The process uses whatever local kubeconfig it loads. | Not rendered. |

In passthrough and byok modes, **a compromise of the cozytempl process cannot impersonate any user** — there is no ambient k8s authority to escalate through. The operator must ensure their OIDC-mapped users already have RBAC on the cozystack CRDs (`apps.cozystack.io`, `cozystack.io`), on `helm.toolkit.fluxcd.io/helmreleases`, and on the `tenant-*` namespaces they should see. Same wiring as `kubectl --as` or kubectl with OIDC auth-provider.

The Helm chart's `rbac.create: true` (default) renders the right shape per mode automatically.

## Observability

### Prometheus

Scrape `/metrics` on the cozytempl pod. The endpoint is intentionally **unauthenticated** — Prometheus operators don't have OIDC credentials, and requiring auth here breaks every stock scrape config. Protect it at the network layer (NetworkPolicy, service mesh, proxy sidecar).

```yaml
- job_name: cozytempl
  kubernetes_sd_configs:
    - role: pod
      namespaces:
        names: [cozy-system]
  relabel_configs:
    - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_name]
      action: keep
      regex: cozytempl
    - source_labels: [__meta_kubernetes_pod_container_port_name]
      action: keep
      regex: http
```

### Audit log

Audit events share the same stdout stream as application logs but carry a stable `"audit":"event"` marker so downstream pipelines can split them out. Example with `jq`:

```bash
kubectl --namespace cozy-system logs deploy/cozytempl | jq -c 'select(.audit == "event")'
```

Each event has `action`, `actor`, `groups`, `resource`, `tenant`, `outcome`, `request_id`, and an optional `details` map. See [`internal/audit/audit.go`](internal/audit/audit.go) for the full schema.

Forward pod logs to Loki / ELK / CloudWatch / wherever your compliance team keeps append-only storage. That's your audit trail.

### Request correlation

Every request mints a UUID (or honours a validated upstream `X-Request-ID`) and echoes it in both the response header and every log line. Grep a single request out of the log stream with `jq -c 'select(.request_id == "<id>")'`.

### Tracing (OpenTelemetry)

Opt-in. When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, cozytempl wraps every HTTP request in a span via `otelhttp`, installs W3C TraceContext + Baggage propagators, and exports spans to the configured OTLP collector. When the env var is empty, the global TracerProvider stays at its zero-cost no-op default — operators who don't run a collector pay nothing for the span pipeline.

Supported env vars (standard OTel SDK, no cozytempl-specific flags):

| Variable | Description |
| --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector host:port or URL. Empty disables tracing. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` (default) or `http/protobuf`. |
| `OTEL_SERVICE_NAME` | `service.name` attribute on every span. Defaults to `cozytempl`. |
| `OTEL_RESOURCE_ATTRIBUTES` | Extra comma-separated resource attributes. |

The Helm chart exposes this under `config.tracing.otlpEndpoint`, `config.tracing.otlpProtocol`, and `config.tracing.serviceName`. Example with Tempo:

```bash
helm upgrade cozytempl deploy/helm/cozytempl \
  --set config.tracing.otlpEndpoint=tempo.observability:4317 \
  --set config.tracing.serviceName=cozytempl-prod
```

## Development

```bash
make install-tools   # templ, air, govulncheck, helm-unittest, eslint
make dev             # air with live reload (COZYTEMPL_DEV_MODE=true required)
make build           # templ generate → esbuild → go build
make test            # go test + helm-unittest
make lint            # golangci-lint + govulncheck + eslint
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
deploy/helm/cozytempl/   Helm chart (Deployment, Service, ServiceAccount,
                         ClusterRole, Secret), values.schema.json, unit tests
internal/
  api/                   Router, middleware stack (request ID → metrics →
                         access log → security headers → timeout),
                         rate limiting, /metrics endpoint
  audit/                 Structured audit event types + JSON slog logger +
                         request-ID context helpers (shared by api/handler)
  auth/                  OIDC, sessions, dev-mode bypass, middleware
  config/                Environment-based configuration with strict
                         production validation
  handler/               HTML page handlers that render templ pages
  k8s/                   Impersonated Kubernetes client: tenant service,
                         application service (with optimistic locking),
                         schema service, event service, usage collector,
                         HelmRelease watcher, deep-merge for spec updates
  view/
    layout/              Base + app shell templates
    page/                Full-page templates
    fragment/            htmx partial templates
    partial/             Shared components (header, sidebar, toast, ...)
static/
  css/                   Dark theme + loading-state classes + truncation
                         banner + click-to-reveal styles
  ts/                    TypeScript source (main, sse, htmx progress bar,
                         modal, toast, clipboard, reveal)
  fonts/                 Self-hosted Inter woff2
  dist/                  esbuild output — bundled & minified (gitignored)
```

## License

Apache-2.0
