package ui

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"gitlab.com/integral-inference/voice-io/internal/overlay"
	"gitlab.com/integral-inference/voice-io/internal/raster"
)

// The Win32 host: a layered, always-on-top overlay window that never takes
// focus, a notification-area icon with the status menu, and context menus.
// The overlay is drawn by internal/raster and shown with
// UpdateLayeredWindow. The canvas is click-through except over the pill's
// interactive areas, which a cursor poll tracks.

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")
	shell  = windows.NewLazySystemDLL("shell32.dll")

	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procGetMessageW                   = user32.NewProc("GetMessageW")
	procTranslateMessage              = user32.NewProc("TranslateMessage")
	procDispatchMessageW              = user32.NewProc("DispatchMessageW")
	procPostMessageW                  = user32.NewProc("PostMessageW")
	procPostQuitMessage               = user32.NewProc("PostQuitMessage")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procSetWindowPos                  = user32.NewProc("SetWindowPos")
	procUpdateLayeredWindow           = user32.NewProc("UpdateLayeredWindow")
	procSetTimer                      = user32.NewProc("SetTimer")
	procKillTimer                     = user32.NewProc("KillTimer")
	procGetCursorPos                  = user32.NewProc("GetCursorPos")
	procSetCapture                    = user32.NewProc("SetCapture")
	procReleaseCapture                = user32.NewProc("ReleaseCapture")
	procGetWindowLongPtrW             = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW             = user32.NewProc("SetWindowLongPtrW")
	procMonitorFromPoint              = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW               = user32.NewProc("GetMonitorInfoW")
	procGetDpiForWindow               = user32.NewProc("GetDpiForWindow")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procCreatePopupMenu               = user32.NewProc("CreatePopupMenu")
	procAppendMenuW                   = user32.NewProc("AppendMenuW")
	procDestroyMenu                   = user32.NewProc("DestroyMenu")
	procTrackPopupMenuEx              = user32.NewProc("TrackPopupMenuEx")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procGetForegroundWindow           = user32.NewProc("GetForegroundWindow")
	procLoadImageW                    = user32.NewProc("LoadImageW")
	procLoadIconW                     = user32.NewProc("LoadIconW")
	procRegisterWindowMessageW        = user32.NewProc("RegisterWindowMessageW")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procCreateCompatibleDC            = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection              = gdi32.NewProc("CreateDIBSection")
	procSelectObject                  = gdi32.NewProc("SelectObject")
	procDeleteObject                  = gdi32.NewProc("DeleteObject")
	procShellNotifyIconW              = shell.NewProc("Shell_NotifyIconW")
)

const (
	wsPopup         = 0x80000000
	wsExLayered     = 0x00080000
	wsExTopmost     = 0x00000008
	wsExToolWindow  = 0x00000080
	wsExNoActivate  = 0x08000000
	wsExTransparent = 0x00000020
	gwlExStyle      = ^uintptr(19) // -20

	swHide           = 0
	swShowNoActivate = 4
	swpNoSize        = 0x0001
	swpNoMove        = 0x0002
	swpNoActivate    = 0x0010
	hwndTopmost      = ^uintptr(0) // -1

	wmDestroy          = 0x0002
	wmClose            = 0x0010
	wmQueryEndSession  = 0x0011
	wmEndSession       = 0x0016
	wmSettingChange    = 0x001A
	wmMouseActivate    = 0x0021
	wmDisplayChange    = 0x007E
	wmTimer            = 0x0113
	wmMouseMove        = 0x0200
	wmLButtonDown      = 0x0201
	wmLButtonUp        = 0x0202
	wmRButtonDown      = 0x0204
	wmRButtonUp        = 0x0205
	wmCaptureChanged   = 0x0215
	wmDpiChanged       = 0x02E0
	wmApp              = 0x8000
	wmReady            = wmApp + 1
	wmFrame            = wmApp + 2
	wmQuit             = wmApp + 3
	wmOverlay          = wmApp + 4
	wmStatus           = wmApp + 5
	wmTray             = wmApp + 6
	maNoActivate       = 3
	mkLButton          = 0x0001
	timerFrame         = 1
	timerPointer       = 2
	tpmReturnCmd       = 0x0100
	tpmNoNotify        = 0x0080
	tpmRightButton     = 0x0002
	mfString           = 0x0000
	mfGrayed           = 0x0001
	mfChecked          = 0x0008
	mfPopup            = 0x0010
	mfSeparator        = 0x0800
	nimAdd             = 0
	nimModify          = 1
	nimDelete          = 2
	nifMessage         = 0x1
	nifIcon            = 0x2
	nifTip             = 0x4
	monitorDefaultPrim = 1
)

type point struct{ x, y int32 }

type rect struct{ left, top, right, bottom int32 }

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
	private uint32
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type monitorInfo struct {
	size    uint32
	monitor rect
	work    rect
	flags   uint32
}

type bitmapInfoHeader struct {
	size          uint32
	width         int32
	height        int32
	planes        uint16
	bitCount      uint16
	compression   uint32
	sizeImage     uint32
	xPelsPerMeter int32
	yPelsPerMeter int32
	clrUsed       uint32
	clrImportant  uint32
}

type notifyIconData struct {
	size            uint32
	hwnd            uintptr
	id              uint32
	flags           uint32
	callbackMessage uint32
	icon            uintptr
	tip             [128]uint16
	state           uint32
	stateMask       uint32
	info            [256]uint16
	version         uint32
	infoTitle       [64]uint16
	infoFlags       uint32
	guid            windows.GUID
	balloonIcon     uintptr
}

// win is the UI thread's state; only that thread touches it, except the
// atomics and the status string.
var win struct {
	instance windows.Handle
	owner    uintptr // hidden window: app messages, the tray icon and menus
	overlay  uintptr
	running  bool

	overlayEnabled bool
	visible        bool
	scale          float32
	pos            point
	canvas         *image.RGBA
	renderer       raster.Renderer
	memDC          uintptr
	bitmap         uintptr
	bits           []byte
	rects          []float32
	transparent    bool
	inside         bool
	dragging       bool
	lastCursor     point

	tray        bool
	trayCreated uint32
	menuActions []string
}

var (
	framePending atomic.Bool
	statusMu     sync.Mutex
	statusText   = "Voice IO"
	list         overlay.List
)

func wstr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// Supported reports whether a native UI exists: Win32 always does.
func Supported() bool { return true }

// Run creates the overlay and the notification-area icon and runs the
// message loop on the calling (main) thread until Quit.
func Run(h Hooks, o Options) {
	hooks = h
	procSetProcessDpiAwarenessContext.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2 (-4)
	_ = windows.GetModuleHandleEx(0, nil, &win.instance)
	proc := windows.NewCallback(wndProc)
	for _, name := range []string{"VoiceIOOwner", "VoiceIOOverlay"} {
		wc := wndClassEx{wndProc: proc, instance: win.instance, className: wstr(name)}
		wc.size = uint32(unsafe.Sizeof(wc))
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	}
	win.owner, _, _ = procCreateWindowExW.Call(wsExToolWindow, uintptr(unsafe.Pointer(wstr("VoiceIOOwner"))),
		uintptr(unsafe.Pointer(wstr("Voice IO"))), wsPopup, 0, 0, 0, 0, 0, 0, uintptr(win.instance), 0)
	win.overlay, _, _ = procCreateWindowExW.Call(wsExLayered|wsExTopmost|wsExToolWindow|wsExNoActivate|wsExTransparent,
		uintptr(unsafe.Pointer(wstr("VoiceIOOverlay"))), uintptr(unsafe.Pointer(wstr("Voice IO"))), wsPopup,
		0, 0, 0, 0, 0, 0, uintptr(win.instance), 0)
	if win.owner == 0 || win.overlay == 0 {
		return
	}
	win.transparent = true
	win.overlayEnabled = o.Overlay
	win.scale = dpiScale()
	r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(wstr("TaskbarCreated"))))
	win.trayCreated = uint32(r)
	if o.StatusItem {
		win.tray = addTrayIcon(nimAdd)
	}
	appearanceChanged()
	win.running = true
	procPostMessageW.Call(win.owner, wmReady, 0, 0)
	RequestFrame()
	var m msg
	for {
		if r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0); int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	win.running = false
	if win.tray {
		addTrayIcon(nimDelete)
	}
}

// Quit ends the message loop; Run returns.
func Quit() { procPostMessageW.Call(win.owner, wmQuit, 0, 0) }

// ReplyTerminate ends the loop after a session-ending quit was handled.
func ReplyTerminate() { Quit() }

// RequestFrame schedules overlay frames until the model stops animating.
func RequestFrame() {
	if win.owner != 0 && framePending.CompareAndSwap(false, true) {
		procPostMessageW.Call(win.owner, wmFrame, 0, 0)
	}
}

// SetOverlayEnabled shows or removes the overlay window.
func SetOverlayEnabled(enabled bool) {
	var w uintptr
	if enabled {
		w = 1
	}
	procPostMessageW.Call(win.owner, wmOverlay, w, 0)
}

var statusNames = map[string]string{"idle": "Ready", "recording": "Recording", "processing": "Transcribing",
	"off": "Voice input off", "setup": "Finish setup"}

// SetStatus shows the state in the notification-area icon's tooltip.
func SetStatus(state string) {
	statusMu.Lock()
	statusText = "Voice IO"
	if s := statusNames[state]; s != "" {
		statusText += " — " + s
	}
	statusMu.Unlock()
	procPostMessageW.Call(win.owner, wmStatus, 0, 0)
}

// ShowMenu pops up a context menu at the pointer. It must be called on the
// UI thread, as overlay actions are.
func ShowMenu(items []MenuItem, _, _ float32) { popupMenu(items) }

// OpenSettings opens Settings in the default browser; the token travels in
// the URL fragment, which never reaches the server or its logs.
func OpenSettings(url, token string) {
	_ = windows.ShellExecute(0, wstr("open"), wstr(url+"&token="+token), nil, nil, windows.SW_SHOWNORMAL)
}

// ---- window procedure -------------------------------------------------------

func lowSigned(v uintptr) int32  { return int32(int16(v & 0xFFFF)) }
func highSigned(v uintptr) int32 { return int32(int16((v >> 16) & 0xFFFF)) }

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	if hwnd == win.overlay {
		if r, ok := overlayMessage(message, wParam, lParam); ok {
			return r
		}
	} else if hwnd == win.owner {
		if r, ok := ownerMessage(message, wParam, lParam); ok {
			return r
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

func ownerMessage(message, wParam, lParam uintptr) (uintptr, bool) {
	switch message {
	case wmReady:
		if hooks.Ready != nil {
			hooks.Ready()
		}
	case wmFrame:
		if win.running && frame() {
			procSetTimer.Call(win.owner, timerFrame, 16, 0)
		}
	case wmTimer:
		switch wParam {
		case timerFrame:
			if !frame() {
				procKillTimer.Call(win.owner, timerFrame)
			}
		case timerPointer:
			pollPointer()
		}
	case wmOverlay:
		win.overlayEnabled = wParam != 0
		RequestFrame()
	case wmStatus:
		if win.tray {
			addTrayIcon(nimModify)
		}
	case wmTray:
		switch lParam {
		case wmLButtonUp, wmRButtonUp:
			if hooks.StatusMenu != nil {
				popupMenu(hooks.StatusMenu())
			}
		}
	case wmQuit:
		procDestroyWindow.Call(win.overlay)
		procPostQuitMessage.Call(0)
	case wmSettingChange:
		appearanceChanged()
		RequestFrame()
	case wmDisplayChange:
		RequestFrame()
	case wmQueryEndSession:
		return 1, true
	case wmEndSession:
		if wParam != 0 && hooks.Terminate != nil {
			hooks.Terminate()
		}
	case wmClose:
		return 0, true
	default:
		if win.trayCreated != 0 && uint32(message) == win.trayCreated && win.tray {
			addTrayIcon(nimAdd) // Explorer restarted
		}
		return 0, false
	}
	return 0, true
}

func overlayMessage(message, wParam, lParam uintptr) (uintptr, bool) {
	m := hooks.Model
	if m == nil {
		return 0, false
	}
	x, y := float32(lowSigned(lParam))/win.scale, float32(highSigned(lParam))/win.scale
	switch message {
	case wmMouseActivate:
		return maNoActivate, true
	case wmLButtonDown:
		procSetCapture.Call(win.overlay)
		win.dragging = true
		m.MouseDown(x, y, screenY(), 0)
	case wmRButtonDown:
		m.MouseDown(x, y, screenY(), 1)
	case wmMouseMove:
		if win.dragging && wParam&mkLButton != 0 {
			m.MouseDragged(screenY())
		} else {
			m.Pointer(x, y, true)
		}
	case wmLButtonUp:
		win.dragging = false
		procReleaseCapture.Call()
		m.MouseUp(x, y)
	case wmCaptureChanged:
		win.dragging = false
	case wmDpiChanged:
		win.scale = float32(wParam&0xFFFF) / 96
		RequestFrame()
	case wmDestroy:
		win.overlay = 0
	default:
		return 0, false
	}
	return 0, true
}

// screenY is the cursor's height on screen in logical pixels, for dragging.
func screenY() float32 {
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return float32(p.y) / win.scale
}

// ---- overlay frames -----------------------------------------------------------

func dpiScale() float32 {
	if win.overlay != 0 && procGetDpiForWindow.Find() == nil {
		if dpi, _, _ := procGetDpiForWindow.Call(win.overlay); dpi > 0 {
			return float32(dpi) / 96
		}
	}
	return 1
}

// workArea is the primary monitor's area outside the taskbar.
func workArea() rect {
	mon, _, _ := procMonitorFromPoint.Call(0, monitorDefaultPrim)
	mi := monitorInfo{size: uint32(unsafe.Sizeof(monitorInfo{}))}
	procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
	return mi.work
}

// frame advances the model one frame and shows it; it reports whether
// more frames should follow.
func frame() bool {
	framePending.Store(false)
	m := hooks.Model
	if m == nil || win.overlay == 0 {
		return false
	}
	f := m.Draw(&list)
	win.rects = m.InputRects(win.rects[:0])
	if !(win.overlayEnabled && f.Visible) {
		if win.visible {
			procShowWindow.Call(win.overlay, swHide)
			procKillTimer.Call(win.owner, timerPointer)
			win.visible = false
			setTransparent(true)
		}
		return false
	}
	if s := dpiScale(); s != win.scale {
		win.scale = s
	}
	wa := workArea()
	m.SetScreenHeight(float64(float32(wa.bottom-wa.top) / win.scale))
	w := int32(math.Ceil(float64(overlay.CanvasW * win.scale)))
	h := int32(math.Ceil(float64(overlay.CanvasH * win.scale)))
	win.pos = point{wa.right - w, wa.top + (wa.bottom-wa.top)/2 + int32(math.Round(f.OffsetY*float64(win.scale))) - h/2}
	if !present(w, h) {
		return false
	}
	if !win.visible {
		procSetWindowPos.Call(win.overlay, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
		procShowWindow.Call(win.overlay, swShowNoActivate)
		procSetTimer.Call(win.owner, timerPointer, 33, 0)
		win.visible = true
	}
	return f.Animating
}

// present renders the display list into the window's DIB and hands it to
// the compositor.
func present(w, h int32) bool {
	if win.canvas == nil || win.canvas.Rect.Dx() != int(w) || win.canvas.Rect.Dy() != int(h) {
		if !makeSurface(w, h) {
			return false
		}
	}
	win.renderer.Fonts = loadFonts()
	win.renderer.Draw(win.canvas, &list, win.scale)
	src, dst := win.canvas.Pix, win.bits
	for i := 0; i+3 < len(src); i += 4 {
		dst[i], dst[i+1], dst[i+2], dst[i+3] = src[i+2], src[i+1], src[i], src[i+3]
	}
	size := point{w, h}
	var zero point
	blend := uint32(0x01FF0000) // AC_SRC_OVER, 0, 255, AC_SRC_ALPHA
	r, _, _ := procUpdateLayeredWindow.Call(win.overlay, 0, uintptr(unsafe.Pointer(&win.pos)),
		uintptr(unsafe.Pointer(&size)), win.memDC, uintptr(unsafe.Pointer(&zero)), 0,
		uintptr(unsafe.Pointer(&blend)), 2) // ULW_ALPHA
	return r != 0
}

// makeSurface (re)creates the canvas and a top-down 32-bit DIB of the same
// size.
func makeSurface(w, h int32) bool {
	if win.bitmap != 0 {
		procDeleteObject.Call(win.bitmap)
		win.bitmap = 0
	}
	if win.memDC == 0 {
		win.memDC, _, _ = procCreateCompatibleDC.Call(0)
	}
	bi := bitmapInfoHeader{width: w, height: -h, planes: 1, bitCount: 32}
	bi.size = uint32(unsafe.Sizeof(bi))
	var bits unsafe.Pointer
	win.bitmap, _, _ = procCreateDIBSection.Call(win.memDC, uintptr(unsafe.Pointer(&bi)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if win.bitmap == 0 || bits == nil {
		return false
	}
	procSelectObject.Call(win.memDC, win.bitmap)
	win.bits = unsafe.Slice((*byte)(bits), int(w)*int(h)*4)
	win.canvas = image.NewRGBA(image.Rect(0, 0, int(w), int(h)))
	return true
}

// ---- pointer --------------------------------------------------------------------

func setTransparent(on bool) {
	if on == win.transparent || win.overlay == 0 {
		return
	}
	style, _, _ := procGetWindowLongPtrW.Call(win.overlay, gwlExStyle)
	if on {
		style |= wsExTransparent
	} else {
		style &^= wsExTransparent
	}
	procSetWindowLongPtrW.Call(win.overlay, gwlExStyle, style)
	win.transparent = on
}

func inRects(x, y float32) bool {
	for i := 0; i+3 < len(win.rects); i += 4 {
		r := win.rects[i : i+4]
		if x >= r[0] && x < r[0]+r[2] && y >= r[1] && y < r[1]+r[3] {
			return true
		}
	}
	return false
}

// pollPointer follows the cursor while the overlay shows: the window takes
// mouse input only over the pill's interactive areas and is click-through
// everywhere else.
func pollPointer() {
	m := hooks.Model
	if m == nil || win.dragging {
		return
	}
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	x := float32(p.x-win.pos.x) / win.scale
	y := float32(p.y-win.pos.y) / win.scale
	in := inRects(x, y)
	setTransparent(!in)
	if in != win.inside || (in && p != win.lastCursor) {
		m.Pointer(x, y, in)
	}
	win.inside, win.lastCursor = in, p
}

// ---- appearance ---------------------------------------------------------------

// appearanceChanged follows Settings › Personalization › Colors.
func appearanceChanged() {
	dark := false
	if k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE); err == nil {
		v, _, err := k.GetIntegerValue("AppsUseLightTheme")
		k.Close()
		dark = err == nil && v == 0
	}
	if hooks.Model != nil {
		hooks.Model.SetSystemDark(dark)
	}
}

// ---- notification area and menus -------------------------------------------------

func trayIcon() uintptr {
	cx, _, _ := procGetSystemMetrics.Call(49) // SM_CXSMICON
	cy, _, _ := procGetSystemMetrics.Call(50)
	if h, _, _ := procLoadImageW.Call(uintptr(win.instance), 1, 1, cx, cy, 0); h != 0 {
		return h // icon resource 1, embedded by scripts/build-windows
	}
	h, _, _ := procLoadIconW.Call(0, 32512) // IDI_APPLICATION
	return h
}

func addTrayIcon(op uintptr) bool {
	nid := notifyIconData{hwnd: win.owner, id: 1, flags: nifMessage | nifIcon | nifTip, callbackMessage: wmTray}
	nid.size = uint32(unsafe.Sizeof(nid))
	nid.icon = trayIcon()
	statusMu.Lock()
	tip, _ := windows.UTF16FromString(statusText)
	statusMu.Unlock()
	copy(nid.tip[:len(nid.tip)-1], tip)
	r, _, _ := procShellNotifyIconW.Call(op, uintptr(unsafe.Pointer(&nid)))
	return r != 0
}

// popupMenu shows items at the cursor and runs the chosen action.
func popupMenu(items []MenuItem) {
	if len(items) == 0 {
		return
	}
	win.menuActions = win.menuActions[:0]
	menu := buildMenu(items)
	defer procDestroyMenu.Call(menu)
	var p point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	// A menu closes on outside clicks only when its owner is in front;
	// give the previous app its focus back afterwards.
	prev, _, _ := procGetForegroundWindow.Call()
	procSetForegroundWindow.Call(win.owner)
	id, _, _ := procTrackPopupMenuEx.Call(menu, tpmReturnCmd|tpmNoNotify|tpmRightButton,
		uintptr(p.x), uintptr(p.y), win.owner, 0)
	procPostMessageW.Call(win.owner, 0, 0, 0) // WM_NULL, per TrackPopupMenu's documentation
	if fg, _, _ := procGetForegroundWindow.Call(); prev != 0 && fg == win.owner {
		procSetForegroundWindow.Call(prev)
	}
	if i := int(id) - 1; i >= 0 && i < len(win.menuActions) && hooks.MenuAction != nil {
		hooks.MenuAction(win.menuActions[i])
	}
}

func buildMenu(items []MenuItem) uintptr {
	menu, _, _ := procCreatePopupMenu.Call()
	for _, it := range items {
		switch {
		case it.Separator:
			procAppendMenuW.Call(menu, mfSeparator, 0, 0)
		case len(it.Items) > 0:
			sub := buildMenu(it.Items)
			procAppendMenuW.Call(menu, mfPopup, sub, uintptr(unsafe.Pointer(wstr(it.Title))))
		default:
			label := it.Title
			if k := menuKey(it); k != "" {
				label += "\t" + k
			} else if it.Subtitle != "" {
				label += "\t" + it.Subtitle
			}
			flags := uintptr(mfString)
			if it.Checked {
				flags |= mfChecked
			}
			if it.Disabled || it.Header || it.Action == "" {
				flags |= mfGrayed
			}
			win.menuActions = append(win.menuActions, it.Action)
			procAppendMenuW.Call(menu, flags, uintptr(len(win.menuActions)), uintptr(unsafe.Pointer(wstr(label))))
		}
	}
	return menu
}

// menuKey spells a menu item's shortcut the Windows way, e.g. Ctrl+Alt+T.
func menuKey(it MenuItem) string {
	if it.Key == "" {
		return ""
	}
	var parts []string
	for _, m := range []struct {
		bit  int
		name string
	}{{1, "Ctrl"}, {2, "Alt"}, {4, "Shift"}, {8, "Win"}} {
		if it.Mods&m.bit != 0 {
			parts = append(parts, m.name)
		}
	}
	key := it.Key
	switch {
	case key == " ":
		key = "Space"
	case len(key) == 1:
		key = strings.ToUpper(key)
	}
	return strings.Join(append(parts, key), "+")
}

// ---- fonts, measuring and offscreen rendering ------------------------------------

var loadFonts = sync.OnceValue(func() *raster.Fonts {
	dir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	read := func(names ...string) []byte {
		for _, n := range names {
			if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
				return b
			}
		}
		return nil
	}
	semibold := read("seguisb.ttf", "segoeuib.ttf", "arialbd.ttf")
	f, err := raster.NewFonts(read("segoeui.ttf", "arial.ttf"), semibold, semibold, read("segoeuib.ttf", "arialbd.ttf"))
	if err != nil {
		return nil
	}
	return f
})

// Measurer measures text with Segoe UI, the font the overlay is drawn in.
type Measurer struct{}

// TextWidth implements overlay.Measurer.
func (Measurer) TextWidth(s string, size float32, w overlay.Weight, _ bool) float32 {
	f := loadFonts()
	if f == nil {
		return overlay.ApproxMeasurer{}.TextWidth(s, size, w, false)
	}
	return f.TextWidth(s, size, w)
}

// RenderPNG replays a display list offscreen into a PNG (no window).
func RenderPNG(l *overlay.List, width, height int, scale float64, backdrop overlay.Color, path string) error {
	fonts := loadFonts()
	if fonts == nil {
		return errors.New("no usable fonts in %WINDIR%\\Fonts")
	}
	r := raster.Renderer{Fonts: fonts}
	pw, ph := int(math.Ceil(float64(width)*scale)), int(math.Ceil(float64(height)*scale))
	canvas := image.NewRGBA(image.Rect(0, 0, pw, ph))
	r.Draw(canvas, l, float32(scale))
	out := image.NewRGBA(canvas.Rect)
	if backdrop[3] > 0 {
		c := image.NewUniform(color.NRGBA{uint8(backdrop[0] * 255), uint8(backdrop[1] * 255),
			uint8(backdrop[2] * 255), uint8(backdrop[3] * 255)})
		draw.Draw(out, out.Rect, c, image.Point{}, draw.Src)
	}
	draw.Draw(out, out.Rect, canvas, image.Point{}, draw.Over)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, out); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
