# Pull Request

## Summary

Brief description of what this PR does and why.

## Changes

- List the key changes in this PR
- Focus on what changed and why, not how — the diff shows how
- Keep it high-level and readable

## Testing

- [ ] `make test` passes locally (Go tests with `-race` + helm-unittest)
- [ ] `make lint` passes locally (`golangci-lint` + `govulncheck` + `eslint`)
- [ ] `make build` produces a working binary + TS bundles
- [ ] Manual UI verification in a browser (if the change is user-visible)
- [ ] Cross-locale check (if the change touches i18n)

## Documentation

- [ ] Top-level `README.md` updated (if user-facing behaviour changed)
- [ ] `docs/` updated (if auth / ops / troubleshooting surface changed)
- [ ] Chart `values.yaml` comments updated (if chart values changed)
- [ ] `values.schema.json` updated (if chart values changed)
- [ ] Code comments added for non-obvious logic

## Helm chart (if touched)

- [ ] `helm-unittest` coverage added for new templates / values paths
- [ ] `values.schema.json` additionalProperties stays `false`
- [ ] `helm lint` clean in every auth mode (dev, passthrough, byok, legacy)

## i18n (if touched)

- [ ] New message IDs present in all four locales (en, ru, kk, zh)
- [ ] IDs follow the existing dot-namespace convention
- [ ] Russian / Kazakh / Chinese translations are natural, not machine-literal

## Checklist

- [ ] Commit messages follow semantic format (`type(scope): description`)
- [ ] Commits are signed (GPG) and carry `Signed-off-by`
- [ ] No secrets, tokens, or credentials in code or CI logs
- [ ] Breaking changes documented (README + migration notes)
- [ ] Related issues referenced (if any)

## Additional Notes

Any additional context reviewers should know — screenshots for UI changes,
before/after curl output for handler changes, etc.
