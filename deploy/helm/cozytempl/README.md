# cozytempl Helm chart

Minimal Helm chart for cozytempl — the Cozystack management UI.

## TL;DR

```bash
# Dev mode, no OIDC, auth disabled, running anywhere:
helm install cozytempl deploy/helm/cozytempl \
  --namespace cozy-system --create-namespace \
  --set config.devMode=true

# Production: wire up OIDC and a session key you generated yourself.
helm install cozytempl deploy/helm/cozytempl \
  --namespace cozy-system --create-namespace \
  --set config.oidc.issuerURL=https://keycloak.example.com/realms/cozystack \
  --set config.oidc.clientID=cozytempl \
  --set config.oidc.redirectURL=https://cozy.example.com/auth/callback \
  --set config.oidc.clientSecret="$(pass show keycloak/cozytempl/client-secret)" \
  --set config.oidc.sessionSecret="$(openssl rand -base64 32)"
```

## What you get

| Resource | Purpose |
| --- | --- |
| `Deployment` | 1 replica by default, runs the cozytempl binary. |
| `Service` | `ClusterIP` on port 8080. Use an Ingress/Gateway for external access. |
| `ServiceAccount` | Identity cozytempl uses to talk to the k8s API. |
| `ClusterRole` + `ClusterRoleBinding` | `impersonate` on users/groups/serviceaccounts and `create` on self-subject reviews. That's the whole RBAC footprint — every other k8s call runs with the logged-in user's impersonated identity. |
| `Secret` | OIDC client secret + session signing key. Skipped entirely if `config.devMode=true` or `config.existingSecret` is set. |

## Notes

- **Dev mode is dangerous.** It disables auth and treats every request as `dev-admin` with `system:masters`. Only enable in a cluster you fully control. The UI renders a loud red banner whenever it's on, but that's client-side — the real gate is your network boundary.
- **OIDC redirect URL.** Must match what you configured in the OIDC provider byte-for-byte, including scheme and trailing slash. Mismatches show up as a 400 on `/auth/callback`.
- **Session secret.** 32+ random bytes. Use `openssl rand -base64 32` or similar. Rotate by updating the Secret and rolling the Deployment — existing sessions are invalidated by design.
- **Ingress is out of scope.** This chart deliberately does not render an Ingress resource. Infrastructure teams have strong opinions about their ingress controller / Gateway API flavour; the chart stays neutral and ships a plain Service. Point your Ingress at `svc/<release>-cozytempl:8080`.
- **Metrics.** `/metrics` is served on the same pod port and is unauthenticated. Protect it with a NetworkPolicy or service mesh policy so only your Prometheus scraper can reach it.
- **Impersonation RBAC is required.** The ClusterRole in this chart is the minimum; your cluster's OIDC users also need whatever RBAC they'd normally need to see tenants and apps. cozytempl cannot grant access the underlying user doesn't already have.

## Values

See [`values.yaml`](values.yaml) — every field is commented.
