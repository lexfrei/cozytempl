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

## "Failed to create tenant" / "Failed to create &lt;app&gt;"

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

The HelmRelease watcher runs in a background goroutine under a dedicated `cozytempl-watcher` ServiceAccount. If it fails to start (RBAC, API version mismatch, network issue) the `/api/events` endpoint is disabled and the server logs:

```text
failed to start watcher, SSE will be unavailable
```

**Fix**: the `cozytempl-watcher` SA needs `list,watch` on `helmreleases.helm.toolkit.fluxcd.io` at the cluster scope. The Helm chart's `rbac.create: true` (default) renders this automatically in every mode except `dev`. If you turned off `watcher.enabled` or set `rbac.create: false`, re-enable them or provision the role yourself:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cozytempl-watcher
rules:
  - apiGroups: ["helm.toolkit.fluxcd.io"]
    resources: ["helmreleases"]
    verbs: ["list", "watch"]
```

## Is the SSE stream supposed to reconnect periodically?

Yes, by design. cozytempl caps every SSE connection at 60 minutes. When the cap fires, the handler returns cleanly, the browser's `EventSource` sees the connection close and reconnects automatically with `Last-Event-ID`. The new request re-runs `RequireAuth`, which refreshes the OIDC ID token if it is close to expiring, and the watcher's ring buffer replays any events that fired during the momentary disconnect. You should not see any missed updates.

If the reconnect itself fails (browser stuck in "connecting" for more than a few seconds, UI live-updates stop), check:

1. The pod logs for `SSE subscribe denied` — the user's RBAC changed.
2. The pod logs for `oidc refresh failed; clearing session` — the user's Keycloak session or refresh token was revoked and they need to log in again.
3. The ingress path timeout. Some proxies close idle long-lived connections below 60 minutes; set a longer `proxy_read_timeout` (nginx) or equivalent.

## Passthrough mode, but every k8s call returns `Unauthorized`

The API server is not configured to accept the issuer your ID token comes from. See the migration guide's [verification section](migrating-to-passthrough-auth.md#1-verify-the-k8s-api-server-is-oidc-configured) for the exact apiserver flags to check.

Other possibilities:

- The `aud` claim on cozytempl's ID token does not contain `kubernetes`. Check the Keycloak `cozytempl` client's audience mapper (migration guide step 2).
- A sidecar proxy or ingress is stripping `Authorization` headers before the request reaches cozytempl's upstream connection. Check the proxy config.
- The ID token has expired and the refresh loop failed silently. Look for `oidc refresh failed` in the pod logs.

## BYOK: "kubeconfig upload rejected: exec plugins not supported"

Your kubeconfig's current-context user is configured with an `exec` block (typically `aws-iam-authenticator`, `gke-gcloud-auth-plugin`, or similar). Exec plugins require an interactive shell cozytempl cannot provide.

**Fix**: generate a static bearer token and reference it directly.

```bash
# GKE
gcloud container clusters get-credentials <cluster> --region <region>
TOKEN=$(kubectl create token my-user --duration=24h)
# Then hand-edit ~/.kube/config so the user block reads:
#   user:
#     token: <TOKEN>
# instead of the exec plugin.
```

Then re-upload.

## BYOK: "kubeconfig too large"

The upload limit is 32 KB (set by cookie-storage constraints). Strip unused contexts, clusters and users:

```bash
kubectl config view --minify --flatten \
  --context=<current-context> \
  > /tmp/minimal-kubeconfig.yaml
```

Upload the minimal file instead of your whole `~/.kube/config`.

## "Showing the first 500 applications" banner on the tenant page

The tenant you're looking at has more than 500 applications, which is the current hard cap on `ApplicationService.List`. The filter and sort controls at the top of the table operate on the **current window only** — a filter that matches 3 of 500 shown apps may miss a match that exists in the pages cozytempl didn't fetch.

**Fix**: split the tenant into sub-tenants so each one has a manageable inventory. The cap exists specifically to prevent a pathological tenant from hanging the UI; growing past it is a signal that the tenant is doing too much.

## Dev mode banner visible on production

You shipped `COZYTEMPL_AUTH_MODE=dev` (or the legacy `COZYTEMPL_DEV_MODE=true`) to a real deployment. This is dangerous — auth is disabled, every request is `dev-admin` with `system:masters`, and nothing gates API access to the cluster.

**Fix immediately**: set `COZYTEMPL_AUTH_MODE=passthrough` (or `impersonation-legacy` if your apiserver isn't OIDC-configured), unset `COZYTEMPL_DEV_MODE`, restart the pod, and audit the logs between the moment dev mode went live and now:

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
