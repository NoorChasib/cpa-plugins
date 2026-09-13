SHELL := /bin/bash
VERSION ?= 0.1.0
.PHONY: ci build fmt-check test race vet
fmt-check:
	@test -z "$$(gofmt -l .)"
vet:
	go vet ./...
test:
	go test ./...
race:
	CGO_ENABLED=1 go test -race ./...
ci: fmt-check vet race build
build:
	mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags "-X github.com/NoorChasib/cpa-plugins/plugins/quota-cache/internal/plugin.Version=$(VERSION)" -o dist/quota-cache.so .
