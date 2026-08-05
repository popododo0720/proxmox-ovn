SHELL := /bin/bash

VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/pvnstack/proxmox-ovn/internal/buildinfo.Version=$(VERSION) \
	-X github.com/pvnstack/proxmox-ovn/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/pvnstack/proxmox-ovn/internal/buildinfo.Date=$(BUILD_DATE)

.PHONY: all build test test-race vet fmt-check clean

all: test build

build:
	mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/pvn-manager ./cmd/pvn-manager
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/pvn-agent ./cmd/pvn-agent
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/pvnctl ./cmd/pvnctl

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -d $$(gofmt -l .); exit 1; }

clean:
	rm -rf bin coverage dist

