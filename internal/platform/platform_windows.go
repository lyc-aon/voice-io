package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/insert"
)

const pasteHint = "Ctrl+V"

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	winmm    = windows.NewLazySystemDLL("winmm.dll")

	procGetForegroundWindow        = user32.NewProc("GetForegroundWindow")
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procGetClipboardFormatNameW    = user32.NewProc("GetClipboardFormatNameW")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procEnumClipboardFormats       = user32.NewProc("EnumClipboardFormats")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procSendInput                  = user32.NewProc("SendInput")
	procMapVirtualKeyW             = user32.NewProc("MapVirtualKeyW")
	procGetAsyncKeyState           = user32.NewProc("GetAsyncKeyState")
	procRegisterHotKey             = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey           = user32.NewProc("UnregisterHotKey")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procPostMessageW               = user32.NewProc("PostMessageW")
	procSetTimer                   = user32.NewProc("SetTimer")
	procKillTimer                  = user32.NewProc("KillTimer")
	procGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	procGlobalFree                 = kernel32.NewProc("GlobalFree")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procPlaySoundW                 = winmm.NewProc("PlaySoundW")
)

// pasteBlocked: Windows needs no grant to paste.
func pasteBlocked() (string, string) { return "", "" }

// SetMainLoopRunning is a no-op: Windows work runs on its own threads.
func SetMainLoopRunning(bool) {}

// ---- the platform thread ----------------------------------------------------
//
// A message-only window on a locked OS thread owns the clipboard while
// Voice IO writes it and receives global hotkeys. Work that must happen
// there runs through onThread.

const (
	wmTimer  = 0x0113
	wmHotkey = 0x0312
	wmCall   = 0x8000 + 1 // WM_APP + 1
)

var thread struct {
	once  sync.Once
	hwnd  uintptr
	calls chan func()
	err   error
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

func startThread() error {
	thread.once.Do(func() {
		thread.calls = make(chan func(), 16)
		ready := make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			class, _ := windows.UTF16PtrFromString("VoiceIOPlatform")
			var instance windows.Handle
			_ = windows.GetModuleHandleEx(0, nil, &instance)
			wc := wndClassEx{wndProc: windows.NewCallback(threadProc), instance: instance, className: class}
			wc.size = uint32(unsafe.Sizeof(wc))
			if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
				ready <- fmt.Errorf("register window class: %w", err)
				return
			}
			const hwndMessage = ^uintptr(2) // HWND_MESSAGE (-3)
			hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(class)), 0, 0, 0, 0, 0, 0,
				hwndMessage, 0, uintptr(instance), 0)
			if hwnd == 0 {
				ready <- fmt.Errorf("create message window: %w", err)
				return
			}
			thread.hwnd = hwnd
			ready <- nil
			var m msg
			for {
				if r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
					return
				}
				procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
			}
		}()
		thread.err = <-ready
	})
	return thread.err
}

// onThread runs fn on the platform thread and waits for it.
func onThread(fn func()) error {
	if err := startThread(); err != nil {
		return err
	}
	done := make(chan struct{})
	thread.calls <- func() { fn(); close(done) }
	procPostMessageW.Call(thread.hwnd, wmCall, 0, 0)
	<-done
	return nil
}

func threadProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch message {
	case wmCall:
		for {
			select {
			case fn := <-thread.calls:
				fn()
			default:
				return 0
			}
		}
	case wmHotkey:
		hotkeyPressed(int(wParam))
		return 0
	case wmTimer:
		hotkeyPoll(int(wParam))
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

// ---- focus ------------------------------------------------------------------

// probeFocus names the foreground window as "app|pid|hwnd", app being its
// executable's name.
func probeFocus() string {
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return ""
	}
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(windows.HWND(hwnd), &pid); err != nil || pid == 0 {
		return ""
	}
	app := processName(pid)
	if app == "" {
		return ""
	}
	return fmt.Sprintf("%s|%d|%x", app, pid, hwnd)
}

func processName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	name := filepath.Base(windows.UTF16ToString(buf[:n]))
	return strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
}

// pasteChord is Ctrl+V everywhere: Windows Terminal and the console accept
// it too.
func pasteChord(string) string { return "ctrl+v" }

// ---- clipboard ----------------------------------------------------------------

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
	maxSnapshot   = 64 << 20
)

// Handle-based formats cannot be copied as bytes; Windows synthesizes the
// text and bitmap variants from the ones kept.
var skipFormats = map[uint32]bool{1: true, 2: true, 3: true, 7: true, 9: true, 14: true,
	0x80: true, 0x82: true, 0x83: true, 0x8E: true}

// oleFormats point at the copying app's live OLE data object; once the
// clipboard is emptied that object is gone, so restoring them would hand
// OLE readers a dangling reference.
var oleFormats = map[string]bool{"DataObject": true, "Ole Private Data": true}

func registeredName(id uint32) string {
	if id < 0xC000 {
		return ""
	}
	var buf [128]uint16
	n, _, _ := procGetClipboardFormatNameW.Call(uintptr(id), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:n])
}

var excludeFormat = sync.OnceValue(func() uint32 {
	name, _ := windows.UTF16PtrFromString("ExcludeClipboardContentFromMonitorProcessing")
	r, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(name)))
	return uint32(r)
})

type clipFormat struct {
	id   uint32
	data []byte
}

type clipSnapshot struct {
	known   bool // false: the clipboard could not be read; never overwrite it
	formats []clipFormat
}

// openClipboard retries briefly: other apps hold the clipboard for moments.
func openClipboard() error {
	var err error
	for range 20 {
		var r uintptr
		if r, _, err = procOpenClipboard.Call(thread.hwnd); r != 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("clipboard busy: %w", err)
}

// globalMem views n bytes of memory that GlobalLock returned. It lies
// outside the Go heap, so the address is safe to hold as a uintptr.
func globalMem(p uintptr, n int) []byte { return unsafe.Slice((*byte)(unsafe.Add(nil, p)), n) }

func globalBytes(h uintptr) ([]byte, bool) {
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 || size > maxSnapshot {
		return nil, false
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return nil, false
	}
	defer procGlobalUnlock.Call(h)
	return append([]byte(nil), globalMem(p, int(size))...), true
}

func globalAlloc(data []byte) uintptr {
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(max(len(data), 1)))
	if h == 0 {
		return 0
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return 0
	}
	copy(globalMem(p, len(data)), data)
	procGlobalUnlock.Call(h)
	return h
}

// setData hands a copy of data to the open clipboard as format id.
func setData(id uint32, data []byte) bool {
	h := globalAlloc(data)
	if h == 0 {
		return false
	}
	if r, _, _ := procSetClipboardData.Call(uintptr(id), h); r == 0 {
		procGlobalFree.Call(h)
		return false
	}
	return true
}

func snapshotClipboard() (s clipSnapshot) {
	_ = onThread(func() {
		if openClipboard() != nil {
			return
		}
		defer procCloseClipboard.Call()
		total := 0
		for id := uint32(0); ; {
			r, _, _ := procEnumClipboardFormats.Call(uintptr(id))
			if id = uint32(r); id == 0 {
				break
			}
			if skipFormats[id] || oleFormats[registeredName(id)] {
				continue
			}
			h, _, _ := procGetClipboardData.Call(uintptr(id))
			if h == 0 {
				continue
			}
			data, ok := globalBytes(h)
			if !ok {
				continue
			}
			if total += len(data); total > maxSnapshot {
				return
			}
			s.formats = append(s.formats, clipFormat{id, data})
		}
		s.known = true
	})
	return s
}

// writeClipboard replaces the clipboard with text and returns its sequence
// number once closed (closing advances it too). A transient write is kept
// out of clipboard history and cloud sync.
func writeClipboard(text string, transient bool) (uint32, error) {
	utf16, err := windows.UTF16FromString(text)
	if err != nil {
		return 0, err
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(&utf16[0])), len(utf16)*2)
	var seq uint32
	if terr := onThread(func() {
		if err = openClipboard(); err != nil {
			return
		}
		procEmptyClipboard.Call()
		if !setData(cfUnicodeText, data) {
			err = errors.New("clipboard rejected the transcript")
		} else if transient {
			setData(excludeFormat(), []byte{0})
		}
		procCloseClipboard.Call()
		seq = clipboardSequence()
	}); terr != nil {
		return 0, terr
	}
	return seq, err
}

func restoreClipboard(s clipSnapshot) {
	if !s.known {
		return
	}
	_ = onThread(func() {
		if openClipboard() != nil {
			return
		}
		defer procCloseClipboard.Call()
		procEmptyClipboard.Call()
		for _, f := range s.formats {
			setData(f.id, f.data)
		}
		if len(s.formats) > 0 {
			setData(excludeFormat(), []byte{0})
		}
	})
}

func clipboardSequence() uint32 {
	r, _, _ := procGetClipboardSequenceNumber.Call()
	return uint32(r)
}

// ---- keys -------------------------------------------------------------------

const (
	vkShift   = 0x10
	vkControl = 0x11
	vkMenu    = 0x12
	vkLWin    = 0x5B
	vkRWin    = 0x5C
)

var vkNames = map[string]uint16{
	"ctrl": vkControl, "control": vkControl, "shift": vkShift, "alt": vkMenu, "option": vkMenu,
	"super": vkLWin, "win": vkLWin, "cmd": vkLWin, "meta": vkLWin,
	"space": 0x20, "enter": 0x0D, "return": 0x0D, "tab": 0x09, "escape": 0x1B, "backspace": 0x08,
	"delete": 0x2E, "insert": 0x2D, "home": 0x24, "end": 0x23, "pageup": 0x21, "pagedown": 0x22,
	"left": 0x25, "up": 0x26, "right": 0x27, "down": 0x28,
}

// extendedKeys need KEYEVENTF_EXTENDEDKEY to reach apps as the navigation
// cluster rather than the numeric keypad.
var extendedKeys = map[uint16]bool{0x2D: true, 0x2E: true, 0x24: true, 0x23: true, 0x21: true,
	0x22: true, 0x25: true, 0x26: true, 0x27: true, 0x28: true, vkLWin: true, vkRWin: true}

// virtualKey maps a config key name to a Windows virtual-key code.
func virtualKey(name string) (uint16, bool) {
	name = strings.ToLower(name)
	if vk, ok := vkNames[name]; ok {
		return vk, true
	}
	if len(name) == 1 && (name[0] >= 'a' && name[0] <= 'z' || name[0] >= '0' && name[0] <= '9') {
		return uint16(strings.ToUpper(name)[0]), true
	}
	var n int
	if _, err := fmt.Sscanf(name, "f%d", &n); err == nil && n >= 1 && n <= 24 {
		return uint16(0x70 + n - 1), true
	}
	return 0, false
}

// keyInput mirrors INPUT with a KEYBDINPUT (64-bit layout).
type keyInput struct {
	typ   uint32
	_     uint32
	vk    uint16
	scan  uint16
	flags uint32
	time  uint32
	_     uint32
	extra uintptr
	_     [8]byte
}

func keyEvent(vk uint16, up bool) keyInput {
	scan, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0) // MAPVK_VK_TO_VSC
	in := keyInput{typ: 1, vk: vk, scan: uint16(scan)}
	if up {
		in.flags |= 0x0002 // KEYEVENTF_KEYUP
	}
	if extendedKeys[vk] {
		in.flags |= 0x0001 // KEYEVENTF_EXTENDEDKEY
	}
	return in
}

func keyDown(vk int) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

// waitForModifiers lets go of a held shortcut before keys are sent, so the
// paste chord does not become Ctrl+Alt+V.
func waitForModifiers() {
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !keyDown(vkShift) && !keyDown(vkControl) && !keyDown(vkMenu) && !keyDown(vkLWin) && !keyDown(vkRWin) {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// sendChord presses chord's keys in order and releases them in reverse.
func sendChord(chord string) error {
	var vks []uint16
	for _, part := range strings.Split(chord, "+") {
		vk, ok := virtualKey(part)
		if !ok {
			return fmt.Errorf("unsupported key %q in %q", part, chord)
		}
		vks = append(vks, vk)
	}
	waitForModifiers()
	ins := make([]keyInput, 0, 2*len(vks))
	for _, vk := range vks {
		ins = append(ins, keyEvent(vk, false))
	}
	for i := len(vks) - 1; i >= 0; i-- {
		ins = append(ins, keyEvent(vks[i], true))
	}
	n, _, err := procSendInput.Call(uintptr(len(ins)), uintptr(unsafe.Pointer(&ins[0])), unsafe.Sizeof(ins[0]))
	if int(n) != len(ins) {
		return fmt.Errorf("keys were blocked: %w", err)
	}
	return nil
}

// ---- paste backend ----------------------------------------------------------

// restoreDelay gives the target app time to read the clipboard before the
// previous contents return.
const restoreDelay = 300 * time.Millisecond

type windowsBackend struct{}

// NewBackend returns the Win32 clipboard and SendInput backend.
func NewBackend() insert.Backend { return windowsBackend{} }

// Paste puts text on the clipboard, re-checks the target window and sends
// the paste chord, then restores the previous clipboard unless something
// else changed it meanwhile. Windows cannot send keys into apps running as
// administrator from a normal process; those pastes have no effect.
func (windowsBackend) Paste(text, chord, expected string, probe func() string) error {
	snap := snapshotClipboard()
	seq, err := writeClipboard(text, true)
	if err != nil {
		return fmt.Errorf("clipboard unavailable: %w", err)
	}
	if expected != "" && probe != nil && probe() != expected {
		return insert.ErrFocusChanged
	}
	if err := sendChord(chord); err != nil {
		return err
	}
	time.Sleep(restoreDelay)
	if clipboardSequence() == seq {
		restoreClipboard(snap)
	}
	return nil
}

func (windowsBackend) Copy(text string) error {
	_, err := writeClipboard(text, false)
	return err
}

func (windowsBackend) Key(chord string) error { return sendChord(chord) }

// ---- hotkeys ----------------------------------------------------------------

// HotkeysNeedRegistration is true: Voice IO registers its own shortcuts.
const HotkeysNeedRegistration = true

const (
	modAlt      = 0x0001
	modControl  = 0x0002
	modShift    = 0x0004
	modWin      = 0x0008
	modNoRepeat = 0x4000

	errHotkeyAlreadyRegistered = 1409
)

var hotkeys struct {
	mu      sync.Mutex
	handler func(HotkeyAction, bool)
	actions map[int]HotkeyAction
	vks     map[int]uint16
	held    map[int]bool
}

// RegisterHotkeys installs global shortcuts. handler receives presses and,
// once the key is let go, releases. Failures name the shortcut.
func RegisterHotkeys(keys config.ParsedHotkeys, handler func(HotkeyAction, bool)) error {
	var errs []error
	err := onThread(func() {
		unregisterAll()
		hotkeys.mu.Lock()
		hotkeys.handler = handler
		hotkeys.actions = map[int]HotkeyAction{1: ActionPush, 2: ActionToggle, 3: ActionCancel}
		hotkeys.vks = map[int]uint16{}
		hotkeys.held = map[int]bool{}
		hotkeys.mu.Unlock()
		for id, hk := range map[int]config.Hotkey{1: keys.Push, 2: keys.Toggle, 3: keys.Cancel} {
			vk, ok := virtualKey(hk.Key)
			if !ok {
				errs = append(errs, fmt.Errorf("%s: key %q is not available on this keyboard", hk.Display("windows"), hk.Key))
				continue
			}
			mods := uintptr(modNoRepeat)
			if hk.Mods&config.ModCtrl != 0 {
				mods |= modControl
			}
			if hk.Mods&config.ModAlt != 0 {
				mods |= modAlt
			}
			if hk.Mods&config.ModShift != 0 {
				mods |= modShift
			}
			if hk.Mods&config.ModSuper != 0 {
				mods |= modWin
			}
			if r, _, e := procRegisterHotKey.Call(thread.hwnd, uintptr(id), mods, uintptr(vk)); r == 0 {
				if errno, ok := e.(syscall.Errno); ok && errno == errHotkeyAlreadyRegistered {
					errs = append(errs, fmt.Errorf("%s is already used by another app", hk.Display("windows")))
				} else {
					errs = append(errs, fmt.Errorf("%s could not be registered (%v)", hk.Display("windows"), e))
				}
				continue
			}
			hotkeys.mu.Lock()
			hotkeys.vks[id] = vk
			hotkeys.mu.Unlock()
		}
	})
	if err != nil {
		return err
	}
	return errors.Join(errs...)
}

// unregisterAll runs on the platform thread.
func unregisterAll() {
	hotkeys.mu.Lock()
	defer hotkeys.mu.Unlock()
	for id := range hotkeys.vks {
		procUnregisterHotKey.Call(thread.hwnd, uintptr(id))
		procKillTimer.Call(thread.hwnd, uintptr(id))
	}
	hotkeys.vks = nil
	hotkeys.held = nil
}

// UnregisterHotkeys removes every shortcut (while recording a new one).
func UnregisterHotkeys() {
	if thread.hwnd != 0 {
		_ = onThread(unregisterAll)
	}
}

// hotkeyPressed reports a press and polls for the release, which Windows
// does not report for hotkeys.
func hotkeyPressed(id int) {
	hotkeys.mu.Lock()
	h, action, held := hotkeys.handler, hotkeys.actions[id], hotkeys.held[id]
	if hotkeys.held != nil {
		hotkeys.held[id] = true
	}
	hotkeys.mu.Unlock()
	if h == nil || action == "" || held {
		return
	}
	procSetTimer.Call(thread.hwnd, uintptr(id), 15, 0)
	h(action, true)
}

func hotkeyPoll(id int) {
	hotkeys.mu.Lock()
	h, action, vk := hotkeys.handler, hotkeys.actions[id], hotkeys.vks[id]
	hotkeys.mu.Unlock()
	if vk != 0 && keyDown(int(vk)) {
		return
	}
	procKillTimer.Call(thread.hwnd, uintptr(id))
	hotkeys.mu.Lock()
	if hotkeys.held != nil {
		hotkeys.held[id] = false
	}
	hotkeys.mu.Unlock()
	if h != nil && action != "" {
		h(action, false)
	}
}

// FnEvent is an fn key transition (Windows apps never see the fn key).
type FnEvent int

const (
	FnReleased FnEvent = 0
	FnPressed  FnEvent = 1
	FnChord    FnEvent = 2
)

// StartFnMonitor is unsupported on Windows.
func StartFnMonitor(func(FnEvent)) {}

// StopFnMonitor is a no-op on Windows.
func StopFnMonitor() {}

// FnKeyAction is unknown on Windows.
func FnKeyAction() int { return -1 }

// FnSupported reports whether hold-fn dictation exists on this system.
const FnSupported = false

// ---- sounds -----------------------------------------------------------------

var sounds = struct {
	sync.Mutex
	m map[string]uintptr
}{m: map[string]uintptr{}}

// RegisterSound copies a WAV into memory that stays put while it plays.
func RegisterSound(name string, wav []byte) {
	if len(wav) == 0 {
		return
	}
	h := globalAlloc(wav)
	if h == 0 {
		return
	}
	p, _, _ := procGlobalLock.Call(h) // locked for the life of the process
	sounds.Lock()
	sounds.m[name] = p
	sounds.Unlock()
}

// PlaySound plays a registered sound without blocking.
func PlaySound(name string) {
	sounds.Lock()
	p := sounds.m[name]
	sounds.Unlock()
	if p != 0 {
		const sndAsync, sndNoDefault, sndMemory = 0x0001, 0x0002, 0x0004
		procPlaySoundW.Call(p, 0, sndAsync|sndNoDefault|sndMemory)
	}
}

// ---- permissions --------------------------------------------------------------

const micConsent = `Software\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`

// micDenied reports whether Windows privacy settings block desktop apps
// from the microphone, for this user or the whole device.
func micDenied() bool {
	for _, k := range []struct {
		root registry.Key
		path string
	}{{registry.LOCAL_MACHINE, micConsent}, {registry.CURRENT_USER, micConsent},
		{registry.CURRENT_USER, micConsent + `\NonPackaged`}} {
		key, err := registry.OpenKey(k.root, k.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, err := key.GetStringValue("Value")
		key.Close()
		if err == nil && strings.EqualFold(v, "Deny") {
			return true
		}
	}
	return false
}

// Permissions reports the privacy grants voice-io needs.
func Permissions() []Permission {
	mic := Permission{ID: "microphone", Title: "Microphone", Required: true,
		Detail: "Hear your voice. Audio never leaves this PC.", State: "granted"}
	if micDenied() {
		mic.State, mic.Action = "denied", "settings"
	}
	return []Permission{mic}
}

// RequestPermission opens the privacy page: Windows has no prompt for
// desktop apps.
func RequestPermission(id string) error { return OpenPermissionSettings(id) }

// OpenPermissionSettings opens the matching Windows Settings page.
func OpenPermissionSettings(id string) error {
	if id != "microphone" {
		return ErrUnsupported
	}
	return OpenURL("ms-settings:privacy-microphone")
}

// ---- files and login item ---------------------------------------------------

func shellOpen(target string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

// Reveal selects path in File Explorer.
func Reveal(path string) error {
	explorer := filepath.Join(os.Getenv("WINDIR"), "explorer.exe")
	cmd := &syscall.SysProcAttr{CmdLine: fmt.Sprintf(`"%s" /select,"%s"`, explorer, path)}
	p, err := os.StartProcess(explorer, nil, &os.ProcAttr{Sys: cmd})
	if err != nil {
		return err
	}
	return p.Release()
}

// OpenPath opens path with its default app.
func OpenPath(path string) error { return shellOpen(path) }

// OpenURL opens a URL in the default browser (or a ms-settings: page).
func OpenURL(url string) error { return shellOpen(url) }

const (
	runKey     = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue   = "Voice IO"
	appExeName = "Voice IO.exe"
)

// appExe is the windowless build beside this executable, which the login
// item starts; without it, starting at login would open a console window.
func appExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), appExeName)
	if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return p
}

// LoginItem reports (supported, enabled, needs approval).
func LoginItem() (bool, bool, bool) {
	exe := appExe()
	if exe == "" {
		return false, false, false
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return true, false, false
	}
	defer key.Close()
	_, _, err = key.GetStringValue(runValue)
	return true, err == nil, false
}

// SetLoginItem adds or removes Voice IO from the user's Run key.
func SetLoginItem(enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if !enabled {
		if err := key.DeleteValue(runValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe := appExe()
	if exe == "" {
		return fmt.Errorf("the app build %q is missing beside voice-io.exe", appExeName)
	}
	return key.SetStringValue(runValue, `"`+exe+`"`)
}
