# stremio-server-go

BINARY      := stremio-server
MAIN        := ./cmd/stremio-server
DIST        := dist
VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS     := -s -w -checklinkname=0 \
	-X main.buildVersion=$(VERSION) \
	-X main.buildCommit=$(COMMIT) \
	-X main.buildDate=$(DATE)
GOFLAGS     := -trimpath -ldflags "$(LDFLAGS)"

# CGO is not required; disabling it makes every target cross-compile as pure Go.
export CGO_ENABLED := 0

.PHONY: all build run test race vet fmt fmt-check lint tidy clean smoke build-all swagger help

all: fmt-check vet lint test build

build: ## Build the binary for the host platform
	go build $(GOFLAGS) -o $(BINARY) $(MAIN)

run: build ## Build and run
	./$(BINARY)

test: ## Run tests (race detector, serial)
	go test -p 1 -race ./...

vet: ## go vet
	go vet ./...

fmt: ## Format (gofmt -s)
	gofmt -s -w .

fmt-check: ## Fail if not gofmt -s clean
	@test -z "$$(gofmt -s -l .)" || (echo "run 'make fmt'"; gofmt -s -l .; exit 1)

lint: ## golangci-lint (if installed)
	@command -v golangci-lint >/dev/null && golangci-lint run ./... || echo "golangci-lint not installed; skipping"

tidy: ## go mod tidy
	go mod tidy

swagger: ## Regenerate the OpenAPI (Swagger 2.0) spec from code annotations (needs swaggo/swag)
	swag init -g cmd/stremio-server/main.go -o docs --outputTypes yaml,json --parseInternal

smoke: build ## Run the end-to-end API smoke test
	./scripts/smoke.sh

clean: ## Remove build artifacts
	rm -rf $(BINARY) $(DIST)

# Cross-compile every published target (pure Go, CGO disabled).
# android/arm64 needs -checklinkname=0 (already in LDFLAGS) for github.com/wlynxg/anet on Go 1.23+.
build-all: ## Cross-build all release targets into dist/
	@mkdir -p $(DIST)
	GOOS=linux   GOARCH=amd64        go build $(GOFLAGS) -o $(DIST)/$(BINARY)_linux_amd64        $(MAIN)
	GOOS=linux   GOARCH=arm64        go build $(GOFLAGS) -o $(DIST)/$(BINARY)_linux_arm64        $(MAIN)
	GOOS=linux   GOARCH=arm GOARM=7  go build $(GOFLAGS) -o $(DIST)/$(BINARY)_linux_armv7        $(MAIN)
	GOOS=darwin  GOARCH=amd64        go build $(GOFLAGS) -o $(DIST)/$(BINARY)_darwin_amd64       $(MAIN)
	GOOS=darwin  GOARCH=arm64        go build $(GOFLAGS) -o $(DIST)/$(BINARY)_darwin_arm64       $(MAIN)
	GOOS=windows GOARCH=amd64        go build $(GOFLAGS) -o $(DIST)/$(BINARY)_windows_amd64.exe  $(MAIN)
	GOOS=windows GOARCH=arm64        go build $(GOFLAGS) -o $(DIST)/$(BINARY)_windows_arm64.exe  $(MAIN)
	GOOS=android GOARCH=arm64        go build $(GOFLAGS) -o $(DIST)/$(BINARY)_android_arm64      $(MAIN)
	@echo "built -> $(DIST)/"

help: ## Show targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
