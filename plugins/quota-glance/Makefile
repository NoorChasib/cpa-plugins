SHELL := /bin/bash
VERSION ?= 0.1.3

# The dashboard. Its build output is committed, because `go build` embeds it and
# must never need Node; web-verify is what stops that committed copy drifting.
WEB_DIR := web
WEB_BUNDLE := $(WEB_DIR)/dist/index.html
WEB_SCRATCH := $(WEB_DIR)/.verify

.PHONY: ci build fmt-check test race vet golden smoke web web-verify web-dev
fmt-check:
	@test -z "$$(gofmt -l .)"
vet:
	go vet ./...
test:
	go test ./...
race:
	CGO_ENABLED=1 go test -race ./...
# Regenerates both committed contracts under testdata/golden/. The web app
# develops against them, so review the diff: a change here is a contract change.
golden:
	go test ./internal/aggregate/ -run 'TestGolden' -count=1 -update
ci: fmt-check vet web-verify race build
# Loads the built library into a disposable pinned CPA container and drives the
# real routes. Needs docker; nothing else in the suite leaves the process.
smoke: build
	python3 ../../scripts/quota-glance-smoke.py
build:
	mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-X github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/plugin.Version=$(VERSION)" -o dist/quota-glance.so .

# Builds the dashboard into web/dist/index.html, which go:embed reads. One
# self-contained document: CPA resource routes are exact-path and GET-only, so
# a directory of hashed assets could not be served without a route per file.
#
# Commit the result. A contributor with no Node toolchain still has to be able
# to build the plugin.
web:
	cd $(WEB_DIR) && pnpm install --frozen-lockfile && pnpm build

# The dev server, against the committed golden documents. Needs no CPA running.
# Set QUOTA_GLANCE_PROXY to develop against a live one instead.
web-dev:
	cd $(WEB_DIR) && pnpm install && pnpm dev

# Rebuilds the dashboard into a scratch directory and compares it to the
# committed bundle.
#
# The bundle is a build artifact that lives in git, which is the arrangement
# that lets a stale one ship silently: edit the app, forget to rebuild, and
# every Go test still passes against last week's page. Rebuilding here rather
# than in `web` is deliberate — CI must fail on the stale commit, not quietly
# fix it and carry on.
web-verify:
	@command -v pnpm >/dev/null || { \
		echo "pnpm is required to verify $(WEB_BUNDLE)."; \
		echo "Install it (npm i -g pnpm), or run the Go-only targets: make fmt-check vet race build"; \
		exit 1; }
	@test -f $(WEB_BUNDLE) || { echo "$(WEB_BUNDLE) is missing; run: make web"; exit 1; }
	cd $(WEB_DIR) && pnpm install --frozen-lockfile
	cd $(WEB_DIR) && pnpm exec tsc -b
	cd $(WEB_DIR) && pnpm exec vite build --outDir $(notdir $(WEB_SCRATCH)) --emptyOutDir
	@if ! cmp -s $(WEB_SCRATCH)/index.html $(WEB_BUNDLE); then \
		echo; \
		echo "$(WEB_BUNDLE) is not what web/src builds."; \
		echo "The committed bundle is stale. Rebuild and commit it:"; \
		echo; \
		echo "    make -C plugins/quota-glance web"; \
		echo "    git add plugins/quota-glance/$(WEB_BUNDLE)"; \
		echo; \
		echo "committed: $$(wc -c < $(WEB_BUNDLE)) bytes, $$(sha256sum < $(WEB_BUNDLE) | cut -c1-16)"; \
		echo "rebuilt:   $$(wc -c < $(WEB_SCRATCH)/index.html) bytes, $$(sha256sum < $(WEB_SCRATCH)/index.html | cut -c1-16)"; \
		rm -rf $(WEB_SCRATCH); \
		exit 1; \
	fi
	@rm -rf $(WEB_SCRATCH)
	@echo "$(WEB_BUNDLE) matches a fresh build."
