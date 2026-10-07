MODULE := github.com/fiddler-labs/fiddler-coding-agent-plugin
BINARY := on-event
BIN_DIR := bin

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

.PHONY: build build-platform clean test lint dev

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY)$(EXE) ./cmd/on-event
	@echo "Built $(BIN_DIR)/$(BINARY)$(EXE)"

build-platform:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY)-$(OS)-$(ARCH)$(EXE) ./cmd/on-event
	@echo "Built $(BIN_DIR)/$(BINARY)-$(OS)-$(ARCH)$(EXE)"

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BIN_DIR) dist

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
