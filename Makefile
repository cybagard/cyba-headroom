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

.PHONY: build test test-host test-tart darwin-tests test-hooks hooks lint lint-docs fmt tidy clean dc-image dc-shell

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

# Git hooks run on the host (they read Orca and the local scrub list). The
# hook and its list of public names are copied, not linked or set via
# core.hooksPath, so a checked-out branch cannot change what runs or what
# passes; re-run after changing scripts/hooks.
hooks:
	@d=$$(git rev-parse --git-common-dir)/hooks; mkdir -p $$d; \
	install -m 0755 scripts/hooks/pre-commit $$d/pre-commit; \
	install -m 0755 scripts/hooks/pre-commit $$d/commit-msg; \
	install -m 0644 scripts/hooks/public-names $$d/public-names; \
	git config --unset core.hooksPath || true; \
	echo "installed pre-commit, commit-msg and public-names into $$d"

test-hooks:
	scripts/hooks/pre-commit_test.sh

# Lint as darwin too: CI runs on macOS, and darwin-only files are invisible
# to a Linux lint.
lint: $(DC_DEP)
	$(RUN) go vet ./...
	$(RUN) golangci-lint run
	$(RUN) env GOOS=darwin go vet ./...
	$(RUN) env GOOS=darwin golangci-lint run

# Docs lint runs in the official markdownlint-cli2 and Vale images, not the
# devcontainer. Configs: .markdownlint-cli2.yaml, .vale.ini.
MDLINT_IMAGE := davidanson/markdownlint-cli2:v0.23.3@sha256:d5f3f3f04b2e285dcbcdcd13b4454d119e273e3c393a9dabd163dba4abad526d
VALE_IMAGE := jdkato/vale:v3.23.0@sha256:d87d6355dc8992f92ec39c4c862a388e56e30302a771fd4512c02660fb25cdf3
DOCS := README.md SECURITY.md CONTRIBUTING.md
DOCS_RUN := docker run --rm -v "$(CURDIR)":/src -w /src --user $(shell id -u):$(shell id -g) -e HOME=/tmp

lint-docs:
	$(DOCS_RUN) --entrypoint markdownlint-cli2 $(MDLINT_IMAGE)
	$(DOCS_RUN) $(VALE_IMAGE) sync
	$(DOCS_RUN) $(VALE_IMAGE) $(DOCS)

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
