GO ?= $(shell if command -v go >/dev/null 2>&1; then command -v go; elif [ -x /usr/local/go/bin/go ]; then printf '%s' /usr/local/go/bin/go; elif [ -x /opt/homebrew/bin/go ]; then printf '%s' /opt/homebrew/bin/go; elif [ -x /tmp/go/bin/go ]; then printf '%s' /tmp/go/bin/go; else printf '%s' go; fi)
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST := dist
PKG := ./cmd/gpt-codex-router

.PHONY: fmt test vet build build-all docker-build docker-up docker-down docker-logs clean

fmt:
	$(GO) fmt ./...

test:
	CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

build:
	mkdir -p $(DIST)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/gpt-codex-router $(PKG)

build-all: test vet
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/gpt-codex-router-linux-amd64 $(PKG)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/gpt-codex-router-darwin-arm64 $(PKG)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/gpt-codex-router-darwin-amd64 $(PKG)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/gpt-codex-router-windows-amd64.exe $(PKG)

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t gpt-codex-router:local .

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f --tail=100 gpt-codex-router

clean:
	rm -rf $(DIST)
