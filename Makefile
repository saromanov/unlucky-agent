BIN := bin/unlucky-agent
PKG := ./...
VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFUMPT := go run mvdan.cc/gofumpt@latest

.PHONY: all build test race vet fmt fmt-check lint cover ci run-mvp image-sandbox clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/unlucky-agent

test:
	go test -race $(PKG)

race:
	go test -race $(PKG)

vet:
	go vet $(PKG)

fmt:
	gofmt -l -w .
	$(GOFUMPT) -l -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt: файлы не отформатированы: $$(gofmt -l .)"; exit 1)
	@test -z "$$($(GOFUMPT) -l .)" || (echo "gofumpt: файлы не отформатированы: $$($(GOFUMPT) -l .)"; exit 1)

lint:
	golangci-lint run $(PKG)

cover:
	go test -race -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

ci: fmt-check vet lint test build

run-mvp: build
	./$(BIN) scenarios validate -dir scenarios
	./$(BIN) run -all -dir scenarios -baseline cp-r -out out/cp-r
	./$(BIN) run -all -dir scenarios -baseline none -out out/none

image-sandbox:
	docker build -t unlucky-agent-sandbox -f docker/Dockerfile.sandbox docker/

clean:
	rm -rf bin out coverage.out
