package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// appExe is the windowless (GUI-subsystem) build of this program that the
// Start menu, the login item and `voice-io start` run; voice-io.exe is the
// console build for the command line.
const appExe = "Voice IO.exe"

// launchedAsApp reports whether this is the app build started without a
// command, as the Start menu and the login item start it.
func launchedAsApp(args []string) bool {
	exe, err := os.Executable()
	return err == nil && len(args) == 0 && strings.EqualFold(filepath.Base(exe), appExe)
}

// backgroundExe prefers the app build beside this one, which never opens a
// console window.
func backgroundExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if app := filepath.Join(filepath.Dir(exe), appExe); fileExists(app) {
		return app, nil
	}
	return exe, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// detach starts cmd without a console window, outside this console's
// Ctrl+C group.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// logToFile sends output to the log file when the process has nowhere to
// write it, as when the Start menu or the login item starts the app build.
func logToFile() {
	if h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE); err == nil && h != 0 && h != windows.InvalidHandle {
		return
	}
	f, err := openLog()
	if err != nil {
		return
	}
	// The handles keep f open for the life of the process; the runtime
	// writes panics to the standard error handle.
	_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(f.Fd()))
	_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
	os.Stdout, os.Stderr = f, f
	log.SetOutput(f)
}
