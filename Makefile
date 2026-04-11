.PHONY: generate build test dev lint clean install-tools ts

# Generate templ output
generate:
	templ generate

# Bundle TypeScript
ts:
	esbuild static/ts/main.ts --bundle --outfile=static/dist/bundle.js --minify --sourcemap

# Run all tests. Exclude node_modules because one npm package ships a stray
# Go file that Go tooling would otherwise pick up.
test: generate
	go test ./cmd/... ./internal/... ./static/... -count=1 -race

# Run linters (Go + TypeScript). Same scope as test — avoid scanning
# node_modules for Go code.
lint: generate
	golangci-lint run ./cmd/... ./internal/... ./static/...
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
	npm install --save-dev eslint @typescript-eslint/eslint-plugin @typescript-eslint/parser typescript
