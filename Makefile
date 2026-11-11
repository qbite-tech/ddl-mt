.DEFAULT_GOAL := help
APP := ddl-mt
VERSION ?= $(shell cat VERSION)
BIN_DIR ?= bin

.PHONY: help build test test-postgres test-pgbouncer check
help:
	@echo 'make build          Build bin/ddl-mt'
	@echo 'make test           Run unit tests (and DB tests if DDL_MT_TEST_DSN is set)'
	@echo 'make test-postgres  Run tests against a disposable local PostgreSQL cluster'
	@echo 'make test-pgbouncer Run tests with disposable PostgreSQL and both PgBouncer modes'
	@echo 'make check          Run race tests, vet, and build'

build:
	mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags '-X main.version=$(VERSION)' -o $(BIN_DIR)/$(APP) .

test:
	go test ./...

test-postgres:
	bash scripts/test-postgres.sh

test-pgbouncer:
	DDL_MT_TEST_PGBOUNCER=1 bash scripts/test-postgres.sh

check:
	go test -race ./...
	go vet ./...
	$(MAKE) build
