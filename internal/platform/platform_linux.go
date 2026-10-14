//go:build linux

package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/insert"
	"gitlab.com/integral-inference/voice-io/internal/paths"
)

const (
	pasteHint = "Ctrl+V"
	focusBus  = "org.voiceio.Focus1"
	focusPath = "/org/voiceio/Focus1"
)

// pasteBlocked: Linux needs no grant; missing tools surface as paste errors.
func pasteBlocked() (string, string) { return "", "" }

// SetMainLoopRunning is a no-op on Linux (GTK work is scheduled by the UI).
func SetMainLoopRunning(bool) {}

func probeFocus() string {
	if os.Getenv("XDG_SESSION_TYPE") == "wayland" {
		return waylandFocus()
	}
	return x11Focus()
}

// waylandFocus asks the optional GNOME Shell extension for the compositor's
// focused window, trusting only a reply from the running gnome-shell.
func waylandFocus() string {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return ""
	}
	defer conn.Close()
	bus := conn.BusObject()
	var owner string
	if bus.Call("org.freedesktop.DBus.GetNameOwner", 0, focusBus).Store(&owner) != nil {
		return ""
	}
	var pid uint32
	if bus.Call("org.freedesktop.DBus.GetConnectionUnixProcessID", 0, owner).Store(&pid) != nil {
		return ""
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil || filepath.Base(exe) != "gnome-shell" {
		return ""
	}
	var app, title, identity string
	call := conn.Object(owner, focusPath).Call(focusBus+".GetFocusedWindow", 0)
	if call.Err != nil || call.Store(&app, &title, &identity) != nil || app == "" || identity == "" {
		return ""
	}
	return app + "|gnome:" + identity
}

// x11Focus tags the X11 keyboard focus with its WM_CLASS.
func x11Focus() string {
	out, err := exec.Command("xdotool", "getwindowfocus").Output()
	if err != nil {
		return ""
	}
	xid := strings.TrimSpace(string(out))
	if xid == "" {
		return ""
	}
	class, err := exec.Command("xprop", "-id", xid, "WM_CLASS").Output()
	if err != nil {
		return ""
	}
	app := ""
	if _, rest, ok := strings.Cut(string(class), "="); ok {
		parts := strings.Split(rest, ",")
		app = strings.Trim(strings.TrimSpace(parts[len(parts)-1]), `"`)
	}
	if app == "" {
		return ""
	}
	return app + "|x11:" + xid
}

func pasteChord(focus string) string { return insert.LinuxChord(focus) }

// Pause between clipboard write and chord, and before restoring: a short
// after-delay lets the target read stale content (Handy #502).
const pasteDelay = 60 * time.Millisecond

type linuxBackend struct{}

// NewBackend returns the wl-clipboard + ydotool/wtype backend.
func NewBackend() insert.Backend { return linuxBackend{} }

type clipSnapshot struct {
	known bool // false: the probe failed; never clear the user's clipboard
	empty bool
	mime  string
	data  []byte
}

func run(timeout time.Duration, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.Bytes(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return nil, errors.New(name + " timed out")
	}
}

func snapshotClipboard() clipSnapshot {
	types, err := run(time.Second, "wl-paste", "--list-types")
	if err != nil {
		return clipSnapshot{}
	}
	listed := strings.Fields(string(types))
	for _, t := range listed {
		if strings.Contains(t, "text") {
			if data, err := run(time.Second, "wl-paste"); err == nil {
				return clipSnapshot{known: true, mime: "text/plain", data: data}
			}
			return clipSnapshot{}
		}
	}
	for _, t := range listed {
		if strings.Contains(t, "image") {
			if data, err := run(time.Second, "wl-paste", "-t", "image/png"); err == nil && len(data) > 0 {
				return clipSnapshot{known: true, mime: "image/png", data: data}
			}
			return clipSnapshot{}
		}
	}
	return clipSnapshot{known: true, empty: true}
}

// spawnCopy hands data to wl-copy, which stays alive as the selection owner.
func spawnCopy(data []byte, mime string) error {
	cmd := exec.Command("wl-copy", "--type", mime)
	cmd.Stdin = bytes.NewReader(data)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func serving(text string) bool {
	out, err := run(time.Second, "wl-paste", "--no-newline")
	return err == nil && string(out) == text
}

func restore(s clipSnapshot) {
	switch {
	case !s.known:
	case s.empty:
		_, _ = run(3*time.Second, "wl-copy", "--clear")
	default:
		_ = spawnCopy(s.data, s.mime)
	}
}

func (linuxBackend) Paste(text, chord, expected string, probe func() string) error {
	snap := snapshotClipboard()
	if err := spawnCopy([]byte(text), "text/plain"); err != nil {
		return fmt.Errorf("clipboard unavailable: %w", err)
	}
	time.Sleep(pasteDelay)
	if !serving(text) {
		return errors.New("clipboard serve failed")
	}
	if expected != "" && probe != nil && probe() != expected {
		return insert.ErrFocusChanged
	}
	if !sendKey(chord) {
		return errors.New("no key sender worked (install ydotool; ydotool 1.x also needs ydotoold running)")
	}
	time.Sleep(pasteDelay)
	// After success, never overwrite a concurrent clipboard change.
	if serving(text) {
		restore(snap)
	}
	return nil
}

func (linuxBackend) Copy(text string) error {
	if err := spawnCopy([]byte(text), "text/plain"); err != nil {
		return err
	}
	time.Sleep(pasteDelay)
	if !serving(text) {
		return errors.New("clipboard serve failed")
	}
	return nil
}

func (linuxBackend) Key(chord string) error {
	if !sendKey(chord) {
		return errors.New("no key sender available")
	}
	return nil
}

// sendKey injects only through senders that reach the active desktop;
// xdotool would target Xwayland's last window under Wayland.
func sendKey(chord string) bool {
	var senders [][]string
	if args, err := ydotoolKey(chord); err == nil {
		senders = append(senders, append([]string{"ydotool"}, args...))
	}
	if os.Getenv("XDG_SESSION_TYPE") != "wayland" {
		senders = append(senders, []string{"xdotool", "key", chord})
	} else {
		parts := strings.Split(chord, "+")
		w := []string{"wtype"}
		for _, m := range parts[:len(parts)-1] {
			w = append(w, "-M", m)
		}
		w = append(w, "-k", parts[len(parts)-1])
		for i := len(parts) - 2; i >= 0; i-- {
			w = append(w, "-m", parts[i])
		}
		senders = append(senders, w)
	}
	for _, argv := range senders {
		if _, err := run(3*time.Second, argv[0], argv[1:]...); err == nil {
			return true
		}
	}
	return false
}

// ydotoolKey returns ydotool's arguments for chord. ydotool 1.x takes raw
// evdev keycodes and needs its ydotoold daemon; 0.1.x (Ubuntu 24.04,
// Debian 12) takes key names. 1.x ignores key names yet still exits 0, so
// the paste would vanish unnoticed: ask which one is installed. Only 1.x
// names its YDOTOOL_SOCKET in the help, which neither needs a daemon for.
func ydotoolKey(chord string) ([]string, error) {
	help, _ := run(time.Second, "ydotool", "help")
	if !bytes.Contains(help, []byte("YDOTOOL_SOCKET")) {
		return []string{"key", chord}, nil
	}
	return evdevKeys(chord)
}

// evdevCodes are the Linux input keycodes (linux/input-event-codes.h) of
// every key Voice IO sends.
var evdevCodes = map[string]int{"ctrl": 29, "shift": 42, "enter": 28, "insert": 110, "v": 47}

// evdevKeys presses chord's keys in order and releases them in reverse, as
// ydotool 1.x KEYCODE:STATE arguments.
func evdevKeys(chord string) ([]string, error) {
	parts := strings.Split(strings.ToLower(chord), "+")
	args := make([]string, 1, 1+2*len(parts))
	args[0] = "key"
	for _, p := range parts {
		code, ok := evdevCodes[p]
		if !ok {
			return nil, fmt.Errorf("no keycode for %q", p)
		}
		args = append(args, fmt.Sprintf("%d:1", code))
	}
	for i := len(parts) - 1; i >= 0; i-- {
		args = append(args, fmt.Sprintf("%d:0", evdevCodes[parts[i]]))
	}
	return args, nil
}

// ---- hotkeys (GNOME custom keybindings) ----

// HotkeysNeedRegistration is false: GNOME owns shortcuts once applied.
const HotkeysNeedRegistration = false

// RegisterHotkeys is unsupported on GNOME; use ApplyGNOMEShortcuts.
func RegisterHotkeys(config.ParsedHotkeys, func(HotkeyAction, bool)) error { return ErrUnsupported }

// FnEvent is an fn key transition (unsupported on Linux).
type FnEvent int

const (
	FnReleased FnEvent = 0
	FnPressed  FnEvent = 1
	FnChord    FnEvent = 2
)

// StartFnMonitor is unsupported on Linux: the fn key never reaches apps.
func StartFnMonitor(func(FnEvent)) {}

// StopFnMonitor is a no-op on Linux.
func StopFnMonitor() {}

// FnKeyAction is unknown on Linux.
func FnKeyAction() int { return -1 }

// FnSupported reports whether hold-fn dictation exists on this system.
const FnSupported = false

// UnregisterHotkeys is a no-op on GNOME.
func UnregisterHotkeys() {}

const mediaKeys = "org.gnome.settings-daemon.plugins.media-keys"

var owned = map[HotkeyAction]struct{ path, name string }{
	ActionPush:   {"/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/voiceio-ptt/", "Voice push-to-talk"},
	ActionToggle: {"/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/voiceio-toggle/", "Voice on/off"},
	ActionCancel: {"/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/voiceio-cancel/", "Voice cancel"},
}

// Gsettings runs gsettings (replaceable in tests).
var Gsettings = func(args ...string) (string, error) {
	out, err := run(10*time.Second, "gsettings", args...)
	return strings.TrimSpace(string(out)), err
}

func customPaths() ([]string, error) {
	raw, err := Gsettings("get", mediaKeys, "custom-keybindings")
	if err != nil {
		return nil, err
	}
	raw = strings.TrimPrefix(raw, "@as ")
	var paths []string
	if err := json.Unmarshal([]byte(strings.ReplaceAll(raw, "'", `"`)), &paths); err != nil {
		return nil, errors.New("unexpected GNOME custom-keybindings value")
	}
	return paths, nil
}

func setKeybinding(action HotkeyAction, binding, command string) error {
	entry := owned[action]
	paths, err := customPaths()
	if err != nil {
		return err
	}
	schema := mediaKeys + ".custom-keybinding:" + entry.path
	// Complete the entry before listing it so GNOME never sees half of it.
	for _, kv := range [][2]string{{"name", entry.name}, {"binding", binding}, {"command", command}} {
		if _, err := Gsettings("set", schema, kv[0], kv[1]); err != nil {
			return err
		}
	}
	for _, p := range paths {
		if p == entry.path {
			return nil
		}
	}
	list, _ := json.Marshal(append(paths, entry.path))
	_, err = Gsettings("set", mediaKeys, "custom-keybindings", string(list))
	return err
}

// ApplyGNOMEShortcuts registers voice-io's three shortcuts, leaving every
// other custom keybinding untouched.
func ApplyGNOMEShortcuts(keys config.ParsedHotkeys, executable string) error {
	for _, item := range []struct {
		action HotkeyAction
		key    config.Hotkey
	}{{ActionPush, keys.Push}, {ActionToggle, keys.Toggle}, {ActionCancel, keys.Cancel}} {
		command := fmt.Sprintf("'%s' ctl %s", strings.ReplaceAll(executable, "'", `'\''`), item.action)
		if err := setKeybinding(item.action, item.key.GNOME(), command); err != nil {
			return fmt.Errorf("GNOME shortcut %s: %w", item.action, err)
		}
	}
	return nil
}

// RemoveGNOMEShortcuts unregisters only voice-io's keybindings.
func RemoveGNOMEShortcuts() error {
	paths, err := customPaths()
	if err != nil {
		return err
	}
	var keep []string
	for _, p := range paths {
		mine := false
		for _, e := range owned {
			mine = mine || p == e.path
		}
		if !mine {
			keep = append(keep, p)
		}
	}
	if len(keep) == len(paths) {
		return nil
	}
	if keep == nil {
		keep = []string{}
	}
	list, _ := json.Marshal(keep)
	_, err = Gsettings("set", mediaKeys, "custom-keybindings", string(list))
	return err
}

// ---- sounds ----

var (
	soundMu    sync.Mutex
	soundFiles = map[string]string{}
)

// RegisterSound caches a WAV for Play.
func RegisterSound(name string, wav []byte) {
	dir := filepath.Join(paths.CacheDir(), "cues")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, name+".wav")
	if err := os.WriteFile(path, wav, 0o600); err != nil {
		return
	}
	soundMu.Lock()
	soundFiles[name] = path
	soundMu.Unlock()
}

// PlaySound plays a cached cue with the first available player.
func PlaySound(name string) {
	soundMu.Lock()
	path := soundFiles[name]
	soundMu.Unlock()
	if path == "" {
		return
	}
	go func() {
		for _, argv := range [][]string{{"pw-play", path}, {"paplay", path}, {"aplay", "-q", path}} {
			if _, err := exec.LookPath(argv[0]); err != nil {
				continue
			}
			if _, err := run(5*time.Second, argv[0], argv[1:]...); err == nil {
				return
			}
		}
	}()
}

// ---- permissions / dependencies ----

func have(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func focusBridgeActive() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	var owner string
	return conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, focusBus).Store(&owner) == nil
}

// Permissions reports desktop dependencies.
func Permissions() []Permission {
	state := func(ok bool) string {
		if ok {
			return "granted"
		}
		return "missing"
	}
	out := []Permission{
		{ID: "clipboard", Title: "Clipboard (wl-clipboard)", Required: true, State: state(have("wl-copy") && have("wl-paste")),
			Detail: "Moves finished text to the clipboard for pasting.", Action: "install"},
		{ID: "keys", Title: "Key sender (ydotool)", Required: true, State: state(have("ydotool") || have("wtype")),
			Detail: "Sends the paste shortcut to the focused app. ydotool 1.x also needs ydotoold running.", Action: "install"},
	}
	if os.Getenv("XDG_SESSION_TYPE") == "wayland" {
		bridge := Permission{ID: "focus", Title: "GNOME focus bridge", Detail: "Verifies the target window " +
			"before pasting and allows the spoken end phrase to press Enter. Enable with: gnome-extensions enable gnome_focus@voice-io"}
		if focusBridgeActive() {
			bridge.State = "granted"
		} else {
			bridge.State = "optional"
		}
		out = append(out, bridge)
	}
	return out
}

// RequestPermission is unsupported: Linux dependencies are installed by the user.
func RequestPermission(string) error { return ErrUnsupported }

// OpenPermissionSettings is unsupported on Linux.
func OpenPermissionSettings(string) error { return ErrUnsupported }

// ---- files and login item ----

// Reveal opens the folder containing path.
func Reveal(path string) error {
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}

// OpenPath opens path with its default app.
func OpenPath(path string) error { return exec.Command("xdg-open", path).Start() }

// OpenURL opens a URL in the default browser.
func OpenURL(url string) error { return exec.Command("xdg-open", url).Start() }

// LoginItem reports (supported, enabled, needs approval) for the systemd unit.
func LoginItem() (bool, bool, bool) {
	if !have("systemctl") {
		return false, false, false
	}
	if _, err := run(3*time.Second, "systemctl", "--user", "cat", "voice-io.service"); err != nil {
		return false, false, false
	}
	_, err := run(3*time.Second, "systemctl", "--user", "is-enabled", "--quiet", "voice-io.service")
	return true, err == nil, false
}

// SetLoginItem enables or disables the systemd user unit.
func SetLoginItem(enabled bool) error {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	_, err := run(10*time.Second, "systemctl", "--user", verb, "voice-io.service")
	return err
}
