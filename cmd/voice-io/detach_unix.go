//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// backgroundExe is the binary `voice-io start` runs in the background.
func backgroundExe() (string, error) { return os.Executable() }

// detach starts cmd in its own session, free of the terminal.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
