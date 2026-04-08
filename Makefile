.PHONY: generate build test dev lint clean install-tools ts

# Generate templ output
generate:
	templ generate

# Bundle TypeScript
ts:
	esbuild static/ts/main.ts --bundle --outfile=static/dist/bundle.js --minify --sourcemap

# Run all tests
test: generate
	go test ./... -count=1 -race

# Run linters (Go + TypeScript)
lint: generate
	golangci-lint run
	npx eslint static/ts/

# Build binary (embeds static assets)
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
