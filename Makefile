.PHONY: build test test-playwright playwright-install vet run-mock loadtest redis-up redis-down

build:
	go build -o bin/harvester ./cmd/harvester
	go build -o bin/loadtest ./cmd/loadtest

test:
	go test ./...

# Browser tests for mode: playwright (local fixtures only). Needs
# `make playwright-install` first.
test-playwright:
	SERP_HARVESTER_PLAYWRIGHT=1 go test ./internal/fetcher/ ./internal/worker/ -run 'Playwright' -v

playwright-install:
	go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps chromium

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
