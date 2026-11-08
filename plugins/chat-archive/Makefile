SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
VERSION ?= 0.1.0
DIST ?= dist
GO ?= go
PYTHON ?= python3
LDFLAGS := -s -w -X github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/plugin.Version=$(VERSION)
# CPA's image is Debian 12 (glibc 2.36): the library must need no newer symbol.
MAX_GLIBC := 2.36

.PHONY: fmt-check vet test test-race build c-shared glibc-check ci smoke native-spike browser-tools browser store-smoke package-current package-tested checksums verify-release pure-check importer-dist fuse-smoke

fmt-check:
	@files=$$(find . -name '*.go' -not -path './.git/*' -not -path './dist/*'); unformatted=$$(gofmt -l $$files); test -z "$$unformatted" || { printf 'Run gofmt on:\n%s\n' "$$unformatted" >&2; exit 1; }
vet:
	$(GO) vet ./...
	$(GO) vet -tags nativefixture ./...
test:
	$(GO) test -count=1 ./...
	@if [ -d scripts/tests ]; then $(PYTHON) -m unittest discover -s scripts/tests -v; fi
test-race:
	$(GO) test -race -count=1 ./...
	$(GO) test -race -tags nativefixture -count=1 ./...
build:
	mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/chat-archive-build .
c-shared:
	@test "$$($(GO) env GOOS)/$$($(GO) env GOARCH)" = linux/amd64 || { printf 'Only Linux amd64 is claimed and packaged.\n' >&2; exit 1; }
	mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags '$(LDFLAGS)' -o $(DIST)/chat-archive.so .
glibc-check:
	@newest=$$(objdump -T $(DIST)/chat-archive.so | grep -o 'GLIBC_[0-9][0-9.]*' | sed 's/GLIBC_//' | sort -Vu | tail -n1); \
	test -n "$$newest" || { printf 'No GLIBC symbol versions found.\n' >&2; exit 1; }; \
	test "$$(printf '%s\n%s\n' "$$newest" "$(MAX_GLIBC)" | sort -V | tail -n1)" = "$(MAX_GLIBC)" || { printf 'chat-archive.so needs GLIBC_%s (> %s).\n' "$$newest" "$(MAX_GLIBC)" >&2; exit 1; }; \
	printf 'Newest GLIBC symbol: %s (limit %s)\n' "$$newest" "$(MAX_GLIBC)"
ci: fmt-check vet test test-race build c-shared glibc-check pure-check
# Purity guards: the shared packages and the importer build without cgo for both claimed
# platforms, and pkg/ depends on neither mattn/go-sqlite3 nor any internal package.
pure-check:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build ./pkg/... ./cmd/chat-archive-import
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build ./pkg/... ./cmd/...
	@deps=$$($(GO) list -deps ./pkg/...); \
	! grep -q '^github.com/mattn/go-sqlite3' <<<"$$deps" || { printf 'pkg/ depends on mattn/go-sqlite3\n' >&2; exit 1; }; \
	! grep -q '^github.com/NoorChasib/cpa-plugins/plugins/chat-archive/internal/' <<<"$$deps" || { printf 'pkg/ depends on internal/\n' >&2; exit 1; }
importer-dist:
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '-s -w' -o $(DIST)/chat-archive-import_linux_amd64 ./cmd/chat-archive-import
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags '-s -w' -o $(DIST)/chat-archive-import_darwin_arm64 ./cmd/chat-archive-import
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
fuse-smoke:
	bash scripts/native/fuse-smoke.sh
native-spike:
	bash scripts/native/run-spike.sh
# Requires the exact library accepted by make smoke; never silently rebuild it.
store-smoke:
	@test -n "$(CPA_SMOKE_WORK)" || { printf 'Set CPA_SMOKE_WORK to the fresh make smoke directory.\n' >&2; exit 1; }
	$(PYTHON) scripts/native/store_smoke.py --library "$(CPA_SMOKE_WORK)/chat-archive-production.so" --fakearchive "$(CPA_SMOKE_WORK)/fakearchive"
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
