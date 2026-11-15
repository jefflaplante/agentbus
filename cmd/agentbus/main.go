// Command agentbus runs the a2a/v1 broker and is a CLI client for agents
// that don't embed the Go client.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
	root := newRootCmd(a)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			return ee.code // already reported
		}
		fmt.Fprintln(a.stderr, "agentbus:", err)
		return 1
	}
	return 0
}
