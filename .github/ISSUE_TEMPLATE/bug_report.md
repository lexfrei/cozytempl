---
name: Bug Report
about: Report a bug or unexpected behaviour
title: '[BUG] '
labels: bug
assignees: ''
---

## Description

A clear and concise description of what the bug is.

## Steps to Reproduce

1. Deploy cozytempl with '...'
2. Log in as '...' / upload kubeconfig '...'
3. Navigate to '...'
4. See error

## Expected Behaviour

What you expected to happen.

## Actual Behaviour

What actually happened. If the UI showed an error toast, include the
full text. If the handler returned a 500, check the pod logs.

## Environment

- **cozytempl version**: [e.g., 0.0.7 / commit sha]
- **Auth mode**: [passthrough / byok / impersonation-legacy / dev]
- **Kubernetes version**: [e.g., v1.31.0]
- **cozystack version**: [e.g., latest / 1.3]
- **Deployment method**: [Helm chart / manual manifest / local binary]
- **Browser**: [Safari 17 / Chrome 120 / Firefox 120]
- **Locale**: [en / ru / kk / zh]

## Pod logs

<details>
<summary>cozytempl pod logs</summary>

```text
Paste the output of:
  kubectl --context <ctx> --namespace <ns> logs deploy/cozytempl --tail=200
```

</details>

## Request correlation ID

If the bug is tied to a specific HTTP request, grab the `X-Request-ID`
response header from the browser network tab and paste it here. Every
audit-log and access-log entry carries the same ID so we can trace the
whole request path server-side.

```text
X-Request-ID: ...
```

## Additional Context

Screenshots, Helm values snippet, OIDC provider / Keycloak client
config — anything else that helps reproduce.
