# Test and build entrypoints.
#
# `check` is the single definition of "is this good to ship". CI runs it, the
# goreleaser before-hook runs it, and you run it locally — one definition, so a
# gate cannot pass in one place and fail in another. Add a gate here, not in
# the workflow file.

GO      ?= go
PKGS    := ./...
COVER   := coverage.out

.DEFAULT_GOAL := check

.PHONY: check
check: fmt-check vet test ## Everything the release gate requires

.PHONY: test
test: ## Run the test suite
	$(GO) test $(PKGS)

.PHONY: race
race: ## Run tests under the race detector (needs a C toolchain)
	CGO_ENABLED=1 $(GO) test -race $(PKGS)

.PHONY: cover
cover: ## Report per-package and total coverage
	$(GO) test -coverprofile=$(COVER) -covermode=atomic $(PKGS)
	@$(GO) tool cover -func=$(COVER) | tail -1

.PHONY: cover-html
cover-html: cover ## Open the coverage report in a browser
	$(GO) tool cover -html=$(COVER)

.PHONY: fmt
fmt: ## Format all Go source in place
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is unformatted
# gofmt -l prints offenders and still exits 0, so the output has to be turned
# into the failure. An unformatted file shipped once because nothing did this.
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum are not tidy
	@cp go.mod go.mod.bak && cp go.sum go.sum.bak
	@$(GO) mod tidy
	@if ! diff -q go.mod go.mod.bak >/dev/null || ! diff -q go.sum go.sum.bak >/dev/null; then \
		mv go.mod.bak go.mod; mv go.sum.bak go.sum; \
		echo "go.mod/go.sum are not tidy — run 'go mod tidy'"; exit 1; \
	fi
	@rm -f go.mod.bak go.sum.bak

.PHONY: build
build: ## Build the binary into bin/
	$(GO) build -o bin/sloppy-toppy ./cmd/sloppy-toppy

.PHONY: install
install: ## Install into GOBIN
	$(GO) install ./cmd/sloppy-toppy

.PHONY: snapshot
snapshot: ## Cross-compile all release targets without tagging or publishing
	goreleaser release --snapshot --clean

.PHONY: clean
clean:
	rm -rf bin dist $(COVER)

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
