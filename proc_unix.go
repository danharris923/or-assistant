//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func isRoot() bool { return os.Geteuid() == 0 }

// detach puts the background server in its own session so it outlives the launcher.
func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// processGroup makes a command kill its whole process tree when cancelled or timed out.
func processGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
}

const shellPath = "/bin/sh"
