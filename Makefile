# A parent go.work covers this directory without listing this module.
export GOWORK := off

# make install puts agentbus in $(DESTDIR)$(BINDIR): /usr/local/bin by
# default, where the units in examples/systemd expect it. For your own
# user only: make install PREFIX=$$HOME/.local (or BINDIR=$$HOME/go/bin).
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin

.DEFAULT_GOAL := build

# make image builds the container image with $(CONTAINER) (podman or docker).
CONTAINER ?= podman
VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo dev)
IMAGE ?= agentbus:$(VERSION)

.PHONY: build image install uninstall test test-e2e test-all bench bench-binary demo demo-once screenshots clean help

help: ## list the targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

build: ## build bin/agentbus, bin/worker, bin/coordinator
	go build -o bin/agentbus ./cmd/agentbus
	go build -o bin/worker ./demo/cmd/worker
	go build -o bin/coordinator ./demo/cmd/coordinator

image: ## build the container image $(IMAGE) from the Containerfile
	$(CONTAINER) build -f Containerfile --build-arg VERSION=$(VERSION) -t $(IMAGE) .

install: ## build agentbus and install it in $(BINDIR) (sudo for /usr/local/bin)
	go build -trimpath -o bin/agentbus ./cmd/agentbus
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 bin/agentbus $(DESTDIR)$(BINDIR)/agentbus
	@echo "installed $(DESTDIR)$(BINDIR)/agentbus"

uninstall: ## remove $(BINDIR)/agentbus (config, tokens and data are left alone)
	rm -f $(DESTDIR)$(BINDIR)/agentbus

test: ## unit + in-process broker tests (fast)
	go test -short -race ./...

test-e2e: ## functional test: real binaries as separate processes
	go test -race -count=1 -v ./test/e2e/

test-all: ## everything
	go vet ./...
	go test -race -count=1 ./...

# make bench measures broker throughput and latency with data_dir off and
# on. The data directory goes under $$TMPDIR, so TMPDIR picks the disk.
# BENCHTIME is messages per case; BENCH filters cases (a go test -bench
# pattern). See docs/BENCHMARKS.md.
BENCHTIME ?= 300x
BENCH ?= .
BENCH_GOARCH ?= amd64

bench: ## broker throughput and latency, data_dir off and on (~1-2 min)
	go test -run '^$$' -bench '$(BENCH)' -benchtime $(BENCHTIME) ./broker

bench-binary: ## build bin/broker-bench.test for linux/$(BENCH_GOARCH), to run on another host
	GOOS=linux GOARCH=$(BENCH_GOARCH) CGO_ENABLED=0 go test -c -o bin/broker-bench.test ./broker

demo: ## run the demo and leave the bus up to explore
	scripts/demo.sh

demo-once: ## run the demo conversation and exit
	scripts/demo.sh --once

screenshots: ## regenerate docs/images from a live in-process bus
	go run ./tools/screenshots -out docs/images

clean: ## remove build output, demo state and test artifacts (not your config or tokens)
	rm -rf bin demo/.run
	rm -f coverage.* *.pprof *.test
	go clean -testcache
