.PHONY: build test lint vuln check install-hooks deps clean

BINARY := buggrit
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

REQUIRED_BINS := go golangci-lint govulncheck

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/buggrit

test:
	go test -cover -race ./...

lint:
	go vet ./...
	golangci-lint run

vuln:
	govulncheck ./...

# check is everything CI runs.
check: lint vuln build test

install-hooks:
	@./hooks/install.sh

deps:
	@missing=0; \
	for bin in $(REQUIRED_BINS); do \
		if ! command -v $$bin >/dev/null 2>&1; then \
			echo "missing: $$bin"; \
			missing=1; \
		fi; \
	done; \
	if [ $$missing -eq 0 ]; then \
		echo "all dependencies present"; \
	else \
		echo "install: go from https://go.dev/dl/;" \
			"golangci-lint: go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8;" \
			"govulncheck: go install golang.org/x/vuln/cmd/govulncheck@v1.1.4"; \
		exit 1; \
	fi

clean:
	rm -rf bin/ coverage/
