# Run `just` to list recipes. Enter the dev shell first with `nix develop` (or direnv).

# 1Password references for `just run`.
op_env := env("OP_ENV", "secrets.op")

default:
    @just --list

# Regenerate templ components.
generate:
    go tool templ generate

# Build bin/runwall.
build: generate
    go build -o bin/runwall ./cmd/runwall

# Run the tests with the race detector.
test: generate
    go test -race ./...

# Run golangci-lint.
lint:
    golangci-lint run ./...

# Format Go and templ files.
fmt:
    go tool templ fmt .
    gofmt -w .

# Run the same checks as CI.
check: generate
    git diff --exit-code -- '*_templ.go'
    test -z "$(gofmt -l .)"
    go vet ./...
    go test -race ./...

# Start with sample data on http://127.0.0.1:8080.
demo: build
    ./bin/runwall --demo

# Start with the real GitHub App; secrets come from 1Password.
run: build
    op run --env-file={{ op_env }} -- ./bin/runwall

# Expose only /webhook through Cloudflare Tunnel (see deploy/cloudflared/config.example.yml).
tunnel:
    cloudflared tunnel --config deploy/cloudflared/config.yml run

# Build the container image locally.
image tag="runwall:dev":
    docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t {{ tag }} .

# Render the Kubernetes manifests.
manifests:
    kubectl kustomize deploy/k8s
