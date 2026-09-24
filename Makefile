.PHONY: build test vet run-mock loadtest redis-up redis-down

build:
	go build -o bin/harvester ./cmd/harvester
	go build -o bin/loadtest ./cmd/loadtest

test:
	go test ./...

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
