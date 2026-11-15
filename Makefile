# A parent go.work covers this directory without listing this module.
export GOWORK := off

.PHONY: build test test-e2e test-all demo demo-once screenshots clean

build: ## build bin/agentbus, bin/worker, bin/coordinator
	go build -o bin/agentbus ./cmd/agentbus
	go build -o bin/worker ./demo/cmd/worker
	go build -o bin/coordinator ./demo/cmd/coordinator

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

clean:
	rm -rf bin demo/.run
