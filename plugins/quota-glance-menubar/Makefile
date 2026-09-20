SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
VERSION ?= 0.3.0
ARCHS ?= arm64 x86_64
SWIFT ?= swift

.PHONY: test build install run package dmg sign-release appcast check-scripts verify-bundle ci
test:
	python3 -m unittest discover -s scripts/tests -p 'test_*.py'
	node --test scripts/tests/*.test.mjs
	$(SWIFT) test
check-scripts:
	bash -n scripts/*.sh
	python3 scripts/check-plist.py
build: check-scripts
	VERSION='$(VERSION)' APP_ARCHS='$(ARCHS)' bash scripts/build-app.sh
verify-bundle:
	APP_ARCHS='$(ARCHS)' bash scripts/verify-bundle.sh
install: build
	bash scripts/install-app.sh
run: build
	open 'dist/Quota Glance.app'
package: build verify-bundle
	ditto -c -k --sequesterRsrc --keepParent 'dist/Quota Glance.app' 'dist/Quota-Glance-$(VERSION)-macOS.zip'
dmg: build verify-bundle
	APP_ARCHS='$(ARCHS)' bash scripts/build-dmg.sh
# Uses the exact app that already passed ci; never rebuild after notarization.
sign-release:
	APP_ARCHS='$(ARCHS)' bash scripts/sign-release.sh
appcast:
	bash scripts/build-appcast.sh
ci: check-scripts test package dmg
