.PHONY: build test vet run-mock

build:
	go build -o bin/harvester ./cmd/harvester

test:
	go test ./...

vet:
	go vet ./...

run-mock: build
	./bin/harvester -config configs/config.example.yaml -mode mock
