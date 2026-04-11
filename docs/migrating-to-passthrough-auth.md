# Migrating to passthrough auth

This guide walks an existing cozytempl deployment from `impersonation-legacy` (the only mode prior to this refactor) to `passthrough`. The goal is to remove cozytempl's cluster-wide `impersonate` permission and have the k8s API server validate user OIDC tokens directly.

Rollback is a single Helm value change — see the final section.

## 1. Verify the k8s API server is OIDC-configured

cozytempl's `passthrough` mode forwards user ID tokens to the API server as `Authorization: Bearer`. The API server must be told to accept tokens from the same issuer.

Stock cozystack already ships this — verify with:

```bash
kubectl --context your-cluster -n kube-system get pod \
  -l component=kube-apiserver -o yaml \
  | grep -E 'oidc-issuer-url|oidc-client-id|oidc-username-claim|oidc-groups-claim'
```

Expected flags (or their equivalents in your distro's config):

```text
--oidc-issuer-url=https://keycloak.example.com/realms/cozystack
--oidc-client-id=kubernetes
--oidc-username-claim=preferred_username
--oidc-groups-claim=groups
```

If any are missing, stop here. You cannot use `passthrough` until the API server accepts OIDC tokens. Either add the flags via your control-plane provisioner (cozystack, kubeadm, Talos, RKE2, etc.), or stay on `impersonation-legacy` one more release.

## 2. Create a dedicated `cozytempl` Keycloak client

The k8s API server is configured with `--oidc-client-id=kubernetes`, so it accepts tokens whose `aud` claim contains `kubernetes`. We create a new Keycloak client for cozytempl and attach an audience mapper so **its** tokens also land in the `kubernetes` audience. No token exchange is needed at runtime.

1. Keycloak admin UI → `cozystack` realm → Clients → Create.
   - Client ID: `cozytempl`.
   - Client type: `OpenID Connect`.
   - Next: `Client authentication: On`, `Authorization: Off`, `Standard flow: On`, `Direct access grants: Off`, `Service accounts roles: Off`, `OAuth 2.0 Device Authorization Grant: Off`, `OIDC CIBA Grant: Off`.
   - Next: `Valid redirect URIs: https://cozytempl.example.com/auth/callback`. `Web origins: https://cozytempl.example.com`. `Post Logout Redirect URIs: https://cozytempl.example.com/`.
   - Save.

2. On the new `cozytempl` client:
   - Credentials tab → copy the **Client secret** — you will paste it into `config.oidc.clientSecret` in step 3.

3. Add the audience mapper.
   - Client Scopes tab on the `cozytempl` client → click the dedicated scope line (`cozytempl-dedicated`) → Mappers → Add mapper → By configuration → `Audience`.
   - Name: `kubernetes-audience`.
   - Included Client Audience: `kubernetes`.
   - Add to ID token: `On`.
   - Add to access token: `On`.
   - Save.

4. Ensure the `kubernetes-client` (or equivalent) client scope is a **default** scope of the `cozytempl` client.
   - Client Scopes tab on `cozytempl` → Add client scope → pick `kubernetes-client` (whatever scope the cozystack platform docs say adds the `groups` claim in the k8s-expected shape) → Add → Default.
   - Without this, users log in successfully but end up without any `groups` claim on the k8s side and every RBAC check returns 403.

5. Ensure `offline_access` is enabled so Keycloak issues refresh tokens.
   - Client Scopes tab on `cozytempl` → verify `offline_access` is in Default or Optional scopes → Save.

6. Verify what Keycloak actually hands out.
   - Log in as a test user at `https://keycloak.example.com/realms/cozystack/account`.
   - Use Keycloak's "Evaluate" UI on the `cozytempl` client (Client Scopes → Evaluate) to produce a sample token.
   - Decode the payload (`jwt.io`, or `cut -d'.' -f2 | base64 -d`) and confirm:
     - `"aud": ["kubernetes", ...]`
     - `"groups": ["cozy-admin", ...]` matching the ClusterRoleBindings you already configured for those groups.

## 3. (Optional but recommended) internal issuer URL

In production the cozytempl pod typically runs in the same cluster as Keycloak. Token refresh goes through cozytempl's backend, not the browser, so making the backend talk to Keycloak through the cluster network avoids the ingress round-trip on every refresh.

Set:

```yaml
config:
  oidc:
    internalIssuerURL: "http://keycloak.cozy-keycloak.svc:8080/realms/cozystack"
```

User-facing redirects still use the external `issuerURL`. Leave `internalIssuerURL` empty if cozytempl runs outside the cluster or your Keycloak has no in-cluster Service.

## 4. Update Helm values

```yaml
config:
  authMode: passthrough          # was "impersonation-legacy" or unset
  oidc:
    issuerURL: https://keycloak.example.com/realms/cozystack
    internalIssuerURL: http://keycloak.cozy-keycloak.svc:8080/realms/cozystack  # optional
    clientID: cozytempl          # was typically "kubernetes"; now a dedicated client
    clientSecret: <paste from step 2>
    redirectURL: https://cozytempl.example.com/auth/callback
    sessionSecret: <keep your existing value>
```

If you use `config.existingSecret`, update the underlying Secret's `oidc-client-secret` key before rolling the Deployment.

## 5. Upgrade

```bash
helm upgrade cozytempl deploy/helm/cozytempl \
  --namespace cozy-system \
  --values your-values.yaml
```

The chart renders:

- **No** ClusterRole for the main cozytempl SA (the old impersonation role is removed on the next `helm upgrade`).
- A new `cozytempl-watcher` ServiceAccount with a ClusterRole granting only `list`, `watch` on `helmreleases.helm.toolkit.fluxcd.io`.
- `COZYTEMPL_AUTH_MODE=passthrough` in the Deployment env.
- `OIDC_INTERNAL_ISSUER_URL` if you set it.

## 6. Verify

1. Log in via the normal cozytempl URL. Keycloak redirects back to the dashboard.
1. Hit the Profile page. The auth-mode section should say `passthrough` and describe the OIDC-Bearer flow.
1. Create a test tenant. In the audit log, the record should carry `auth_mode=passthrough`:

    ```bash
    kubectl --namespace cozy-system logs deploy/cozytempl \
      | jq -c 'select(.audit == "event") | {action, actor, auth_mode}' \
      | head
    ```

1. Check the k8s API server audit log (if you have one). The `user.username` for cozytempl-originated requests should now be the real OIDC username, not `system:serviceaccount:cozy-system:cozytempl`.

## 7. Rollback

If anything goes wrong, flip back to legacy with a single Helm upgrade. The deprecated ClusterRole is recreated; no data is lost.

```bash
helm upgrade cozytempl deploy/helm/cozytempl \
  --namespace cozy-system \
  --set config.authMode=impersonation-legacy \
  --reuse-values
```

Users stay logged in because the session cookie format is backwards-compatible.

## Common failure modes

| Symptom | Likely cause |
| --- | --- |
| Login redirects to Keycloak but the dashboard shows `401` from the k8s API | k8s API server is not OIDC-configured, or is configured with a different issuer URL. Re-check step 1. |
| Dashboard loads but every tenant list is empty and kube audit logs show `system:anonymous` | Token is being stripped somewhere between cozytempl and the API server. Check for a sidecar proxy rewriting headers. |
| User logs in fine but all RBAC checks return `403` | The `groups` claim is missing or malformed in the ID token. Re-check step 2 (the `kubernetes-client` scope or equivalent groups mapper). |
| Every k8s call returns `Unauthorized: the server has asked for the client to provide credentials` after ~15 minutes | Keycloak is not issuing refresh tokens (missing `offline_access` scope), or Keycloak rotated the refresh token and cozytempl did not persist the new one. Re-check step 2.5 and the pod logs for `oidc refresh failed`. |
| SSE stream disconnects every hour with an audit line `SSE client disconnected` | This is by design — the 60-minute stream cap. The browser reconnects automatically and the watcher replays missed events. Only a problem if the reconnect itself fails (see previous row). |
| `kubeconfig upload rejected: exec plugins not supported` | BYOK mode specific. Regenerate the kubeconfig with `kubectl create token --duration=24h` and reference the resulting token directly instead of going through an exec plugin. |
