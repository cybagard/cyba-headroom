# Build and test environment for headroom. Tests and lint run on Linux here;
# `make build` cross-compiles the darwin/arm64 binary the host runs.
FROM golang:1.27-bookworm

ARG GOLANGCI_LINT_VERSION=latest
RUN GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION} \
 && rm -rf /root/go /root/.cache

# /go is a named volume (module + build cache) shared by every run; make it
# writable for the host uid the Makefile runs as.
ENV GOPATH=/go \
    GOCACHE=/go/cache \
    GOLANGCI_LINT_CACHE=/go/lint-cache \
    HOME=/tmp \
    GOFLAGS=-buildvcs=false \
    HEADROOM_DEVCONTAINER=1
RUN mkdir -p /go/pkg /go/cache /go/lint-cache && chmod -R 0777 /go
