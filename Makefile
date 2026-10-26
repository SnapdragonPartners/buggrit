.PHONY: build test lint vuln sqlc-generate sqlc-verify check release-snapshot install-hooks deps clean

BINARY := buggrit
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

REQUIRED_BINS := go golangci-lint govulncheck sqlc

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/buggrit

# The Postgres tests skip unless BUGGRIT_POSTGRES_DSN is set (see
# internal/store/store_test.go); CI runs them against a service container.
test:
	go test -cover -race ./...

lint:
	go vet ./...
	golangci-lint run

vuln:
	govulncheck ./...

# sqlc-generate regenerates internal/store/db from its queries and the
# migrations; sqlc-verify fails if the committed code is out of date.
sqlc-generate:
	sqlc generate

sqlc-verify:
	@sqlc diff >/dev/null && echo "sqlc generated code is up to date" || { \
		echo "sqlc generated code is out of date; run 'make sqlc-generate' and commit the result"; \
		exit 1; \
	}

# check is everything CI runs.
check: lint sqlc-verify vuln build test

# release-snapshot builds the release archives into dist/ without publishing
# (needs goreleaser v2). Releases come from pushing a vX.Y.Z tag.
release-snapshot:
	goreleaser release --snapshot --clean

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
			"govulncheck: go install golang.org/x/vuln/cmd/govulncheck@v1.1.4;" \
			"sqlc: go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1"; \
		exit 1; \
	fi

clean:
	rm -rf bin/ coverage/
