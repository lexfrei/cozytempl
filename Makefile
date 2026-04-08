.PHONY: generate build test dev lint clean install-tools

# Generate templ output
generate:
	templ generate

# Run all tests
test: generate
	go test ./... -count=1 -race

# Run linter
lint: generate
	golangci-lint run

# Build binary (embeds static assets)
build: generate
	CGO_ENABLED=0 go build -o bin/cozytempl ./cmd/cozytempl

# Dev mode with live reload
dev:
	air

# Clean build artifacts
clean:
	rm -rf bin/ tmp/
	find . -name '*_templ.go' -delete

# Install development tools
install-tools:
	go install github.com/a-h/templ/cmd/templ@latest
	go install github.com/air-verse/air@latest
