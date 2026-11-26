//go:build !unix

package main

import "os/exec"

// Non-Unix builds exist only so the code compiles for local testing.
func isRoot() bool             { return false }
func detach(c *exec.Cmd)       {}
func processGroup(c *exec.Cmd) {}

const shellPath = "sh"
