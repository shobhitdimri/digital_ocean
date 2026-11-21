BINARY := bin/feature-flag-service
PKGS   := ./...

.PHONY: run build test test-race integration cover lint fmt vet tidy clean

run:
	go run ./cmd/server

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

test:
	go test $(PKGS)

test-race:
	go test -race -cover $(PKGS)

integration:
	./sample_test/integration_test.sh

cover:
	go test -race -coverprofile=coverage.out $(PKGS)
	go tool cover -func=coverage.out

fmt:
	gofmt -s -w .

vet:
	go vet $(PKGS)

lint: vet
	@test -z "$$(gofmt -s -l .)" || (echo "gofmt needed on:"; gofmt -s -l .; exit 1)

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out
