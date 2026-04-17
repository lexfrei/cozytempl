# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

All development flows through the `Makefile` — prefer it over raw `go`/`esbuild`/`helm` invocations.

```bash
make install-tools   # templ, air, govulncheck, helm-unittest
make generate        # templ generate → *_templ.go files
make ts              # esbuild bundle.js + theme-early.js into static/dist/
make build           # generate + ts + go build → bin/cozytempl
make dev             # air live reload (needs COZYTEMPL_AUTH_MODE=dev in env)
make test            # Go tests + helm-unittest
make test-all        # test + TypeScript tests (bun required)
make test-ts         # bun test static/ts/ only
make helm-test       # helm unittest only
make lint            # golangci-lint + govulncheck + eslint
make clean           # deletes bin/, tmp/, static/dist/, *_templ.go
```

Running a single Go test: `go test ./internal/k8s/ -run TestNewUserClient_Passthrough -race -count=1`.

**Build ordering is non-obvious.** `static/static.go` has `//go:embed css dist fonts`. `dist/` is gitignored and only exists after `make ts`, so `go build` / `golangci-lint` fail with `pattern dist: no matching files found` on a fresh clone or after `make clean` unless `ts` ran first. `build`/`lint`/`test` targets chain `generate` but NOT `ts` — run `make ts` yourself when hitting the embed error.

`templ generate` can silently report `updates=0` even when a `.templ` file changed — its cache misses real edits in some flows. Force a regen with `rm internal/**/*_templ.go && templ generate` if compilation errors say a templ symbol's signature still has the old shape after you edited the source.

## Architecture

### The auth-mode seam

The whole app pivots on `config.AuthMode` — set via `COZYTEMPL_AUTH_MODE` env var, one of `passthrough` (default), `byok`, `dev`, `impersonation-legacy`. Every k8s call flows through `k8s.NewUserClient(baseCfg, usr, mode)` in [internal/k8s/client.go](internal/k8s/client.go), which branches on the mode to decide how to convey user identity:

- `passthrough`: user's OIDC ID token → `cfg.BearerToken` (cozytempl SA has zero RBAC)
- `byok`: user-uploaded kubeconfig bytes → `clientcmd.RESTConfigFromKubeConfig`
- `impersonation-legacy`: `cfg.Impersonate` headers (deprecated)
- `dev`: baseCfg as-is (the process's own kubeconfig, dev-admin identity)

**Service structs hold the mode.** `TenantService`, `ApplicationService`, `SchemaService`, `UsageService`, `EventService`, `LogService` — all constructed with `(baseCfg, mode)` in [cmd/cozytempl/main.go](cmd/cozytempl/main.go), and every method takes `*auth.UserContext` as its credential input. Adding a new k8s-touching service: mimic `TenantService`, call `NewUserClient` in every method, never reach for `baseCfg` directly for user-scoped ops.

Full threat model per mode in [docs/auth-architecture.md](docs/auth-architecture.md).

### Watcher SA split

The HelmRelease watcher used by the SSE stream runs under a **separate** ServiceAccount `cozytempl-watcher` with only `list,watch` on `helmreleases.helm.toolkit.fluxcd.io`. The Helm chart mounts a projected token and points `COZYTEMPL_WATCHER_KUBECONFIG` at it; `loadWatcherKubeConfig()` in main.go picks it up. Locally (env unset) the watcher reuses the primary kubeconfig. This is why in passthrough/byok modes the main cozytempl SA can stay at zero RBAC.

### Template + htmx + SSE

Server-rendered templ pages with htmx for navigation/mutations and SSE for live HelmRelease updates. One unified reducer on the client keyed by a stable `row-{name}` id — create/update/delete go through the same DOM path regardless of whether the trigger is htmx or SSE. SSE streams are capped at 60 minutes (`sseMaxStreamAge` in api/sse.go); `EventSource` auto-reconnects with `Last-Event-ID` and the ring buffer replays missed events — this is how token refresh survives long connections.

### i18n

go-i18n bundle loaded from `internal/i18n/locales/active.{en,ru,kk,zh}.toml`. Templates pull the Localizer from context via `partial.Tc(ctx, id)` — no need to thread a Localizer parameter. **cozytempl translates only its own UI chrome.** Cluster-sourced strings (CRD openAPISchema labels, upstream k8s error messages) stay untranslated by design — see [memory](../.claude/projects/-Users-lex-git-github-com-lexfrei-cozytempl/memory/project_i18n_scope.md).

### Schema-driven forms

The app renders application create/edit forms by walking the `openAPISchema` on each ApplicationDefinition. This is intentional architecture, not a gap — per-resource custom form fields would be sync debt against upstream. Actions/tabs/labels per resource are fine; form fields aren't.

## Release pipeline gotchas

Release is triggered by pushing a `v*` tag. Three ghcr-specific quirks worth knowing:

1. **`oras push` needs `--annotation org.opencontainers.image.source=...`** to auto-link the ghcr package to the repo. Without it, the package is created unlinked and the Actions `GITHUB_TOKEN` gets `permission_denied: write_package` on subsequent `oras tag` / `cosign sign` calls. Docker images don't have this problem because `docker/metadata-action` sets the source label automatically. Fix lives in [.github/workflows/release.yaml](.github/workflows/release.yaml) — any new `oras push` must include the annotation.

2. **`docker/build-push-action` needs explicit `build-args: VERSION=... REVISION=...`** — they aren't auto-propagated from the tag. Source them from `fromJSON(steps.meta.outputs.json).labels['org.opencontainers.image.{version,revision}']` so they match the image tag that `docker/metadata-action` emits (the `v` prefix is already stripped).

3. **`workflow_run` privileged workflows always run the default-branch copy of the file.** Changes to `pr-privileged.yaml` don't take effect for PR runs until they land on master. Tested by merging first, then re-running the workflow.

## Session-specific conventions

- Commits use Conventional Commits (`type(scope): description`). The release changelog regex matches only `feat|fix|security|refactor|docs|perf|build` — `ci/chore/style` commits are filtered out of release notes by design.
- Branch protection on master: required checks `[Security Scan, Lint, Test, Lint Chart]`, `enforce_admins=true`. All changes go through PRs; direct pushes to master are rejected.
- README.md deliberately contains no architecture diagrams or directory tree — both duplicate information that is authoritative in the source and rot as packages are renamed. Keep narrative docs narrative.

## Operating environment (Cozystack platform contracts)

cozytempl runs on top of Cozystack. Two platform-side contracts the demo / dev flow depends on, and that have bitten us when missing:

- **`Tenant.spec.etcd: true` is required on the tenant whose name matches `_cluster.expose-ingress` (typically `root`).** That tenant chart materialises a Kamaji `DataStore` named after the tenant namespace (`tenant-root`). Without it, every `apps.cozystack.io/Kubernetes` Application install hangs waiting for `<release>-admin-kubeconfig`, because the Kamaji `vtenantcontrolplane` admission webhook rejects every `TenantControlPlane` with `tenant-root DataStore does not exist`. Symptom is identical to a chart-level race; the fix is one `kubectl patch tenant root --type=merge --patch '{"spec":{"etcd":true}}'`.
- **The cozystack `ingress-nginx-system` chart uses `Service: LoadBalancer` for every tenant whose name doesn't match `_cluster.expose-ingress`.** Only the `expose-ingress` tenant gets `externalIPs` populated from `_cluster.expose-external-ips`. On platforms without a working LB provider (no MetalLB IPAddressPool, no equivalent), the non-owning tenant's install times out on the LB-IP wait. Either configure the LB pool or set `Tenant.spec.ingress: false` on the tenant — external traffic for its apps still terminates on the `expose-ingress` tenant's controller via `Ingress.spec.ingressClassName: tenant-root`.
