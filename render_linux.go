//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// dieWithParent makes the spawned renderer die when this process does — even
// on SIGKILL, where no defer or signal handler gets to run (Linux pdeathsig).
func dieWithParent(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
