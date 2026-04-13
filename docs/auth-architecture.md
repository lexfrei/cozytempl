# Authentication architecture

cozytempl supports five authentication modes, selected at install time via `COZYTEMPL_AUTH_MODE` (Helm: `config.authMode`). Each mode shapes both the user-facing login flow and the k8s RBAC the cozytempl Helm chart installs. The right choice depends on where cozytempl runs, who operates the cluster, and what trust boundary you need.

## Mode comparison

| Aspect | passthrough | byok | token | dev | impersonation-legacy |
| --- | --- | --- | --- | --- | --- |
| User identity source | OIDC (Keycloak, Dex, ...) | User-uploaded kubeconfig | User-pasted Bearer token | Hard-coded `dev-admin` + `system:masters` | OIDC |
| Credential forwarded to k8s | Raw ID token as `Authorization: Bearer` | Everything in the uploaded kubeconfig | Pasted token as `Authorization: Bearer` | Process's own kubeconfig | `Impersonate-User` / `Impersonate-Group` headers |
| cozytempl pod SA RBAC | None | None | None | None | Cluster-wide `impersonate` on users/groups/serviceaccounts |
| Watcher SA RBAC | `list`, `watch` on `helmreleases` only | Same | Same | Not rendered | Same |
| Who evaluates RBAC | k8s API server, as the real user | k8s API server, as whoever the kubeconfig identifies | k8s API server, as whoever the token identifies | Whatever identity the local kubeconfig presents | k8s API server, as the impersonated user |
| IdP required | Yes (OIDC) | No | No | No | Yes (OIDC) |
| k8s API server OIDC-configured? | Required | Not required | Not required | Not required | Not required |
| Safe as internet-facing | Yes | Yes (per-session kubeconfig) | Yes (per-session token) | No | Risky — single point of trust |

## What an attacker gets from RCE in cozytempl

| Mode | Blast radius of a compromised cozytempl process |
| --- | --- |
| passthrough | Only the tokens of currently-logged-in users held in session cookies, for as long as each token's `exp` is valid (≤ 1 hour with the default ID token TTL). The cozytempl ServiceAccount itself has zero k8s permissions — the attacker cannot impersonate anyone they do not already have a token for. |
| byok | The kubeconfigs of currently-logged-in users held in their encrypted session cookies. Same boundary as passthrough — the cozytempl SA has no cluster access of its own. |
| token | The Bearer tokens of currently-logged-in users held in their encrypted session cookies. Same boundary as passthrough and byok — the cozytempl SA has no cluster access of its own. Note that pasted tokens typically do NOT carry a short-lived `exp` claim the way OIDC ID tokens do; long-lived ServiceAccount tokens stay valid until explicitly deleted, so the blast-radius window is larger than passthrough unless operators rotate tokens proactively. |
| dev | **Full cluster access.** Dev mode by design has no authentication; every request, including the attacker's, is treated as `dev-admin` + `system:masters`. This mode is intended only for single-user local development. |
| impersonation-legacy | **Full cluster access.** The cozytempl SA holds cluster-wide `impersonate` on `users`, `groups`, and `serviceaccounts`. An attacker with RCE sets `Impersonate-User: system:admin` and has the cluster. This is why the mode is deprecated. |

## Token-upload validation probe

In both `byok` and `token` modes, the upload handler runs one `SelfSubjectAccessReview` (SAR) round-trip against the apiserver before the credential lands in the session cookie. The review itself uses a cheap dummy resource — the point is the round-trip, not the verdict — so an apiserver that accepts the credential returns 200 regardless of whether the user would actually be authorised for the queried verb.

This validation relies on the pasted credential being allowed to create a `SelfSubjectAccessReview`. On vanilla Kubernetes, the built-in `system:basic-user` ClusterRole grants this to every authenticated user, so the probe succeeds as long as the token is accepted at all. Distributions that strip or replace `system:basic-user` (unusual, but possible) will see legitimate uploads fail the probe with a 403 — operators in that situation either need to grant the equivalent right back, or accept that cozytempl will reject credentials their apiserver would otherwise allow.

## Token flow

### passthrough

```text
Browser ──(1) login redirect──▶ Keycloak
Browser ◀─(2) auth code──────── Keycloak
Browser ──(3) /auth/callback──▶ cozytempl
cozytempl ──(4) code→tokens──▶ Keycloak              (IDToken + RefreshToken returned)
cozytempl ──(5) set cookie──▶  Browser               (session stores IDToken + RefreshToken + exp)

Subsequent requests:
Browser ──(6) cookie────────▶  cozytempl
cozytempl ──(7) refresh if <60s to exp──▶ Keycloak   (in-middleware, transparent)
cozytempl ──(8) Authorization: Bearer <IDToken>──▶ k8s API
k8s API ──(9) validates token via OIDC JWKS──▶ real user identity
```

The k8s API server validates the forwarded ID token against Keycloak's JWKS endpoint on its own schedule — cozytempl itself does no second-layer JWKS check per request. This is the same validation path kubectl uses with an OIDC auth-provider.

### byok

```text
Browser ──(1) first visit───▶  cozytempl
cozytempl ──(2) redirect to /auth/kubeconfig──▶ Browser
Browser ──(3) upload kubeconfig────▶ cozytempl
cozytempl ──(4) validate (parse, reject exec plugins, test call)──▶ k8s API
cozytempl ──(5) encrypt + store in session cookie──▶ Browser

Subsequent requests:
Browser ──(6) cookie (carrying encrypted kubeconfig)──▶ cozytempl
cozytempl ──(7) build rest.Config from kubeconfig──▶ k8s API
```

BYOK validation rejects any kubeconfig whose current-context user has an `exec` or `auth-provider` section — those require an interactive shell cozytempl cannot provide. Size is capped at 32 KB so the cookie stays within browser limits.

### token

```text
Browser ──(1) first visit───▶  cozytempl
cozytempl ──(2) redirect to /auth/token──▶ Browser
Browser ──(3) paste Bearer token────▶ cozytempl
cozytempl ──(4) SelfSubjectAccessReview probe──▶ k8s API    (confirms apiserver accepts the token)
cozytempl ──(5) encrypt + store in session cookie──▶ Browser

Subsequent requests:
Browser ──(6) cookie (carrying encrypted token)──▶ cozytempl
cozytempl ──(7) Authorization: Bearer <token>──▶ k8s API
```

Token mode skips OIDC entirely — the apiserver treats the Bearer token the same way it treats any other (ServiceAccount token, client-certificate-backed token, OIDC ID token, etc.). Size is capped at 1.5 KB (the largest raw token that still fits a single gorilla/securecookie cookie after gob+encrypt+base64); a real-world Kubernetes ServiceAccount token is typically 900–1500 bytes so this is comfortable. Operators whose IdP mints larger tokens should use byok instead. The probe step is detailed in [Token-upload validation probe](#token-upload-validation-probe). A revoked token fails at the next k8s call after revocation, not at session-refresh time — sessions have no clock; log out and paste a fresh token if operations stop working after a rotation.

**Identity in logs.** Cozytempl does not decode the pasted Bearer token, so it has no way to distinguish two users who both paste valid tokens — every token-mode session gets the synthetic username `token-user` in audit log lines and the UI. The apiserver still sees the real identity (the SA or user the token belongs to) and evaluates RBAC against it, so authorisation is not affected; only cozytempl-level attribution is. The same design choice applies to byok (`kubeconfig-user`). Operators who need per-user attribution at the UI layer should prefer passthrough instead, where the OIDC claims carry a username.

### dev

```text
Browser ──▶ cozytempl ──(cozytempl's own kubeconfig, identity = local process)──▶ k8s API
```

A red DEV MODE banner is rendered on every page so an accidentally-exposed dev instance is impossible to miss.

### impersonation-legacy

```text
Browser ──(1-5 same as passthrough login)──▶ cozytempl

Subsequent requests:
Browser ──(6) cookie──▶ cozytempl
cozytempl ──(7) Impersonate-User, Impersonate-Group──▶ k8s API
                (cozytempl authenticates via its own SA token; k8s checks
                 that SA has impersonate on the requested user+groups)
```

## When to use which mode

- **passthrough**: the recommended default for production. Use when the k8s API server is already configured for OIDC (stock cozystack ships this). Delivers the smallest blast radius of any mode.
- **byok**: single-tenant deployments where the user already has a kubeconfig and there is no shared IdP — laptops, MSP engineers jumping between unrelated customer clusters, homelab setups without Keycloak. Pairs well with a secondary SSO at the ingress layer if needed.
- **token**: same situations as byok, but when all the user needs to forward to the apiserver is a single Bearer token — e.g. `kubectl create token` against a ServiceAccount — and assembling a full kubeconfig is friction they would rather skip. No IdP required. Prefer `passthrough` if the apiserver is OIDC-configured; prefer `byok` if the identity is backed by a client certificate rather than a Bearer token.
- **dev**: local development and CI snapshots. Never expose to a network an attacker can reach.
- **impersonation-legacy**: only if the k8s API server is not OIDC-configured and you cannot enable it. Tracked for removal two minor releases after passthrough ships.

## Cozystack compatibility

Stock cozystack Keycloak already configures the k8s API server with `--oidc-issuer-url`, `--oidc-client-id=kubernetes`, `--oidc-username-claim=preferred_username`, and `--oidc-groups-claim=groups`. The migration guide walks through adding a dedicated `cozytempl` Keycloak client with an audience mapper so its ID tokens are accepted by the `kubernetes` audience — no token exchange step, no changes to the apiserver flags.
