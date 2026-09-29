SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
VERSION ?= 0.1.0
DIST ?= dist
GO ?= go
PYTHON ?= python3
LDFLAGS := -s -w -X github.com/NoorChasib/cpa-plugins/plugins/codex-catalog-filter/internal/plugin.Version=$(VERSION)

.PHONY: fmt-check vet test test-race build c-shared ci smoke
fmt-check:
	@files=$$(find . -name '*.go' -not -path './.git/*' -not -path './dist/*'); unformatted=$$(gofmt -l $$files); test -z "$$unformatted" || { printf 'Run gofmt on:\n%s\n' "$$unformatted" >&2; exit 1; }
vet:
	$(GO) vet ./...
test:
	$(GO) test -count=1 ./...
test-race:
	$(GO) test -race -count=1 ./...
build: c-shared
c-shared:
	@test "$$($(GO) env GOOS)/$$($(GO) env GOARCH)" = linux/amd64 || { printf 'Only Linux amd64 is claimed and packaged.\n' >&2; exit 1; }
	mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags '$(LDFLAGS)' -o $(DIST)/codex-catalog-filter.so .
ci: fmt-check vet test test-race c-shared
# Loads the exact library built above into the pinned official CPA v8.0.4
# image with synthetic static models. Requires Docker; never uses real accounts.
smoke: c-shared
	$(PYTHON) scripts/smoke.py --library $(DIST)/codex-catalog-filter.so --version '$(VERSION)'
