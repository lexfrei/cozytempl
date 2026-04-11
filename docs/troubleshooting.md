# Troubleshooting

Common failure modes and how to diagnose them.

## "OIDC_ISSUER_URL is required; set COZYTEMPL_DEV_MODE=true to run with authentication disabled"

The server refuses to start if neither OIDC is configured nor `COZYTEMPL_DEV_MODE=true` is set explicitly. This is a deliberate safety net — an earlier iteration silently enabled dev mode when OIDC env vars were missing, which risks exposing an unauthenticated admin session to production.

**Fix**: either wire up all four OIDC variables (`OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URL`) **and** `SESSION_SECRET`, OR set `COZYTEMPL_DEV_MODE=true` if you're running locally against a cluster you control.

## "SESSION_SECRET is required in production and must not be the placeholder dev value"

OIDC is configured but the cookie signing key is missing or still the placeholder. A predictable signing key lets anyone forge session cookies.

**Fix**: generate a real secret and wire it in.

```bash
openssl rand -base64 32
```

## Login redirects to `/auth/callback?error=...`

The OIDC handshake failed. Causes in descending order of likelihood:

1. **Redirect URI mismatch.** The URL registered in your OIDC provider must match `OIDC_REDIRECT_URL` byte-for-byte, including scheme, host, port, and trailing slash.
2. **Client secret wrong or rotated.** Check the secret matches what's in the provider.
3. **Issuer URL not discoverable.** cozytempl fetches `${OIDC_ISSUER_URL}/.well-known/openid-configuration` at startup. A typo, a closed port, or a TLS cert the pod can't verify will make discovery fail.
4. **Clock skew.** JWTs carry `iat`/`exp` — if your cluster nodes' clocks drift more than a few minutes from the OIDC provider, tokens look expired. `chrony` or `systemd-timesyncd` on the hosts.

## "Failed to create tenant" / "Failed to create <app>"

cozytempl deliberately returns generic error messages on mutations so an attacker can't probe RBAC by reading specific errors. The real error is in the server log.

```bash
kubectl --namespace cozy-system logs deploy/cozytempl | jq 'select(.msg == "creating tenant" or .msg == "creating app")'
```

Common real causes:

- **RBAC denial.** The logged-in user doesn't have `create` on the CRD in the target namespace. `kubectl auth can-i --as=<user> create tenants -n tenant-root` tells you directly.
- **Name collision.** An object of the same kind/name already exists.
- **Chart validation.** Cozystack's admission webhook ran through the HelmRelease and rejected something in the spec — usually a missing required field or a value out of range.
- **Stale edit (optimistic locking conflict).** Two users edited the same resource; the second save gets "Another user modified ... Please reload and try again." Reload and retry.

## "Another user modified this tenant while you were editing"

You or someone else wrote to the same resource between the moment you opened the edit form and the moment you hit Save. cozytempl captures this deliberately (see optimistic locking in the README) to prevent silent data loss.

**Fix**: close the modal, reload the page, open the edit form again, re-apply your changes. The fresh form carries the new `resourceVersion`.

## Metrics scrape returns 403

`/metrics` is intentionally unauthenticated. A 403 means something in front of cozytempl is blocking it — usually an Ingress/Gateway that requires OAuth on every path.

**Fix**: carve out `/metrics` at your ingress layer. Alternatively, expose a dedicated pod port/Service for the metrics endpoint and point your scraper at that directly.

## SSE never connects / live updates don't fire

The HelmRelease watcher is cluster-scoped and runs in a background goroutine per cozytempl pod. If it fails to start (RBAC, API version mismatch, etc.) the `/api/events` endpoint is disabled and the server logs:

```
failed to start watcher, SSE will be unavailable
```

**Fix**: the cozytempl ServiceAccount needs `watch` on `helmreleases.helm.toolkit.fluxcd.io` at the cluster scope. The Helm chart's default ClusterRole doesn't grant this (only impersonation is cluster-scoped) — add a second ClusterRole if your deployment wants live updates:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cozytempl-watcher
rules:
  - apiGroups: ["helm.toolkit.fluxcd.io"]
    resources: ["helmreleases"]
    verbs: ["watch", "list"]
```

## "Showing the first 500 applications" banner on the tenant page

The tenant you're looking at has more than 500 applications, which is the current hard cap on `ApplicationService.List`. The filter and sort controls at the top of the table operate on the **current window only** — a filter that matches 3 of 500 shown apps may miss a match that exists in the pages cozytempl didn't fetch.

**Fix**: split the tenant into sub-tenants so each one has a manageable inventory. The cap exists specifically to prevent a pathological tenant from hanging the UI; growing past it is a signal that the tenant is doing too much.

## Dev mode banner visible on production

You shipped `COZYTEMPL_DEV_MODE=true` to a real deployment. This is dangerous — auth is disabled, every request is `dev-admin` with `system:masters`, and nothing gates API access to the cluster.

**Fix immediately**: set `COZYTEMPL_DEV_MODE=false` (or unset), restart the pod, and audit the logs between the moment dev mode went live and now:

```bash
kubectl --namespace cozy-system logs deploy/cozytempl --since=24h | jq 'select(.msg == "audit")'
```

Every mutation logged during that window ran as `dev-admin`, not as a real user, so the audit trail effectively has no actor. Treat the exposure window as a security incident.

## Container starts but the pod gets OOM-killed

Default memory limit is `256Mi`. That's enough for a small homelab. Large clusters (thousands of tenants, hundreds of apps per tenant) can push over this on the dashboard aggregation path.

**Fix**: bump the limit in `values.yaml`:

```yaml
resources:
  limits:
    memory: 512Mi
  requests:
    memory: 128Mi
```

If you still OOM after 512Mi, capture a heap profile via `/debug/pprof/heap` (not enabled by default; add the pprof handler behind a debug flag to diagnose) and file an issue.
