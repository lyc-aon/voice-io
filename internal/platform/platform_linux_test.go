//go:build linux

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeYdotool puts a ydotool on PATH that prints help like the given
// version and logs every key command it receives.
func fakeYdotool(t *testing.T, help string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "keys.log")
	script := "#!/bin/sh\nif [ \"$1\" = help ]; then\n" + help + "\nfi\nprintf '%s\\n' \"$*\" >> " + logPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ydotool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("XDG_SESSION_TYPE", "wayland")
	return logPath
}

func sentKeys(t *testing.T, logPath string) string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestYdotool1SendsKeycodes(t *testing.T) {
	// ydotool 1.x: help on stdout, exit 0; key takes KEYCODE:STATE.
	logPath := fakeYdotool(t, "echo 'Usage: ydotool <cmd> <args>'; echo 'Use environment variable YDOTOOL_SOCKET to specify daemon socket.'; exit 0")
	for _, chord := range []string{"ctrl+shift+v", "shift+Insert", "ctrl+enter"} {
		if !sendKey(chord) {
			t.Fatalf("sendKey(%q) failed", chord)
		}
	}
	want := "key 29:1 42:1 47:1 47:0 42:0 29:0\nkey 42:1 110:1 110:0 42:0\nkey 29:1 28:1 28:0 29:0"
	if got := sentKeys(t, logPath); got != want {
		t.Fatalf("sent\n%s\nwant\n%s", got, want)
	}
}

func TestYdotool01SendsKeyNames(t *testing.T) {
	// ydotool 0.1.x: help on stderr, exit 1; key takes names like ctrl+v.
	logPath := fakeYdotool(t, "echo 'Usage: ydotool <cmd> <args>' >&2; exit 1")
	if !sendKey("ctrl+v") {
		t.Fatal("sendKey failed")
	}
	if got := sentKeys(t, logPath); got != "key ctrl+v" {
		t.Fatalf("sent %q, want key ctrl+v", got)
	}
}

func TestUnknownKeyIsNeverSentAsKeycodeZero(t *testing.T) {
	if _, err := evdevKeys("ctrl+q"); err == nil {
		t.Fatal("evdevKeys accepted a key without a keycode")
	}
}
