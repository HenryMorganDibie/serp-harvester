.PHONY: build test test-playwright playwright-install integration e2e vet run-mock loadtest redis-up redis-down

build:
	go build -o bin/harvester ./cmd/harvester
	go build -o bin/loadtest ./cmd/loadtest
	go build -o bin/crawl ./cmd/crawl

test:
	go test ./...

# Browser tests for mode: playwright (local fixtures only). Needs
# `make playwright-install` first.
test-playwright:
	SERP_HARVESTER_PLAYWRIGHT=1 go test ./internal/fetcher/ ./internal/worker/ ./internal/crawl/ -run 'Playwright|Auto' -v

playwright-install:
	sh scripts/install-playwright.sh --with-deps

# Reproducible runs in Docker (isolated project, throwaway passwords):
# `integration` runs go test ./... with real Redis, PostgreSQL and Chromium;
# `e2e` runs the harvester-playwright image against the fixture site. Set
# EXTRA_CA_FILE when building behind a TLS-inspecting proxy.
integration:
	sh scripts/compose-test.sh integration

e2e:
	sh scripts/compose-test.sh e2e

vet:
	go vet ./...

run-mock: build
	./bin/harvester -config configs/config.example.yaml -mode mock

loadtest: build
	./bin/loadtest -n 100000 -concurrency 800

redis-up:
	docker compose up -d redis

redis-down:
	docker compose down
