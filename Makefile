# Variables
BINARY_NAME=audio-scout
BIN_DIR=bin

.PHONY: init checks staticcheck lint test build clean

init:
	git config core.hooksPath .githooks

checks: staticcheck lint test

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