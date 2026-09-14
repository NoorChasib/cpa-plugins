SHELL := /bin/bash
VERSION ?= 0.1.0
.PHONY: ci build fmt-check test race vet golden
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
ci: fmt-check vet race build
build:
	mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-X github.com/NoorChasib/cpa-plugins/plugins/quota-glance/internal/plugin.Version=$(VERSION)" -o dist/quota-glance.so .
