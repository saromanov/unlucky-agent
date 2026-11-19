BIN := bin/unlucky-agent
PKG := ./...
VERSION ?= 0.1.0-dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race vet fmt lint ci run-mvp clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/unlucky-agent

test:
	go test $(PKG)

race:
	go test -race $(PKG)

vet:
	go vet $(PKG)

fmt:
	gofmt -l -w .

lint: fmt vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt: файлы не отформатированы: $$(gofmt -l .)"; exit 1)

ci: lint test build

run-mvp: build
	./$(BIN) scenarios validate -dir scenarios
	./$(BIN) run -all -dir scenarios -baseline cp-r -out out/cp-r
	./$(BIN) run -all -dir scenarios -baseline none -out out/none

clean:
	rm -rf bin out
