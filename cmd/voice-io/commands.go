package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/app"
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/control"
	"gitlab.com/integral-inference/voice-io/internal/insert"
	"gitlab.com/integral-inference/voice-io/internal/paths"
	"gitlab.com/integral-inference/voice-io/internal/platform"
	"gitlab.com/integral-inference/voice-io/internal/recovery"
)

func cmdRun(args []string) error {
	logToFile()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var o app.Options
	fs.BoolVar(&o.DryRun, "dry-run", false, "log instead of pasting or playing sounds")
	fs.StringVar(&o.Input, "input", "", "replay a 16 kHz mono WAV instead of the microphone")
	fs.BoolVar(&o.Realtime, "realtime", false, "pace --input at real-time speed")
	fs.BoolVar(&o.NoUI, "no-ui", false, "no overlay, menu bar item or Settings window")
	fs.DurationVar(&o.Duration, "duration", 0, "stop after this long (0 = run until quit)")
	fs.StringVar(&o.RecordPath, "record", "", "record the next session's audio to this WAV")
	fs.BoolVar(&o.OpenSettings, "open-settings", false, "open Settings once running")
	if err := fs.Parse(args); err != nil {
		return exitCode(2)
	}
	if o.Input != "" {
		o.NoUI = true
	}
	err := app.Run(o)
	if errors.Is(err, control.ErrAlreadyRunning) {
		if o.OpenSettings {
			_, _ = control.Send("settings", "")
		}
		fmt.Fprintln(os.Stderr, "voice-io is already running")
		return nil
	}
	return err
}

// appBundle returns the enclosing .app when this binary runs from one.
func appBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
		return exe[:i+4]
	}
	return ""
}

func installedBundle() string {
	if b := appBundle(); b != "" {
		return b
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{filepath.Join(home, "Applications", "Voice IO.app"), "/Applications/Voice IO.app"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func waitRunning(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if control.Running() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// launch starts the background app; extra args go to `run`.
func launch(extra ...string) error {
	if runtime.GOOS == "darwin" {
		if bundle := installedBundle(); bundle != "" {
			// LaunchServices makes the app (not this terminal) the owner of
			// microphone and Accessibility permissions.
			args := []string{"-g", "-a", bundle}
			if len(extra) > 0 {
				args = append(args, "--args", "run")
				args = append(args, extra...)
			}
			return exec.Command("open", args...).Run()
		}
	}
	if runtime.GOOS == "linux" && len(extra) == 0 {
		if err := exec.Command("systemctl", "--user", "cat", "voice-io.service").Run(); err == nil {
			return exec.Command("systemctl", "--user", "start", "voice-io.service").Run()
		}
	}
	logf, err := openLog()
	if err != nil {
		return err
	}
	defer logf.Close()
	exe, err := backgroundExe()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, append([]string{"run"}, extra...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// openLog opens the private daemon log for appending.
func openLog() (*os.File, error) {
	if err := paths.PrivateDir(paths.LogDir()); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(paths.LogFile()); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlinked log %s", paths.LogFile())
	}
	f, err := os.OpenFile(paths.LogFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY|paths.NoFollow, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	return f, nil
}

func cmdStart(args []string) error {
	if control.Running() {
		fmt.Println("voice-io is running")
		return nil
	}
	if err := launch(args...); err != nil {
		return err
	}
	if !waitRunning(30 * time.Second) {
		return fmt.Errorf("voice-io did not start; see %s", paths.LogFile())
	}
	fmt.Println("voice-io is running")
	return nil
}

func cmdStop([]string) error {
	if _, err := control.Send("quit", ""); err != nil {
		if errors.Is(err, control.ErrNotRunning) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !control.Running() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("voice-io did not stop; see %s", paths.LogFile())
}

func cmdStatus(args []string) error {
	resp, err := control.Send("status", "")
	if errors.Is(err, control.ErrNotRunning) {
		fmt.Println("voice-io is not running")
		return exitCode(3)
	}
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "--json" {
		fmt.Println(string(resp.Data))
		return nil
	}
	var st app.StatusInfo
	if err := json.Unmarshal(resp.Data, &st); err != nil {
		return err
	}
	on := "on"
	if !st.Daemon.Enabled {
		on = "off"
	}
	fmt.Printf("voice-io %s: %s, mode %s, voice %s, shortcut %s\n", st.Version,
		strings.ToLower(string(st.Daemon.State)), st.Daemon.Mode, on, st.Shortcut)
	for _, issue := range st.Issues {
		fmt.Printf("  needs attention: %s\n", issue.Message)
	}
	return nil
}

func cmdSettings(args []string) error {
	page := ""
	if len(args) > 0 {
		page = args[0]
	}
	if _, err := control.Send("settings", page); err == nil {
		return nil
	}
	if err := launch("--open-settings"); err != nil {
		return err
	}
	if !waitRunning(30 * time.Second) {
		return fmt.Errorf("voice-io did not start; see %s", paths.LogFile())
	}
	return nil
}

func cmdRecover([]string) error {
	if _, err := control.Send("recover", ""); err == nil {
		fmt.Println("Transcript copied. Paste it where you need it.")
		return nil
	} else if !errors.Is(err, control.ErrNotRunning) {
		return err
	}
	cfg, err := config.Load("")
	if err != nil {
		cfg = config.Default()
	}
	store, err := recovery.Open(cfg.RecoveryPath(), time.Duration(cfg.Dictation.RecoveryTTL*float64(time.Second)))
	if err != nil {
		return err
	}
	e, ok := store.Latest()
	if !ok || strings.TrimSpace(e.Text) == "" {
		fmt.Fprintln(os.Stderr, "No saved transcript to recover")
		return exitCode(1)
	}
	res := (&insert.Executor{Backend: platform.NewBackend()}).CopyOnly(e.Text)
	if !res.OK {
		return fmt.Errorf("could not copy saved transcript: %s", res.Message)
	}
	fmt.Println("Transcript copied. Paste into the intended editor; the saved copy stays until it expires.")
	return nil
}

func cmdCtl(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: voice-io ctl push|toggle|cancel|dictate|meeting|collaborative|ptt|ptt-stop")
	}
	_, err := control.Send(args[0], "")
	return err
}

func cmdShortcuts(args []string) error {
	if runtime.GOOS != "linux" {
		fmt.Println("macOS shortcuts are registered by the app; change them in Settings.")
		return nil
	}
	if len(args) != 1 || (args[0] != "apply" && args[0] != "remove" && args[0] != "--register" && args[0] != "--unregister") {
		return errors.New("usage: voice-io shortcuts apply|remove")
	}
	if args[0] == "remove" || args[0] == "--unregister" {
		return removeShortcuts()
	}
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	keys, err := cfg.Hotkeys.Parse()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := applyShortcuts(keys, exe); err != nil {
		return err
	}
	fmt.Println("GNOME shortcuts applied")
	return nil
}
