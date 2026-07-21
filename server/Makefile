# Private cloud API — developer tasks.
#
# Every target works with only Go and Docker installed; there is nothing to
# install globally and no code generation step yet.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.DEFAULT_GOAL := check

.PHONY: check
check: fmt vet test ## fmt + vet + test — run this before every commit

.PHONY: build
build: ## compile the api binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/api ./cmd/api

.PHONY: run
run: ## run locally against a database on localhost
	PC_ENV=dev PC_LOG_FORMAT=text \
	PC_DATABASE_URL="postgres://privatecloud:privatecloud@localhost:5432/privatecloud" \
	go run ./cmd/api

.PHONY: test
test: ## unit tests with the race detector
	# -race requires cgo. In a golang:alpine container CGO_ENABLED defaults to 0
	# and this silently degrades to a plain run, so set it explicitly and install
	# gcc/musl-dev there. The race detector is what caught a data race inside a
	# chunking library that looked completely fine by inspection.
	CGO_ENABLED=1 go test -race ./...

.PHONY: test-integration
test-integration: ## tests that need a real Postgres
	# The tree and storage logic live largely in SQL — partial unique indexes,
	# cascades, refcount triggers, prefix rewrites. A mock would pass while the
	# real schema silently allowed duplicate siblings, so these run against a
	# genuine database and skip without one.
	@test -n "$(PC_TEST_DATABASE_URL)" || \
		(echo "set PC_TEST_DATABASE_URL — see README 'Integration tests'"; exit 1)
	go test ./... -count=1

.PHONY: cover
cover: ## test coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## format all Go source
	go fmt ./...

.PHONY: vet
vet: ## static analysis
	go vet ./...

.PHONY: tidy
tidy: ## sync go.mod/go.sum with imports
	go mod tidy

.PHONY: docker
docker: ## build the container image
	docker build -t privatecloud-api:$(VERSION) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .

.PHONY: help
help: ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
