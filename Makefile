# A parent go.work covers this directory without listing this module.
export GOWORK := off

# make install puts agentbus in $(DESTDIR)$(BINDIR): /usr/local/bin by
# default, where the units in examples/systemd expect it. For your own
# user only: make install PREFIX=$$HOME/.local (or BINDIR=$$HOME/go/bin).
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin

.DEFAULT_GOAL := build

.PHONY: build install uninstall test test-e2e test-all demo demo-once screenshots clean help

help: ## list the targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

build: ## build bin/agentbus, bin/worker, bin/coordinator
	go build -o bin/agentbus ./cmd/agentbus
	go build -o bin/worker ./demo/cmd/worker
	go build -o bin/coordinator ./demo/cmd/coordinator

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
