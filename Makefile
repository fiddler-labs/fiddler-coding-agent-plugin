MODULE := github.com/fiddler-labs/fiddler-coding-agent-plugin
BINARY := on-event
BIN_DIR := bin

# Release checksums: build every release target with the Go toolchain pinned in
# go.mod so the output matches GoReleaser's, then hash it into checksums.txt.
GO_TOOLCHAIN := $(shell awk '/^toolchain /{print $$2}' go.mod)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
PIN_DIR := dist-pin

OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
ARCH := $(shell uname -m)
ifeq ($(ARCH),x86_64)
  ARCH := amd64
endif
ifeq ($(ARCH),aarch64)
  ARCH := arm64
endif
# Git Bash / MSYS2 / Cygwin on Windows. `go build -o` does not add ".exe" by
# itself, and Windows only runs the binary with the suffix, which is also the
# name scripts/on-event.sh looks for in local mode.
EXE :=
ifneq ($(filter mingw% msys% cygwin%,$(OS)),)
  OS := windows
  EXE := .exe
endif

.PHONY: build build-platform checksums clean test lint dev

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY)$(EXE) ./cmd/on-event
	@echo "Built $(BIN_DIR)/$(BINARY)$(EXE)"

build-platform:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY)-$(OS)-$(ARCH)$(EXE) ./cmd/on-event
	@echo "Built $(BIN_DIR)/$(BINARY)-$(OS)-$(ARCH)$(EXE)"

checksums:
	@test -n "$(GO_TOOLCHAIN)" || { echo "checksums: add a toolchain line to go.mod" >&2; exit 1; }
	@rm -rf $(PIN_DIR) && mkdir -p $(PIN_DIR)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  GOTOOLCHAIN=$(GO_TOOLCHAIN) CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -buildvcs=false -ldflags "-s -w" \
	    -o $(PIN_DIR)/on-event-$$os-$$arch$$ext ./cmd/on-event || exit 1; \
	done
	@cd $(PIN_DIR) && { command -v sha256sum >/dev/null && sha256sum * || shasum -a 256 *; } \
	  | LC_ALL=C sort -k2 > ../checksums.txt
	@echo "Wrote checksums.txt"

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BIN_DIR) dist $(PIN_DIR)

# Local development: export .env.local into this make process, then start
# Claude Code with the plugin loaded from this checkout. The plugin itself never
# reads .env.local; hooks inherit the exported CLAUDE_PLUGIN_OPTION_* values.
# Builds first when FIDDLER_BINARY_SOURCE=local. Extra claude flags:
#   make dev ARGS="--debug"
dev:
	@test -f .env.local || { echo "dev: .env.local not found. Run: cp .env.local.example .env.local and fill it in" >&2; exit 1; }
	@set -a; . ./.env.local; set +a; \
	if [ "$${FIDDLER_BINARY_SOURCE:-release}" = local ]; then $(MAKE) --no-print-directory build; fi; \
	exec claude --plugin-dir . $(ARGS)
