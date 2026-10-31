//go:build !unix

package postgres_test

import "os/exec"

func ownProcessGroup(cmd *exec.Cmd) {}
