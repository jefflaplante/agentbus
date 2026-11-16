// Command hostdoc is an agentbus agent whose handlers are a model and the
// tools it may call. Build it with `agentbus agent build`; see README.md.
package main

import (
	"embed"

	"github.com/jefflaplante/agentbus/agent"
)

// The manifest and every script ship inside the binary. agent.local.yaml
// is not embedded: pass it with -manifest on hosts that use a local model.
//
//go:embed agent.yaml all:scripts
var files embed.FS

func main() { agent.Main(files, "agent.yaml") }
