# cozytempl Helm chart

Helm chart for cozytempl — the Cozystack management UI.

> **Disclaimer.** cozytempl is a personal side project by a Cozystack
> contributor. It is **not** part of the upstream Cozystack project and
> **not** endorsed by Aenix. See the [top-level README](../../../README.md)
> for the full disclaimer.

## TL;DR

```bash
# Dev mode: no OIDC, auth disabled, running anywhere. DO NOT expose to a network
# you do not control — the UI renders a loud red banner but that is client-side.
helm install cozytempl oci://ghcr.io/lexfrei/charts/cozytempl \
  --version 0.1.0 \
  --namespace cozy-system --create-namespace \
  --set config.authMode=dev

# Passthrough (recommended production mode). The user's OIDC ID token is
# forwarded directly to the Kubernetes API; the cozytempl pod's SA has
# zero cluster permissions.
helm install cozytempl oci://ghcr.io/lexfrei/charts/cozytempl \
  --version 0.1.0 \
  --namespace cozy-system --create-namespace \
  --set config.authMode=passthrough \
  --set config.oidc.issuerURL=https://keycloak.example.com/realms/cozystack \
  --set config.oidc.clientID=cozytempl \
  --set config.oidc.redirectURL=https://cozy.example.com/auth/callback \
  --set config.oidc.clientSecret="$(pass show keycloak/cozytempl/client-secret)" \
  --set config.oidc.sessionSecret="$(openssl rand -base64 32)"
```

The chart lives both under `deploy/helm/cozytempl` in this repo (for
`helm install deploy/helm/cozytempl` workflows and `helm lint` in CI)
and on ghcr as an OCI artifact at `ghcr.io/lexfrei/charts/cozytempl`.
The OCI copy is published and signed by the release pipeline on every
`v*` git tag.

## Auth modes

| Mode | When | k8s RBAC on the cozytempl SA |
| --- | --- | --- |
| `passthrough` | Stock Cozystack. k8s API server trusts the OIDC issuer. | **None** — user's OIDC ID token is forwarded as a Bearer credential. |
| `byok` | Homelab, laptop, MSP engineer hopping between clusters with no shared IdP. | **None** — user uploads a kubeconfig, stored encrypted in the session cookie. |
| `token` | Same situations as `byok`, but when the user would rather paste a single Bearer token than assemble a full kubeconfig. | **None** — user pastes a Bearer token, stored encrypted in the session cookie. |
| `impersonation-legacy` | Deprecated. Only if the API server is not OIDC-configured and you cannot enable it. | Cluster-wide `impersonate` on users, groups, serviceaccounts. |
| `dev` | Single-user local testing. **Never production.** | None on the SA; the process uses its own kubeconfig. |

Threat model per mode in [`docs/auth-architecture.md`](../../../docs/auth-architecture.md).
Migration guide from impersonation-legacy in
[`docs/migrating-to-passthrough-auth.md`](../../../docs/migrating-to-passthrough-auth.md).

## Rendered resources

All resources except Deployment / Service / ServiceAccount are opt-in.
Everything is rendered per the active `authMode` so a mode change on
`helm upgrade` reshapes the cluster state automatically.

### Always

| Resource | Purpose |
| --- | --- |
| `Deployment` | Runs the cozytempl binary. Stateless; sessions live in cookies. |
| `Service` (ClusterIP 8080) | Pod endpoint. Point an Ingress / HTTPRoute at it. |
| `ServiceAccount` | Identity for the main pod. Zero-RBAC in passthrough / byok / token. |

### Mode-conditional

| Resource | Rendered when | Notes |
| --- | --- | --- |
| `ClusterRole` + `ClusterRoleBinding` (main SA) | `authMode=impersonation-legacy` | Grants `impersonate` on users / groups / serviceaccounts. Deprecated path. |
| `ServiceAccount` `cozytempl-watcher` + `ClusterRole` + `Binding` | `authMode ∈ {passthrough, byok, token, impersonation-legacy}` AND `watcher.enabled=true` (default) | Narrow `list,watch` on `helmreleases.helm.toolkit.fluxcd.io`. Drives the SSE stream. |
| `Secret` | `authMode ≠ dev` AND `existingSecret` unset | Holds `session-secret` always; `oidc-client-secret` only in passthrough / legacy. In byok / token modes only `session-secret` is rendered. |

### Opt-in (all `enabled: false` by default)

| Resource | Flag | Purpose |
| --- | --- | --- |
| `Ingress` (networking.k8s.io/v1) | `ingress.enabled` | Classic nginx / Traefik / HAProxy path. `className`, TLS, per-host path rules. |
| `HTTPRoute` (gateway.networking.k8s.io/v1) | `httpRoute.enabled` | Gateway API path. `rules[]` with per-rule `matches` / `filters` / `weight` for canary splits. |
| `NetworkPolicy` | `networkPolicy.enabled` | Default ingress allows 8080 from any cluster ns; default egress DNS + HTTPS. `ingress` / `egress` arrays append extra rules. |
| `PodDisruptionBudget` | `podDisruptionBudget.enabled` AND `replicaCount>1` | Double-guarded — a PDB on a single replica breaks node drains. Takes `minAvailable` or `maxUnavailable`. |
| `HorizontalPodAutoscaler` (autoscaling/v2) | `autoscaling.enabled` | cozytempl is stateless so HPA is trivially safe. CPU + optional memory + custom metrics + behavior. |
| `ServiceMonitor` (monitoring.coreos.com/v1) | `serviceMonitor.enabled` | Prometheus Operator scrape CR. Targets `/metrics` on the existing http port; no separate metrics port needed. |
| `VerticalPodAutoscaler` (autoscaling.k8s.io/v1) | `vpa.enabled` | `updateMode: Auto` default. Do not combine with HPA on the same metric without a `resourcePolicy` exclusion. |

All seven opt-in templates are covered by `helm-unittest` suites with
per-flag cases (79 assertions across 11 suites today).

## Verify signatures (cosign)

The release pipeline signs both the chart and the container image via
cosign keyless through the GitHub OIDC id-token.

```bash
# Chart
cosign verify \
  ghcr.io/lexfrei/charts/cozytempl:0.1.0 \
  --certificate-identity-regexp=https://github.com/lexfrei/cozytempl \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com

# Container image
cosign verify \
  ghcr.io/lexfrei/cozytempl:0.1.0 \
  --certificate-identity-regexp=https://github.com/lexfrei/cozytempl \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com
```

Chart and image versions always move together on every release — the
pipeline pins `image.tag`, `Chart.yaml` `version` and `appVersion` all
to the stripped git tag.

## Notes

- **Dev mode is dangerous.** It disables authentication and every
  request is treated as `dev-admin` with `system:masters`. The UI
  renders a loud red banner whenever it is on, but that is client-side
  — the real gate is the network boundary.
- **OIDC redirect URL.** Must match what you configured in the
  provider byte-for-byte, including scheme and trailing slash.
  Mismatches surface as a 400 on `/auth/callback`.
- **Session secret.** 32+ random bytes. Rotate by updating the Secret
  and rolling the Deployment — existing sessions are invalidated by
  design.
- **Internal vs external issuer URL.** Set `config.oidc.internalIssuerURL`
  to a cluster-internal Keycloak service if you want the backend
  token-refresh path to stay inside the cluster network while the
  user-facing redirect keeps the external URL.
- **Metrics are unauthenticated.** `/metrics` shares the public HTTP
  port. Restrict access via `networkPolicy.enabled=true` with a
  scraper-scoped ingress rule, or an outer service-mesh policy.
- **The cozytempl pod cannot grant access its user doesn't already
  have.** Your OIDC-mapped users still need the RBAC they would need
  for `kubectl --as` to see tenants / apps / namespaces.

## Values

See [`values.yaml`](values.yaml) — every field has an inline comment.
`values.schema.json` at the chart root enforces strict
`additionalProperties: false` on every block so a typo fails
`helm install --dry-run` before it renders a silently-broken
Deployment.
