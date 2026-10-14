//go:build darwin

package platform

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -fmodules -Wno-deprecated-declarations
#cgo LDFLAGS: -framework AppKit -framework ApplicationServices -framework AVFoundation -framework Carbon -framework ServiceManagement
#include <stdlib.h>
#include "darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/insert"
)

const pasteHint = "⌘V"

func probeFocus() string {
	var reason C.int
	s := C.vio_focus_identity(&reason)
	if s == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(s))
	return C.GoString(s)
}

func pasteChord(string) string { return "cmd+v" }

func pasteBlocked() (string, string) {
	if C.vio_ax_trusted(0) == 1 {
		return "", ""
	}
	return "accessibility", "allow Accessibility to paste"
}

// SetMainLoopRunning tells AppKit helpers to hop to the main queue.
func SetMainLoopRunning(running bool) {
	v := C.int(0)
	if running {
		v = 1
	}
	C.vio_set_main_loop_running(v)
}

type macBackend struct{}

// NewBackend returns the macOS pasteboard/keyboard backend.
func NewBackend() insert.Backend { return macBackend{} }

// Paste puts text on the clipboard and sends ⌘V. With an expected focus it
// first re-checks that the same window still has focus; without one (an app
// that exposes no focused window) it pastes at the current keyboard focus.
func (macBackend) Paste(text, _ string, expected string, _ func() string) error {
	ct := C.CString(text)
	defer C.free(unsafe.Pointer(ct))
	var ce *C.char
	if expected != "" {
		ce = C.CString(expected)
		defer C.free(unsafe.Pointer(ce))
	}
	switch C.vio_paste(ct, ce) {
	case 0:
		return nil
	case 1:
		return insert.ErrFocusChanged
	case 2:
		return errors.New("macOS clipboard rejected transcript")
	case 3:
		return errors.New("macOS Accessibility permission required")
	case 4:
		return errors.New("cannot snapshot the current clipboard; paste cancelled")
	}
	return errors.New("paste failed")
}

func (macBackend) Copy(text string) error {
	ct := C.CString(text)
	defer C.free(unsafe.Pointer(ct))
	if C.vio_copy(ct) != 0 {
		return errors.New("macOS clipboard rejected transcript")
	}
	return nil
}

func (macBackend) Key(chord string) error {
	var code C.int
	switch chord {
	case "enter":
		code = 0
	case "ctrl+enter":
		code = 1
	case "cmd+v":
		code = 2
	default:
		return fmt.Errorf("unsupported macOS key chord %q", chord)
	}
	if C.vio_key(code) != 0 {
		return errors.New("macOS Accessibility permission required")
	}
	return nil
}

// ---- hotkeys ----

var (
	hotkeyMu      sync.Mutex
	hotkeyHandler func(HotkeyAction, bool)
	hotkeyActions = map[int]HotkeyAction{}
)

//export vioHotkeyEvent
func vioHotkeyEvent(id, pressed C.int) {
	hotkeyMu.Lock()
	h, action := hotkeyHandler, hotkeyActions[int(id)]
	hotkeyMu.Unlock()
	if h != nil && action != "" {
		h(action, pressed != 0)
	}
}

// RegisterHotkeys installs global shortcuts (requires the AppKit loop).
// handler receives press and release events. Failures name the shortcut.
func RegisterHotkeys(keys config.ParsedHotkeys, handler func(HotkeyAction, bool)) error {
	C.vio_hotkeys_clear()
	hotkeyMu.Lock()
	hotkeyHandler = handler
	hotkeyActions = map[int]HotkeyAction{1: ActionPush, 2: ActionToggle, 3: ActionCancel}
	hotkeyMu.Unlock()
	var errs []error
	for id, hk := range map[int]config.Hotkey{1: keys.Push, 2: keys.Toggle, 3: keys.Cancel} {
		mods := 0
		if hk.Mods&config.ModCtrl != 0 {
			mods |= 1
		}
		if hk.Mods&config.ModAlt != 0 {
			mods |= 2
		}
		if hk.Mods&config.ModShift != 0 {
			mods |= 4
		}
		if hk.Mods&config.ModSuper != 0 {
			mods |= 8
		}
		ck := C.CString(hk.Key)
		rc := C.vio_hotkey_register(C.int(id), ck, C.int(mods))
		C.free(unsafe.Pointer(ck))
		switch {
		case rc == -1:
			errs = append(errs, fmt.Errorf("%s: key %q is not available on this keyboard", hk.Display("darwin"), hk.Key))
		case rc == -9878: // eventHotKeyExistsErr
			errs = append(errs, fmt.Errorf("%s is already used by another app", hk.Display("darwin")))
		case rc != 0:
			errs = append(errs, fmt.Errorf("%s could not be registered (error %d)", hk.Display("darwin"), int(rc)))
		}
	}
	return errors.Join(errs...)
}

// FnEvent is an fn (🌐) key transition.
type FnEvent int

const (
	FnReleased FnEvent = 0
	FnPressed  FnEvent = 1
	FnChord    FnEvent = 2 // another key was pressed while fn was held
)

var fnHandler func(FnEvent)

//export vioFnEvent
func vioFnEvent(kind C.int) {
	hotkeyMu.Lock()
	h := fnHandler
	hotkeyMu.Unlock()
	if h != nil {
		h(FnEvent(kind))
	}
}

// StartFnMonitor reports fn key holds (needs Accessibility; runs on the
// AppKit loop).
func StartFnMonitor(handler func(FnEvent)) {
	hotkeyMu.Lock()
	fnHandler = handler
	hotkeyMu.Unlock()
	C.vio_fn_monitor_start()
}

// StopFnMonitor removes the fn key monitor.
func StopFnMonitor() { C.vio_fn_monitor_stop() }

// FnKeyAction reports the system "Press 🌐 key to" setting (0 = Do Nothing,
// 1 input source, 2 emoji, 3 dictation, -1 unset).
func FnKeyAction() int { return int(C.vio_fn_usage_type()) }

// FnSupported reports whether hold-fn dictation exists on this system.
const FnSupported = true

// UnregisterHotkeys removes every shortcut (while recording a new one).
func UnregisterHotkeys() { C.vio_hotkeys_clear() }

// HotkeysNeedRegistration reports whether shortcuts are registered by the
// app (macOS) rather than by the desktop (GNOME).
const HotkeysNeedRegistration = true

// ---- sounds ----

// RegisterSound loads a WAV for Play.
func RegisterSound(name string, wav []byte) {
	if len(wav) == 0 {
		return
	}
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	C.vio_sound_register(cn, unsafe.Pointer(&wav[0]), C.int(len(wav)))
}

// PlaySound plays a registered sound without blocking.
func PlaySound(name string) {
	cn := C.CString(name)
	defer C.free(unsafe.Pointer(cn))
	C.vio_sound_play(cn)
}

// ---- permissions ----

// Permissions reports the privacy grants voice-io needs.
func Permissions() []Permission {
	mic := Permission{ID: "microphone", Title: "Microphone", Required: true,
		Detail: "Hear your voice. Audio never leaves this Mac."}
	switch C.vio_mic_status() {
	case 3:
		mic.State = "granted"
	case 0:
		mic.State, mic.Action = "prompt", "request"
	default:
		mic.State, mic.Action = "denied", "settings"
	}
	ax := Permission{ID: "accessibility", Title: "Accessibility", Required: true,
		Detail: "Paste into the app you are using and press Return when you say the end phrase."}
	if C.vio_ax_trusted(0) == 1 {
		ax.State = "granted"
	} else {
		ax.State, ax.Action = "denied", "request"
	}
	return []Permission{mic, ax}
}

// axPrompted records that macOS's Accessibility prompt was shown this run.
var axPrompted atomic.Bool

// RequestPermission asks for a privacy grant: the system prompt the first
// time, System Settings after that (or when access was denied before).
func RequestPermission(id string) error {
	switch id {
	case "microphone":
		if C.vio_mic_status() == 0 {
			C.vio_mic_request()
			return nil
		}
		return OpenPermissionSettings(id)
	case "accessibility":
		if C.vio_ax_trusted(0) == 1 {
			return nil
		}
		if axPrompted.CompareAndSwap(false, true) {
			// Lists Voice IO in Privacy & Security and offers to open it.
			C.vio_ax_trusted(1)
			return nil
		}
		return OpenPermissionSettings(id)
	}
	return ErrUnsupported
}

// OpenPermissionSettings opens the matching System Settings pane.
func OpenPermissionSettings(id string) error {
	pane := map[string]string{
		"microphone":    "Privacy_Microphone",
		"accessibility": "Privacy_Accessibility",
	}[id]
	if pane == "" {
		return ErrUnsupported
	}
	cu := C.CString("x-apple.systempreferences:com.apple.preference.security?" + pane)
	defer C.free(unsafe.Pointer(cu))
	C.vio_open_url(cu)
	return nil
}

// ---- files and login item ----

// Reveal selects path in the file manager.
func Reveal(path string) error {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	C.vio_reveal(cp)
	return nil
}

// OpenPath opens path with its default app.
func OpenPath(path string) error {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	C.vio_open_path(cp)
	return nil
}

// OpenURL opens a web or settings URL.
func OpenURL(url string) error {
	cu := C.CString(url)
	defer C.free(unsafe.Pointer(cu))
	C.vio_open_url(cu)
	return nil
}

// LoginItem reports (supported, enabled, needs approval).
func LoginItem() (bool, bool, bool) {
	switch C.vio_login_item_status() {
	case 1:
		return true, true, false
	case 2:
		return true, true, true
	case 0:
		return true, false, false
	}
	return false, false, false
}

// SetLoginItem enables or disables launching at login.
func SetLoginItem(enabled bool) error {
	v := C.int(0)
	if enabled {
		v = 1
	}
	if msg := C.vio_login_item_set(v); msg != nil {
		defer C.free(unsafe.Pointer(msg))
		return errors.New(C.GoString(msg))
	}
	return nil
}
