# Harnext agent build. Release builds are plain trimmed Go builds; anyone can repeat them.
GO ?= go
VERSION ?= dev
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
# Release inputs: public keys as "kid:base64pub[,kid:base64pub]" and the default server URL.
TRUSTED_KEYS ?=
SERVER_URL ?=

MODULE := github.com/harnext/harnext-agent
BIN := bin
REL_BIN := $(BIN)/release/harnext-agent-$(GOOS)-$(GOARCH)
RELEASE_LDFLAGS := -s -w -X main.version=$(VERSION) \
	-X $(MODULE)/internal/remoteconfig.trustedKeys=$(TRUSTED_KEYS) \
	-X $(MODULE)/internal/service.defaultServerURL=$(SERVER_URL)

.PHONY: build test check release clean

build:
	$(GO) build -o $(BIN)/harnext-agent -ldflags "-X main.version=$(VERSION)" ./cmd/agent

# Single gate: formatting, vet (also the realdata tag), race tests.
test:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	$(GO) vet ./...
	$(GO) vet -tags realdata ./...
	$(GO) test -race -count=1 ./...

check: test

release:
	@test "$(VERSION)" != "dev" || { echo "set VERSION=x.y.z"; exit 1; }
	@test -n "$(TRUSTED_KEYS)" || { echo "set TRUSTED_KEYS=kid:base64pub (without it friction scores stay null)"; exit 1; }
	$(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(REL_BIN) ./cmd/agent

clean:
	rm -rf $(BIN)
