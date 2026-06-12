GO ?= /usr/local/go/bin/go
BIN := fanctl
PKG := ./cmd/fanctl

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build vet test fmt tidy snapshot clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

# Local release artifacts (.deb/.rpm/.apk/tarballs) without publishing.
snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -f $(BIN)
	rm -rf dist
