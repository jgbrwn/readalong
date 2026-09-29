.PHONY: build test run fmt vet doctor install-service install-litestream-service

build:
	mkdir -p bin
	go build -o bin/readalong ./cmd/readalong

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

run:
	go run ./cmd/readalong

doctor:
	./scripts/doctor.sh

install-service:
	./scripts/install-service.sh

install-litestream-service:
	./scripts/install-litestream-service.sh
