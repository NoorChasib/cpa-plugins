SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
VERSION ?= 0.5.0
SWIFT ?= swift
# The app is Apple silicon (arm64) only. Fail instead of ignoring an old
# ARCHS=x86_64 or universal request, also one exported in the environment.
# Only test and check-scripts (and plain `make`, which runs test) never build
# the app, so they ignore ARCHS.
ifneq ($(filter-out arm64,$(ARCHS)),)
ifneq ($(filter-out test check-scripts,$(MAKECMDGOALS)),)
$(error Quota Glance builds for Apple silicon (arm64) only; Intel and universal builds were removed)
endif
endif

.PHONY: test build install run package dmg sign-release appcast check-updater check-scripts verify-bundle ci
test:
	python3 -m unittest discover -s scripts/tests -p 'test_*.py'
	node --test scripts/tests/*.test.mjs
	$(SWIFT) test
check-scripts:
	bash -n scripts/*.sh
	python3 scripts/check-plist.py
build: check-scripts
	VERSION='$(VERSION)' bash scripts/build-app.sh
verify-bundle:
	bash scripts/verify-bundle.sh
install: build
	bash scripts/install-app.sh
run: build
	open 'dist/Quota Glance.app'
package: build verify-bundle
	ditto -c -k --sequesterRsrc --keepParent 'dist/Quota Glance.app' 'dist/Quota-Glance-$(VERSION)-macOS.zip'
dmg: build verify-bundle
	bash scripts/build-dmg.sh
# Uses the exact app that already passed ci; never rebuild after notarization.
sign-release:
	bash scripts/sign-release.sh
check-updater:
	xcrun swiftc scripts/probe-updater.swift -F 'dist/Quota Glance.app/Contents/Frameworks' -framework Sparkle -Xlinker -rpath -Xlinker '$(CURDIR)/dist/Quota Glance.app/Contents/Frameworks' -o .build/probe-updater
appcast: check-updater
	bash scripts/build-appcast.sh
ci: check-scripts test package dmg check-updater
