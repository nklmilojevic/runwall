.PHONY: generate build test run demo tunnel image

generate:
	go tool templ generate

build: generate
	go build -o bin/runwall ./cmd/runwall

test: generate
	go test ./...

# Secrets are injected by 1Password; nothing is written to disk.
OP_ENV ?= secrets.op
run: build
	op run --env-file=$(OP_ENV) -- ./bin/runwall

demo: build
	./bin/runwall --demo

# Exposes only /webhook; see deploy/cloudflared/config.example.yml.
tunnel:
	cloudflared tunnel --config deploy/cloudflared/config.yml run

image:
	docker build -t runwall .
