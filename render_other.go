//go:build !linux

package main

import "os/exec"

// dieWithParent: no pdeathsig outside Linux — stopRenderer plus the signal
// handler in main cover the shutdown paths there.
func dieWithParent(_ *exec.Cmd) {}
