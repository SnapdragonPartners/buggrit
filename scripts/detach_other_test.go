//go:build !unix

package scripts

import "os/exec"

// detach is a no-op where there's no session to leave.
func detach(*exec.Cmd) {}
