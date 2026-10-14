//go:build linux

package ui

/*
#cgo pkg-config: gtk+-3.0
#cgo LDFLAGS: -lm
#include <stdlib.h>
#include "ui_linux.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"unsafe"

	"gitlab.com/integral-inference/voice-io/internal/overlay"
)

func init() {
	if unsafe.Sizeof(overlay.Op{}) != C.sizeof_vio_op ||
		uint64(unsafe.Offsetof(overlay.Op{}.Off)) != uint64(C.vio_op_off_offset()) {
		panic(fmt.Sprintf("overlay.Op layout (%d bytes) does not match vio_op (%d bytes)",
			unsafe.Sizeof(overlay.Op{}), C.sizeof_vio_op))
	}
}

var (
	gtkOnce sync.Once
	gtkOK   bool
)

// Supported reports whether an X11 display (or XWayland) is available. It
// initializes GTK, so the first call must come from the thread that later
// calls Run.
func Supported() bool {
	gtkOnce.Do(func() { gtkOK = C.vio_gtk_init() != 0 })
	return gtkOK
}

// Run starts the GTK loop on the calling (main) thread until Quit. GNOME
// has no status area, so the pill's right-click menu replaces the menu bar
// item; o.StatusItem is ignored.
func Run(h Hooks, o Options) {
	hooks = h
	C.vio_ui_run(C.int(overlay.CanvasW), C.int(overlay.CanvasH), cbool(o.Overlay))
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// Quit stops the GTK loop; Run returns.
func Quit() { C.vio_ui_quit() }

// ReplyTerminate ends the loop after a session-ending quit was handled.
func ReplyTerminate() { C.vio_ui_quit() }

// RequestFrame schedules overlay frames until the model stops animating.
func RequestFrame() { C.vio_ui_request_frame() }

// SetOverlayEnabled shows or removes the overlay window.
func SetOverlayEnabled(enabled bool) { C.vio_ui_set_overlay_enabled(cbool(enabled)) }

// SetStatus has no surface on GNOME (no status area); the pill shows state.
func SetStatus(string) {}

// ShowMenu pops up a context menu at the pointer. It must be called on the
// UI thread, as overlay actions are.
func ShowMenu(items []MenuItem, _, _ float32) {
	m := C.vio_menu_new()
	buildMenu(m, items)
	C.vio_menu_popup(m)
}

func buildMenu(m unsafe.Pointer, items []MenuItem) {
	for _, it := range items {
		if it.Separator {
			C.vio_menu_add_separator(m)
			continue
		}
		title := C.CString(it.Title)
		if len(it.Items) > 0 {
			buildMenu(C.vio_menu_add_submenu(m, title), it.Items)
			C.free(unsafe.Pointer(title))
			continue
		}
		sub, action := C.CString(it.Subtitle), C.CString(it.Action)
		C.vio_menu_add_item(m, title, sub, action, cbool(it.Checked), cbool(it.Disabled), cbool(it.Header))
		C.free(unsafe.Pointer(title))
		C.free(unsafe.Pointer(sub))
		C.free(unsafe.Pointer(action))
	}
}

// OpenSettings opens Settings in the default browser; the token travels in
// the URL fragment, which never reaches the server or its logs.
func OpenSettings(url, token string) {
	cu := C.CString(url + "&token=" + token)
	defer C.free(unsafe.Pointer(cu))
	C.vio_ui_open_uri(cu)
}

var (
	list   overlay.List
	rects  []float32
	shaped []float32
)

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

// vioOverlayFrame advances the model one frame and updates the window's
// input shape to the pill's interactive areas.
//
//export vioOverlayFrame
func vioOverlayFrame(info *C.vio_frame_info) {
	if hooks.Model == nil {
		return
	}
	f := hooks.Model.Draw(&list)
	rects = hooks.Model.InputRects(rects[:0])
	if !slices.Equal(rects, shaped) {
		shaped = append(shaped[:0], rects...)
		var p *C.float
		if len(rects) > 0 {
			p = (*C.float)(unsafe.Pointer(&rects[0]))
		}
		C.vio_ui_set_input_rects(p, C.int(len(rects)))
	}
	info.animating = cbool(f.Animating)
	info.visible = cbool(f.Visible)
	info.active = cbool(f.Active)
	info.offset_y = C.double(f.OffsetY)
}

//export vioOverlayReplay
func vioOverlayReplay(cr unsafe.Pointer) {
	if len(list.Ops) > 0 {
		ops, text, path := listPointers(&list)
		C.vio_replay(cr, ops, C.int(len(list.Ops)), text, path)
	}
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

//export vioMenuAction
func vioMenuAction(action *C.char) {
	if hooks.MenuAction != nil {
		hooks.MenuAction(C.GoString(action))
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

// Measurer measures text with the desktop UI font (Pango).
type Measurer struct{}

// TextWidth implements overlay.Measurer.
func (Measurer) TextWidth(s string, size float32, w overlay.Weight, mono bool) float32 {
	if s == "" {
		return 0
	}
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	return float32(C.vio_measure_text(cs, C.int(len(s)), C.float(size), C.int(w), cbool(mono)))
}

// RenderPNG replays a display list offscreen into a PNG (no display needed).
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
