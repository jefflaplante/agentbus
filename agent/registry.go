package agent

import (
	"fmt"
	"slices"
	"sync"

	"github.com/jefflaplante/agentbus/client"
)

// Func is a Go handler: it gets the envelope and returns the reply body
// (for a request) or nothing (for an event). Return client.Errorf(code,
// ...) to choose the error code the requester sees.
type Func = client.Handler

var (
	regMu    sync.Mutex
	registry = map[string]Func{}
)

// Register makes fn available to manifests as `go: name`. Call it from an
// init function in a handlers package; a generated agent imports every
// such package, so adding a .go file under handlers/ is all it takes.
// Registering a name twice panics: two plugins would silently fight.
func Register(name string, fn Func) {
	regMu.Lock()
	defer regMu.Unlock()
	if name == "" || fn == nil {
		panic("agent.Register: empty name or nil function")
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("agent.Register: %q registered twice", name))
	}
	registry[name] = fn
}

// Registered lists the Go handler names available in this binary.
func Registered() []string {
	regMu.Lock()
	defer regMu.Unlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func lookup(name string) (Func, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	fn, ok := registry[name]
	return fn, ok
}
