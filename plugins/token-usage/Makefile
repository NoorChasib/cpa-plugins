SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
VERSION ?= 0.1.3
DIST ?= dist
GO ?= go
PYTHON ?= python3
LDFLAGS := -s -w -X github.com/NoorChasib/cpa-plugins/plugins/token-usage/internal/plugin.Version=$(VERSION)

.PHONY: fmt-check vet test test-race build c-shared ci smoke native-spike browser-tools browser store-smoke package-current package-tested checksums verify-release
fmt-check:
	@files=$$(find . -name '*.go' -not -path './.git/*' -not -path './dist/*'); unformatted=$$(gofmt -l $$files); test -z "$$unformatted" || { printf 'Run gofmt on:\n%s\n' "$$unformatted" >&2; exit 1; }
vet:
	$(GO) vet ./...
	$(GO) vet -tags nativefixture ./...
test:
	$(GO) test -count=1 ./...
	$(PYTHON) -m unittest discover -s scripts/tests -v
test-race:
	$(GO) test -race -count=1 ./...
	$(GO) test -race -tags nativefixture -count=1 ./...
build:
	mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/token-usage-build .
c-shared:
	@test "$$($(GO) env GOOS)/$$($(GO) env GOARCH)" = linux/amd64 || { printf 'Only Linux amd64 is claimed and packaged.\n' >&2; exit 1; }
	mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags '$(LDFLAGS)' -o $(DIST)/token-usage.so .
ci: fmt-check vet test test-race build c-shared
browser-tools:
	@test "$$(node --version)" = v26.8.1 && test "$$(npm --version)" = 11.19.0
	npm --prefix scripts/browser ci --ignore-scripts --no-audit --no-fund
	chmod u+x scripts/browser/node_modules/agent-browser/bin/agent-browser-linux-x64
	bash scripts/browser/install-chrome.sh
	bash scripts/browser/check-tools.sh
browser:
	bash scripts/browser/run.sh
smoke:
	bash scripts/native/run-smoke.sh
# Requires the exact library accepted by make smoke; never silently rebuild it.
store-smoke:
	@test -n "$(CPA_SMOKE_WORK)" || { printf 'Set CPA_SMOKE_WORK to the fresh make smoke directory.\n' >&2; exit 1; }
	bash -c 'source scripts/browser/check-tools.sh; $(PYTHON) scripts/native/store_smoke.py --library "$(CPA_SMOKE_WORK)/token-usage-production.so"'
native-spike:
	bash scripts/native/run-spike.sh
package-current: c-shared
	$(PYTHON) scripts/release.py package --version '$(VERSION)' --dist '$(DIST)'
# Release workflow copies its smoke-tested production library into DIST first.
# No rebuild here: the published bytes must be the bytes accepted by real CPA.
package-tested:
	$(PYTHON) scripts/release.py package --version '$(VERSION)' --dist '$(DIST)'
checksums:
	$(PYTHON) scripts/release.py checksums --version '$(VERSION)' --dist '$(DIST)'
verify-release:
	$(PYTHON) scripts/release.py verify --version '$(VERSION)' --dist '$(DIST)'
