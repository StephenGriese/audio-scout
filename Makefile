init:
	git config core.hooksPath .githooks

checks:	staticcheck lint test

staticcheck:
	staticcheck ./...

lint:
	golangci-lint -v run --fix ./...

test:
	go test ./...

.PHONY: init clean lint modules build
