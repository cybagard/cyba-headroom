VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/cybagard/cyba-headroom/internal/cli.Version=$(VERSION)

# Every target runs inside the devcontainer (.devcontainer/). Inside it, or with
# NATIVE=1 (CI on macOS runners), commands run directly.
DC_IMAGE := headroom-dev
ifneq ($(HEADROOM_DEVCONTAINER)$(NATIVE),)
RUN :=
DC_DEP :=
else
RUN := docker run --rm -i -v "$(CURDIR)":/src -w /src -v headroom-go:/go \
	--user $(shell id -u):$(shell id -g) $(DC_IMAGE)
DC_DEP := dc-image
endif

# The host is darwin/arm64; build for it wherever the build runs.
BUILD_ENV := GOOS=darwin GOARCH=arm64 CGO_ENABLED=0

.PHONY: build test test-host test-tart darwin-tests lint fmt tidy clean dc-image dc-shell

build: $(DC_DEP)
	$(RUN) env $(BUILD_ENV) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/headroom ./cmd/headroom

test: $(DC_DEP)
	$(RUN) go test -race ./...

# darwin test binaries, compiled in the devcontainer and run on a Mac.
darwin-tests: $(DC_DEP)
	$(RUN) scripts/darwin-tests.sh build

test-host: darwin-tests
	scripts/darwin-tests.sh run

test-tart: darwin-tests
	scripts/tart-test.sh

lint: $(DC_DEP)
	$(RUN) go vet ./...
	$(RUN) golangci-lint run

fmt: $(DC_DEP)
	$(RUN) gofmt -s -w .

tidy: $(DC_DEP)
	$(RUN) go mod tidy

clean:
	rm -rf bin

dc-image:
	docker build -q -t $(DC_IMAGE) .devcontainer >/dev/null

dc-shell: dc-image
	docker run --rm -it -v "$(CURDIR)":/src -w /src -v headroom-go:/go \
		--user $(shell id -u):$(shell id -g) $(DC_IMAGE) bash
