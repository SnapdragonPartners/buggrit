//go:build unix

package scripts

import (
	"os/exec"
	"syscall"
)

// detach runs cmd in a new session, with no controlling terminal, so a
// script that reads /dev/tty gets an error instead of the test's terminal.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
