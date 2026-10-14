package main

import (
	"os"
	"syscall"
)

// logToFile points stdout and stderr at the log file when they lead to
// /dev/null, as they do when LaunchServices or a login item starts the app.
// `voice-io start` and Settings refer people to that file.
func logToFile() {
	out, err := os.Stderr.Stat()
	if err != nil {
		return
	}
	null, err := os.Stat(os.DevNull)
	if err != nil || !os.SameFile(out, null) {
		return
	}
	f, err := openLog()
	if err != nil {
		return
	}
	defer f.Close()
	_ = syscall.Dup2(int(f.Fd()), 1)
	_ = syscall.Dup2(int(f.Fd()), 2)
}
