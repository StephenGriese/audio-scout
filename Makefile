# Varialbes
BINARY_NAME=audio-scout
BIN_DIR=bin

.PHONY: init
init:
	git config core.hooksPath .githooks

.PHONY: checks
checks:	staticcheck	lint test

.PHONY: staticcheck
staticcheck:
	staticcheck ./...

.PHONY: lint
lint:
	golangci-lint -v run --fix ./...

.PHONY: test
test:
	go test ./...

.PHONY: build
build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY_NAME) main.go
	@echo "Build complete. Binary located at $(BIN_DIR)/$(BINARY_NAME)"

.PHONY: clean
clean:
	rm -rf $(BIN_DIR)
	@echo "Clean complete. Removed $(BIN_DIR) directory."