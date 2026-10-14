// Command voice-io is the local voice dictation daemon and its control CLI.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// AppKit and GTK must run on the process's main thread.
func init() { runtime.LockOSThread() }

const usage = `usage: voice-io <command> [options]

Commands:
  start                 start the background app (default)
  run                   run the daemon in the foreground
  stop                  quit the running daemon
  status                print daemon state
  settings              open Settings
  recover               copy the latest saved transcript to the clipboard
  models install|verify [--meeting]
                        download or check offline model weights
  ctl <command>         send push|dictate|meeting|collaborative|toggle|cancel|
                        ptt|ptt-stop|reload to the daemon
  shortcuts apply|remove
                        register GNOME shortcuts (Linux)
  render-overlay        render overlay states to PNG files (diagnostics)
  version               print version information
`

func main() {
	args := os.Args[1:]
	// LaunchServices (Finder, login item, `open`) starts the bundle with no
	// arguments (or a -psn_ one): run the app itself.
	cmd := "start"
	if appBundle() != "" && (len(args) == 0 || strings.HasPrefix(args[0], "-psn_")) {
		cmd, args = "run", nil
	}
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "start":
		err = cmdStart(args)
	case "run", "live":
		err = cmdRun(args)
	case "stop":
		err = cmdStop(args)
	case "status":
		err = cmdStatus(args)
	case "settings":
		err = cmdSettings(args)
	case "recover":
		err = cmdRecover(args)
	case "models":
		err = cmdModels(args)
	case "ctl":
		err = cmdCtl(args)
	case "shortcuts", "hotkey":
		err = cmdShortcuts(args)
	case "render-overlay":
		err = cmdRenderOverlay(args)
	case "version", "--version", "-v":
		err = cmdVersion(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "voice-io: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		if code, ok := err.(exitCode); ok {
			os.Exit(int(code))
		}
		fmt.Fprintf(os.Stderr, "voice-io %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

// exitCode ends the process with a status without printing an error.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
