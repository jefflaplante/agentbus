//go:build !unix

package agent

import "os/exec"

// setProcessGroup is a no-op where process groups aren't available; a
// timeout kills the command itself (exec.CommandContext's default) but
// not any children it started.
func setProcessGroup(*exec.Cmd) {}
