VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/cybagard/cyba-headroom/internal/cli.Version=$(VERSION)

.PHONY: build test lint fmt tidy clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/headroom ./cmd/headroom

test:
	go test -race ./...

lint:
	go vet ./...
	golangci-lint run

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

clean:
	rm -rf bin
