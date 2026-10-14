//go:build darwin

package ui

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -fmodules -Wno-deprecated-declarations
#cgo LDFLAGS: -framework AppKit -framework WebKit -framework CoreText -framework ImageIO -framework UniformTypeIdentifiers
#include <stdlib.h>
#include "ui_darwin.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"gitlab.com/integral-inference/voice-io/internal/overlay"
)

// menuJSON encodes menu items for the AppKit side.
func menuJSON(items []MenuItem) string {
	if items == nil {
		items = []MenuItem{}
	}
	data, _ := json.Marshal(items)
	return string(data)
}

func init() {
	if unsafe.Sizeof(overlay.Op{}) != C.sizeof_vio_op ||
		uint64(unsafe.Offsetof(overlay.Op{}.Off)) != uint64(C.vio_op_off_offset()) {
		panic(fmt.Sprintf("overlay.Op layout (%d bytes) does not match vio_op (%d bytes)",
			unsafe.Sizeof(overlay.Op{}), C.sizeof_vio_op))
	}
}

// Supported reports whether a native UI exists on this platform.
func Supported() bool { return true }

// Run starts the AppKit loop on the calling (main) thread until Quit.
func Run(h Hooks, o Options) {
	hooks = h
	C.vio_ui_run(C.int(overlay.CanvasW), C.int(overlay.CanvasH), cbool(o.Overlay), cbool(o.StatusItem))
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// Quit stops the native loop; Run returns.
func Quit() { C.vio_ui_quit() }

// ReplyTerminate lets a system-requested quit proceed.
func ReplyTerminate() { C.vio_ui_reply_terminate() }

// RequestFrame schedules overlay redraws until the model stops animating.
func RequestFrame() { C.vio_ui_request_frame() }

// SetOverlayEnabled shows or removes the overlay window.
func SetOverlayEnabled(enabled bool) { C.vio_ui_set_overlay_enabled(cbool(enabled)) }

// SetStatus updates the menu bar icon: idle | recording | processing | off | setup.
func SetStatus(state string) {
	cs := C.CString(state)
	defer C.free(unsafe.Pointer(cs))
	C.vio_ui_set_status(cs)
}

// ShowMenu pops up a context menu at a point in overlay window coordinates.
func ShowMenu(items []MenuItem, x, y float32) {
	cs := C.CString(menuJSON(items))
	defer C.free(unsafe.Pointer(cs))
	C.vio_ui_show_menu(cs, C.double(x), C.double(y))
}

// OpenSettings opens (or focuses) the Settings window at url.
func OpenSettings(url, token string) {
	cu, ct := C.CString(url), C.CString(token)
	defer C.free(unsafe.Pointer(cu))
	defer C.free(unsafe.Pointer(ct))
	C.vio_ui_open_settings(cu, ct)
}

var list overlay.List

func listPointers(l *overlay.List) (*C.vio_op, *C.char, *C.float) {
	var ops *C.vio_op
	var text *C.char
	var path *C.float
	if len(l.Ops) > 0 {
		ops = (*C.vio_op)(unsafe.Pointer(&l.Ops[0]))
	}
	if len(l.TextBuf) > 0 {
		text = (*C.char)(unsafe.Pointer(&l.TextBuf[0]))
	}
	if len(l.PathBuf) > 0 {
		path = (*C.float)(unsafe.Pointer(&l.PathBuf[0]))
	}
	return ops, text, path
}

//export vioUIReady
func vioUIReady() {
	if hooks.Ready != nil {
		hooks.Ready()
	}
}

//export vioOverlayPaint
func vioOverlayPaint(ctx unsafe.Pointer, scale C.double, info *C.vio_frame_info) {
	if hooks.Model == nil {
		return
	}
	f := hooks.Model.Draw(&list)
	if len(list.Ops) > 0 {
		ops, text, path := listPointers(&list)
		C.vio_replay(ctx, ops, C.int(len(list.Ops)), text, path, scale)
	}
	info.animating = cbool(f.Animating)
	info.visible = cbool(f.Visible)
	info.active = cbool(f.Active)
	info.offset_y = C.double(f.OffsetY)
}

//export vioOverlayPointer
func vioOverlayPointer(x, y C.double, inside C.int) C.int {
	if hooks.Model == nil {
		return 0
	}
	hooks.Model.Pointer(float32(x), float32(y), inside != 0)
	return cbool(inside != 0 && hooks.Model.HitTest(float32(x), float32(y)))
}

//export vioOverlayMouse
func vioOverlayMouse(kind C.int, x, y, sy C.double, button C.int) {
	m := hooks.Model
	if m == nil {
		return
	}
	switch kind {
	case 0:
		m.MouseDown(float32(x), float32(y), float32(sy), int(button))
	case 1:
		m.MouseDragged(float32(sy))
	case 2:
		m.MouseUp(float32(x), float32(y))
	}
}

//export vioStatusMenuJSON
func vioStatusMenuJSON() *C.char {
	var items []MenuItem
	if hooks.StatusMenu != nil {
		items = hooks.StatusMenu()
	}
	return C.CString(menuJSON(items))
}

//export vioMenuAction
func vioMenuAction(action *C.char) {
	if hooks.MenuAction != nil {
		hooks.MenuAction(C.GoString(action))
	}
}

//export vioSettingsClosed
func vioSettingsClosed() {
	if hooks.SettingsClosed != nil {
		hooks.SettingsClosed()
	}
}

//export vioAppearance
func vioAppearance(dark C.int) {
	if hooks.Model != nil {
		hooks.Model.SetSystemDark(dark != 0)
	}
}

//export vioScreenHeight
func vioScreenHeight(h C.double) {
	if hooks.Model != nil {
		hooks.Model.SetScreenHeight(float64(h))
	}
}

//export vioTerminateRequested
func vioTerminateRequested() {
	if hooks.Terminate != nil {
		go hooks.Terminate()
		return
	}
	ReplyTerminate()
}

// Measurer measures text with the system font (CoreText).
type Measurer struct{}

// TextWidth implements overlay.Measurer.
func (Measurer) TextWidth(s string, size float32, w overlay.Weight, mono bool) float32 {
	if s == "" {
		return 0
	}
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	m := 0
	if mono {
		m = 1
	}
	return float32(C.vio_measure_text(cs, C.int(len(s)), C.float(size), C.int(w), C.int(m)))
}

// RenderPNG replays a display list offscreen into a PNG (no window).
func RenderPNG(l *overlay.List, width, height int, scale float64, backdrop overlay.Color, path string) error {
	cp := C.CString(path)
	defer C.free(unsafe.Pointer(cp))
	ops, text, pathBuf := listPointers(l)
	bg := [4]C.float{C.float(backdrop[0]), C.float(backdrop[1]), C.float(backdrop[2]), C.float(backdrop[3])}
	if rc := C.vio_render_png(ops, C.int(len(l.Ops)), text, pathBuf, C.int(width), C.int(height),
		C.double(scale), &bg[0], cp); rc != 0 {
		return errors.New("PNG rendering failed")
	}
	return nil
}
