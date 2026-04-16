.PHONY: generate build test test-ts dev lint clean install-tools ts helm-test

# Generate templ output
generate:
	templ generate

# Bundle TypeScript. Two entry points:
#   - main.ts  → static/dist/bundle.js (deferred; everything else)
#   - theme-early.ts → static/dist/theme-early.js (non-deferred,
#     loaded in <head> before the stylesheet so the stored
#     data-theme is applied before first paint and no flash of
#     the wrong theme leaks onto the screen).
ts:
	esbuild static/ts/main.ts --bundle --outfile=static/dist/bundle.js --minify --sourcemap
	esbuild static/ts/theme-early.ts --bundle --outfile=static/dist/theme-early.js --minify

# Run all tests. Exclude node_modules because one npm package ships a stray
# Go file that Go tooling would otherwise pick up. Also runs the Helm chart
# unit tests if the helm-unittest plugin is installed — silently skipped
# otherwise so CI that doesn't have it yet stays green.
test: generate test-ts
	go test ./cmd/... ./internal/... ./static/... -count=1 -race
	@helm plugin list 2>/dev/null | grep -q unittest && helm unittest deploy/helm/cozytempl || echo "helm-unittest plugin not installed; skipping chart tests"

# TypeScript tests run under bun. Bun is the stated preferred
# runtime for TS work in this repo; it requires zero setup and
# reads bun:test out of the imports. Kept as its own target so
# contributors without bun can run `go test` alone without
# tripping over the TS side, but `make test` runs both so CI
# catches divergence between the Go and TS humanisers in the
# live-age column.
test-ts:
	@if command -v bun >/dev/null 2>&1; then \
		bun test static/ts/; \
	else \
		echo "bun not found on PATH; skipping TypeScript tests"; \
	fi

# Run just the Helm chart unit tests.
helm-test:
	helm unittest deploy/helm/cozytempl

# Run linters (Go + TypeScript). Same scope as test — avoid scanning
# node_modules for Go code. govulncheck runs against the Go module
# graph and fails the build on any known CVE in the vendored deps.
lint: generate
	golangci-lint run ./cmd/... ./internal/... ./static/...
	govulncheck ./cmd/... ./internal/...
	npx eslint static/ts/

# Build binary (embeds static assets). Same scoping rationale — package
# list explicit so node_modules is never linked into the binary.
build: generate ts
	CGO_ENABLED=0 go build -o bin/cozytempl ./cmd/cozytempl

# Dev mode with live reload
dev:
	air

# Clean build artifacts
clean:
	rm -rf bin/ tmp/ static/dist/
	find . -name '*_templ.go' -delete

# Install development tools
install-tools:
	go install github.com/a-h/templ/cmd/templ@latest
	go install github.com/air-verse/air@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	helm plugin install https://github.com/helm-unittest/helm-unittest 2>/dev/null || true
	npm install --save-dev eslint @typescript-eslint/eslint-plugin @typescript-eslint/parser typescript
