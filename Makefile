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

.PHONY: build clean test lint

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY) ./cmd/on-event
	@echo "Built $(BIN_DIR)/$(BINARY)"

build-platform:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY)-$(OS)-$(ARCH) ./cmd/on-event
	@echo "Built $(BIN_DIR)/$(BINARY)-$(OS)-$(ARCH)"

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BIN_DIR) dist
