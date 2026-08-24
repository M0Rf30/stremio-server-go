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

# CGO is not required for most targets; disabling it makes them cross-compile
# as pure Go. The two exceptions are android/arm and android/arm64: Android
# ships no /etc/resolv.conf, so a pure-Go binary's resolver falls back to
# 127.0.0.1:53 (nothing listens there) and every DNS lookup fails -- only cgo
# linking against bionic's real resolver works. See ANDROID_ARM_CC /
# ANDROID_ARM64_CC in build-all.
export CGO_ENABLED := 0

# android/arm and android/arm64 both need an NDK clang cross-compiler (see
# above). Point ANDROID_ARM_CC / ANDROID_ARM64_CC at one each (or put the NDK
# toolchain bin dir on PATH) and build-all includes them; otherwise each
# missing one is skipped with a notice. ANDROID_LDFLAGS forces external
# linking + PIE (mirroring elgatito/elementum's Makefile:91-103) and statically
# links libstdc++ so the binary doesn't NEED libc++_shared.so -- an NDK
# redistributable apps normally bundle inside their APK that a standalone
# Kodi-addon-dropped binary has no APK to ship.
ANDROID_ARM_CC     ?= armv7a-linux-androideabi21-clang
ANDROID_ARM_CXX    ?= armv7a-linux-androideabi21-clang++
ANDROID_ARM64_CC   ?= aarch64-linux-android21-clang
ANDROID_ARM64_CXX  ?= aarch64-linux-android21-clang++
ANDROID_LDFLAGS    := -linkmode=external -extldflags "-pie -lm -static-libstdc++"

.PHONY: all build run test vet fmt fmt-check lint tidy clean smoke build-all swagger help

all: fmt-check vet lint test build

build: ## Build the binary for the host platform
	go build $(GOFLAGS) -o $(BINARY) $(MAIN)

run: build ## Build and run
	./$(BINARY)

# The race detector requires cgo, so this is the one target that overrides the
# global CGO_ENABLED := 0 above. Builds stay pure Go.
test: export CGO_ENABLED := 1
test: ## Run tests (race detector, serial)
	go test -p 1 -race ./...

vet: ## go vet
	go vet ./...

fmt: ## Format (gofmt -s)
	gofmt -s -w .

fmt-check: ## Fail if not gofmt -s clean
	@test -z "$$(gofmt -s -l .)" || (echo "run 'make fmt'"; gofmt -s -l .; exit 1)

lint: ## golangci-lint (if installed)
	@if command -v golangci-lint >/dev/null; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping"; \
	fi

tidy: ## go mod tidy
	go mod tidy

swagger: ## Regenerate the OpenAPI (Swagger 2.0) spec from code annotations (needs swaggo/swag)
	swag init -g cmd/stremio-server/main.go -o docs --outputTypes yaml,json --parseInternal

smoke: build ## Run the end-to-end API smoke test
	./scripts/smoke.sh

clean: ## Remove build artifacts
	rm -rf $(BINARY) $(DIST)

# Cross-compile every published target (pure Go, CGO disabled -- except
# android/arm and android/arm64, which need cgo + an NDK clang; see above).
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
	@if command -v $(ANDROID_ARM64_CC) >/dev/null 2>&1; then \
		CGO_ENABLED=1 CC=$(ANDROID_ARM64_CC) CXX=$(ANDROID_ARM64_CXX) \
		GOOS=android GOARCH=arm64 \
		go build -trimpath -ldflags '$(LDFLAGS) $(ANDROID_LDFLAGS)' -o $(DIST)/$(BINARY)_android_arm64 $(MAIN); \
	else \
		echo "skipping android/arm64: $(ANDROID_ARM64_CC) not found (set ANDROID_ARM64_CC to an NDK clang)"; \
	fi
	@if command -v $(ANDROID_ARM_CC) >/dev/null 2>&1; then \
		CGO_ENABLED=1 CC=$(ANDROID_ARM_CC) CXX=$(ANDROID_ARM_CXX) \
		GOOS=android GOARCH=arm GOARM=7 \
		go build -trimpath -ldflags '$(LDFLAGS) $(ANDROID_LDFLAGS)' -o $(DIST)/$(BINARY)_android_armv7 $(MAIN); \
	else \
		echo "skipping android/armv7: $(ANDROID_ARM_CC) not found (set ANDROID_ARM_CC to an NDK clang)"; \
	fi
	@echo "built -> $(DIST)/"

help: ## Show targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
