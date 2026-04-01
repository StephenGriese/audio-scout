# Variables
BINARY_NAME=audio-scout
BIN_DIR=bin

# Sentinel file — proves `make init` has been run on this clone.
# Checked before checks/build so contributors can't accidentally skip it.
HOOKS_SENTINEL=.git/hooks/.githooks-installed

.PHONY: init checks staticcheck lint test build clean all

all: build

init:
	git config core.hooksPath .githooks
	@touch $(HOOKS_SENTINEL)
	@echo "✓ Git hooks configured. You're ready to develop."

# Guard target — aborts with a helpful message if init was never run.
$(HOOKS_SENTINEL):
	@echo ""
	@echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
	@echo "⚠️  Run 'make init' first to configure git hooks."
	@echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
	@echo ""
	@exit 1

checks: $(HOOKS_SENTINEL) staticcheck lint test

staticcheck:
	staticcheck ./...

lint:
	golangci-lint -v run --fix ./...

test:
	go test ./...

build: checks
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY_NAME) main.go
	@echo "Build complete. Binary located at $(BIN_DIR)/$(BINARY_NAME)"

clean:
	rm -rf $(BIN_DIR)
	@echo "Clean complete. Removed $(BIN_DIR) directory."