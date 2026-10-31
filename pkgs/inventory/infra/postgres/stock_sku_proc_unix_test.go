//go:build unix

package postgres_test

import (
	"errors"
	"os/exec"
	"syscall"
)

// ownProcessGroup runs cmd as the leader of a new process group, and makes cancelling it
// kill the whole group, so a helper that forks cannot outlive the test.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// groupAlive reports whether any process of the group led by pid still exists.
func groupAlive(pid int) bool {
	return !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}
