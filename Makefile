BINARY := bin/feature-flag-service
PKGS   := ./...

.PHONY: run build test test-race integration ci cover lint fmt vet tidy clean

run:
	go run ./cmd/server

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

test:
	go test $(PKGS)

test-race:
	go test -race -cover $(PKGS)

# ci runs what .github/workflows/ci.yml runs, so a green 'make ci' predicts a green pipeline.
ci: lint
	go mod tidy && git diff --exit-code -- go.mod go.sum
	go mod verify
	go test -race -shuffle=on -count=1 -covermode=atomic -coverprofile=coverage.out $(PKGS)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /dev/null ./cmd/server
	./sample_test/integration_test.sh

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
