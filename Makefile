SHELL := /bin/bash

VERSION ?= dev
DEB_VERSION ?= 0.2.13
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/popododo0720/proxmox-ovn/internal/buildinfo.Version=$(VERSION) \
	-X github.com/popododo0720/proxmox-ovn/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/popododo0720/proxmox-ovn/internal/buildinfo.Date=$(BUILD_DATE)
GO_BUILD_FLAGS := -trimpath -buildvcs=false

.PHONY: all build web-build test test-race web-test ui-test vet fmt-check package-check deb release clean

all: test build

build:
	mkdir -p bin
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags '$(LDFLAGS)' -o bin/pvn-manager ./cmd/pvn-manager
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags '$(LDFLAGS)' -o bin/pvn-agent ./cmd/pvn-agent
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags '$(LDFLAGS)' -o bin/pvnctl ./cmd/pvnctl

web-build:
	npm --prefix web ci
	npm --prefix web run build

test:
	go test ./...

test-race:
	go test -race ./...

web-test:
	npm --prefix web ci
	npm --prefix web test -- --run

ui-test:
	node --test pve-ui/tests/loader.test.mjs
	pve-ui/tests/injector-test.sh

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -d $$(gofmt -l .); exit 1; }

package-check: build
	packaging/tests/package-check.sh

deb: package-check web-build ui-test
	$(MAKE) build VERSION=$(DEB_VERSION)
	@set -eu; \
	pvn_pkg_tmp=$$(mktemp -d); \
	trap 'rm -rf "$$pvn_pkg_tmp"' EXIT HUP INT TERM; \
	pvn_arch=$$(dpkg --print-architecture); \
	pvn_root="$$pvn_pkg_tmp/pvn-node"; \
	install -d "$$pvn_root/DEBIAN" "$$pvn_root/usr/sbin" "$$pvn_root/usr/lib/pvn"; \
	sed -e 's/@VERSION@/$(DEB_VERSION)/g' -e "s/@ARCH@/$$pvn_arch/g" packaging/pvn-node.control > "$$pvn_root/DEBIAN/control"; \
	install -m 0755 packaging/debian/pvn-node.postinst "$$pvn_root/DEBIAN/postinst"; \
	install -m 0755 packaging/debian/pvn-node.prerm "$$pvn_root/DEBIAN/prerm"; \
	install -m 0755 packaging/debian/pvn-node.postrm "$$pvn_root/DEBIAN/postrm"; \
	install -m 0644 packaging/debian/triggers "$$pvn_root/DEBIAN/triggers"; \
	install -m 0755 bin/pvn-manager bin/pvn-agent bin/pvnctl "$$pvn_root/usr/sbin/"; \
	install -m 0755 deploy/scripts/* "$$pvn_root/usr/lib/pvn/"; \
	install -m 0755 deploy/scripts/pvn-db-backup "$$pvn_root/usr/sbin/pvn-db-backup"; \
	install -m 0755 pve-ui/inject.sh "$$pvn_root/usr/lib/pvn/pvn-ui-inject"; \
	install -m 0644 pve-ui/pvn-loader.js "$$pvn_root/usr/lib/pvn/"; \
	install -d "$$pvn_root/usr/share/pvn" "$$pvn_root/usr/share/doc/pvn-node/examples" "$$pvn_root/usr/share/doc/pvn-node/inventory"; \
	cp -a web/dist "$$pvn_root/usr/share/pvn/web"; \
	install -d "$$pvn_root/usr/share/pvn/schema"; \
	install -m 0644 schema/*.ovsschema "$$pvn_root/usr/share/pvn/schema/"; \
	install -m 0644 README.md docs/*.md "$$pvn_root/usr/share/doc/pvn-node/"; \
	install -m 0644 deploy/examples/* "$$pvn_root/usr/share/doc/pvn-node/examples/"; \
	install -m 0644 deploy/inventory/* "$$pvn_root/usr/share/doc/pvn-node/inventory/"; \
	install -d "$$pvn_root/usr/lib/systemd/system"; \
	install -m 0644 deploy/systemd/*.service deploy/systemd/*.target "$$pvn_root/usr/lib/systemd/system/"; \
	for pvn_dropin_source in deploy/systemd/*.service.d; do \
		pvn_dropin_unit=$$(basename "$$pvn_dropin_source"); \
		install -d "$$pvn_root/usr/lib/systemd/system/$$pvn_dropin_unit"; \
		install -m 0644 "$$pvn_dropin_source"/*.conf "$$pvn_root/usr/lib/systemd/system/$$pvn_dropin_unit/"; \
	done; \
	install -d dist; \
	pvn_deb="dist/pvn-node_$(DEB_VERSION)_$${pvn_arch}.deb"; \
	dpkg-deb --root-owner-group --build "$$pvn_root" "$$pvn_deb"; \
	packaging/tests/package-check.sh "$$pvn_deb"

release: deb
	@set -eu; \
	pvn_arch=$$(dpkg --print-architecture); \
	pvn_deb="pvn-node_$(DEB_VERSION)_$${pvn_arch}.deb"; \
	install -m 0755 deploy/scripts/pvn-install.sh dist/pvn-install.sh; \
	install -m 0755 deploy/scripts/pvn-update.sh dist/pvn-update.sh; \
	install -m 0755 deploy/scripts/pvn-cluster-install dist/pvn-cluster-install; \
	install -m 0755 deploy/scripts/pvn-cluster-update dist/pvn-cluster-update; \
	install -m 0755 deploy/scripts/pvn-cluster-lease dist/pvn-cluster-lease; \
	cd dist; \
	sha256sum "$$pvn_deb" pvn-cluster-install pvn-cluster-update \
		pvn-cluster-lease pvn-install.sh pvn-update.sh > SHA256SUMS

clean:
	rm -rf bin coverage dist
