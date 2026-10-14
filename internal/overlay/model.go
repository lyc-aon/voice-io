package overlay

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
)

// The overlay window is a fixed transparent canvas docked to the right edge
// of the screen. The pill hugs the canvas's right side; bubbles (live
// preview, status, hints) open to its left.
const (
	CanvasW     = 460
	CanvasH     = 340
	edgeMargin  = 10 // gap between the pill and the screen edge
	pillW       = 36 // recording and processing pill width
	handleW     = 7
	handleH     = 40
	hoverD      = 38
	resultD     = 38
	bars        = 19 // waveform bars, stacked vertically
	historyLen  = bars/2 + 1
	levelStep   = 0.02 // seconds of audio per received level
	bubbleH     = 30
	bubbleGap   = 10
	maxBubbleW  = 340
	toastTime   = 1800 * time.Millisecond
	previewTime = 1500 * time.Millisecond
	readyTime   = 2600 * time.Millisecond
)

// Measurer reports text widths in points.
type Measurer interface {
	TextWidth(s string, size float32, w Weight, mono bool) float32
}

// ApproxMeasurer estimates widths without a font system (tests, headless).
type ApproxMeasurer struct{}

// TextWidth approximates proportional system UI text.
func (ApproxMeasurer) TextWidth(s string, size float32, w Weight, mono bool) float32 {
	per := float32(0.53)
	if mono {
		per = 0.6
	}
	if w >= Semibold {
		per += 0.03
	}
	return float32(len([]rune(s))) * size * per
}

// Action is a user request from the overlay.
type Action struct {
	Kind string // dictate | meeting | push | toggle | cancel | settings | reveal | permission | menu | position
	Arg  string
	X, Y float32 // menu location (window points)
	N    float64 // position offset
}

// Config is the overlay's user configuration.
type Config struct {
	Enabled      bool
	Theme        string // auto | dark | light
	Accent       string // "" = monochrome
	PreviewColor string
	IdleHandle   bool
	LivePreview  bool
	Shortcut     string  // display form of the start shortcut
	OffsetY      float64 // dragged offset from the screen's vertical centre (down is positive)
}

// Result mirrors the daemon's session result.
type Result struct {
	Text       string
	Style      string
	AutoClose  float64
	ShowText   bool
	Reveal     string // clicking the result opens this file
	Permission string // clicking the result asks for this permission
}

type visual uint8

const (
	vHidden visual = iota
	vHandle
	vHover
	vRecording
	vProcessing
	vResult
)

type toast struct {
	text, style string
	until       time.Time
	forever     bool
}

// Model is the overlay state machine. All methods are safe for concurrent
// use; daemon events may arrive from any goroutine.
type Model struct {
	mu      sync.Mutex
	clock   func() time.Time
	measure Measurer
	notify  func()
	act     func(Action)

	cfg        Config
	systemDark bool
	theme      Theme
	setup      string // non-empty: dictation cannot start until setup finishes

	state        string // idle | listening | processing | off
	session      bool
	sessionMode  string
	sessionStart time.Time
	procLabel    string
	procSince    time.Time
	levels       []float64           // received voice levels not yet shown (20 ms each)
	levelAcc     float64             // time owed to the level playhead (seconds)
	peak         float64             // recent loudest level, for automatic gain
	history      [historyLen]float64 // shown levels, newest first
	heights      [bars]float64       // drawn bar lengths (0-1), smoothed
	preview      string
	previewAt    time.Time
	previewUntil time.Time
	toast        toast
	result       Result
	resultAt     time.Time
	resultUntil  time.Time
	readyUntil   time.Time

	hoverRegion string
	expanded    bool
	collapseAt  time.Time
	pressRegion string
	pressSY     float32
	pressOffset float64
	dragging    bool
	screenH     float64
	regions     []region
	shownVisual visual
	visualSince time.Time
	pillH       spring
	pillW       spring
	lastFrame   time.Time
}

type region struct {
	id         string
	x, y, w, h float32
}

// New creates a model. clock may be nil (time.Now).
func New(measure Measurer, clock func() time.Time) *Model {
	if clock == nil {
		clock = time.Now
	}
	if measure == nil {
		measure = ApproxMeasurer{}
	}
	m := &Model{clock: clock, measure: measure, state: "idle"}
	m.cfg = Config{Enabled: true, Theme: "dark", IdleHandle: true, LivePreview: true}
	m.theme = NewTheme(true, "", "")
	return m
}

// SetHooks installs the frame request and action callbacks.
func (m *Model) SetHooks(notify func(), act func(Action)) {
	m.mu.Lock()
	m.notify, m.act = notify, act
	m.mu.Unlock()
}

func (m *Model) changed() {
	m.mu.Lock()
	n := m.notify
	m.mu.Unlock()
	if n != nil {
		n()
	}
}

func (m *Model) updateTheme() {
	dark := m.systemDark
	switch m.cfg.Theme {
	case "dark":
		dark = true
	case "light":
		dark = false
	}
	m.theme = NewTheme(dark, m.cfg.Accent, m.cfg.PreviewColor)
}

// SetConfig applies user settings.
func (m *Model) SetConfig(c Config) {
	m.mu.Lock()
	m.cfg = c
	m.updateTheme()
	m.mu.Unlock()
	m.changed()
}

// SetSystemDark follows the desktop appearance for theme "auto".
func (m *Model) SetSystemDark(dark bool) {
	m.mu.Lock()
	m.systemDark = dark
	m.updateTheme()
	m.mu.Unlock()
	m.changed()
}

// SetSetup marks dictation unavailable with a short hint ("" = ready).
func (m *Model) SetSetup(hint string) {
	m.mu.Lock()
	m.setup = hint
	m.mu.Unlock()
	m.changed()
}

// OffsetY is the dragged vertical offset from the screen centre.
func (m *Model) OffsetY() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.OffsetY
}

// SetScreenHeight bounds vertical dragging.
func (m *Model) SetScreenHeight(h float64) {
	m.mu.Lock()
	m.screenH = h
	m.mu.Unlock()
}

// ---- daemon events --------------------------------------------------------

// State sets idle | listening | processing | off.
func (m *Model) State(s string) {
	m.mu.Lock()
	now := m.clock()
	if s == "processing" && m.state != "processing" {
		m.procSince = now
	}
	m.state = s
	if s != "listening" {
		m.previewUntil = time.Time{}
	}
	if s == "idle" && !m.session && m.toast.forever {
		m.toast = toast{}
	}
	m.mu.Unlock()
	m.changed()
}

// SessionOpen starts a recording session.
func (m *Model) SessionOpen(mode string) {
	m.mu.Lock()
	m.session = true
	m.sessionMode = mode
	m.sessionStart = m.clock()
	m.history = [historyLen]float64{}
	m.heights = [bars]float64{}
	m.levels, m.levelAcc, m.peak = m.levels[:0], 0, 0
	m.preview, m.previewUntil = "", time.Time{}
	m.procLabel = ""
	m.resultUntil = time.Time{}
	m.expanded = false
	m.mu.Unlock()
	m.changed()
}

// SessionClose ends the session.
func (m *Model) SessionClose() {
	m.mu.Lock()
	m.session = false
	if m.state == "listening" || m.state == "processing" {
		m.state = "idle"
	}
	m.preview, m.previewUntil = "", time.Time{}
	m.mu.Unlock()
	m.changed()
}

// Show displays a transient status during a session (info, warn, error) or
// a prompt that stays until the session ends.
func (m *Model) Show(text, style string) {
	m.mu.Lock()
	now := m.clock()
	switch {
	case text == "":
		m.toast = toast{}
	case style == "info" && !m.session:
	default:
		m.toast = toast{text: text, style: style, until: now.Add(toastTime), forever: style == "prompt"}
		m.previewUntil = time.Time{}
	}
	m.mu.Unlock()
	m.changed()
}

// Processing labels the current processing phase.
func (m *Model) Processing(label string) {
	m.mu.Lock()
	m.procLabel = label
	m.mu.Unlock()
	m.changed()
}

// Preview shows the latest tentative words while listening.
func (m *Model) Preview(text string) {
	m.mu.Lock()
	now := m.clock()
	if m.session && m.state == "listening" && m.cfg.LivePreview && !m.toastActive(now) && text != "" {
		if m.previewUntil.Before(now) {
			m.previewAt = now
		}
		m.preview = text
		m.previewUntil = now.Add(previewTime)
	}
	m.mu.Unlock()
	m.changed()
}

// Level queues the voice level of the next 20 ms of audio (RMS above the
// room's noise floor). Levels arrive in bursts with each capture block;
// frames play them back at an even pace.
func (m *Model) Level(rms float64) {
	m.mu.Lock()
	m.levels = append(m.levels, audio.SpeechLevel(rms))
	if len(m.levels) > 32 {
		m.levels = m.levels[len(m.levels)-32:]
	}
	m.mu.Unlock()
}

// Result finishes a session with a status.
func (m *Model) Result(r Result) {
	m.mu.Lock()
	now := m.clock()
	m.previewUntil = time.Time{}
	if r.Style == "ok" && !r.ShowText {
		r.Text = "" // success never displays transcript text
	}
	m.result = r
	m.resultAt = now
	m.resultUntil = now.Add(time.Duration(math.Max(0.6, r.AutoClose) * float64(time.Second)))
	m.toast = toast{}
	m.mu.Unlock()
	m.changed()
}

// Ready pulses the resting handle (active=false marks it muted).
func (m *Model) Ready(active bool) {
	m.mu.Lock()
	if active {
		m.readyUntil = m.clock().Add(readyTime)
	} else {
		m.readyUntil = time.Time{}
	}
	m.mu.Unlock()
	m.changed()
}

// Clear hides transient text.
func (m *Model) Clear() {
	m.mu.Lock()
	m.previewUntil = time.Time{}
	m.toast = toast{}
	m.mu.Unlock()
	m.changed()
}

func (m *Model) toastActive(now time.Time) bool {
	return m.toast.text != "" && (m.toast.forever || now.Before(m.toast.until))
}

// ---- pointer --------------------------------------------------------------

func (m *Model) regionAt(x, y float32) string {
	for i := len(m.regions) - 1; i >= 0; i-- {
		r := m.regions[i]
		if x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h {
			return r.id
		}
	}
	return ""
}

// HitTest reports whether (x, y) is interactive (window points).
func (m *Model) HitTest(x, y float32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.regionAt(x, y) != "" || m.dragging
}

// InputRects appends the interactive areas of the last frame to dst as
// x, y, w, h quadruples (window points). Hosts that cannot poll the
// pointer outside their window (X11 under Wayland) accept input only there.
func (m *Model) InputRects(dst []float32) []float32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.regions {
		dst = append(dst, r.x, r.y, r.w, r.h)
	}
	return dst
}

// Pointer reports the pointer position; inside is false when it left.
func (m *Model) Pointer(x, y float32, inside bool) {
	m.mu.Lock()
	now := m.clock()
	id := ""
	if inside {
		id = m.regionAt(x, y)
	}
	changed := id != m.hoverRegion
	m.hoverRegion = id
	switch id {
	case "handle", "hover", "hint", "mic", "meeting":
		if !m.expanded {
			changed = true
		}
		m.expanded = true
		m.collapseAt = time.Time{}
	default:
		if m.expanded && m.collapseAt.IsZero() {
			m.collapseAt = now.Add(250 * time.Millisecond)
			changed = true
		}
	}
	m.mu.Unlock()
	if changed {
		m.changed()
	}
}

// MouseDown begins a click or drag at window point (x, y); sy is the
// pointer's screen y (downward) for drag deltas.
func (m *Model) MouseDown(x, y, sy float32, button int) {
	m.mu.Lock()
	id := m.regionAt(x, y)
	if button != 0 {
		act := m.act
		m.mu.Unlock()
		if act != nil {
			act(Action{Kind: "menu", X: x, Y: y})
		}
		return
	}
	m.pressRegion, m.pressSY, m.pressOffset, m.dragging = id, sy, m.cfg.OffsetY, false
	m.mu.Unlock()
}

// MouseDragged slides the dock along the screen edge past a small threshold.
func (m *Model) MouseDragged(sy float32) {
	m.mu.Lock()
	if m.pressRegion == "" || m.pressRegion == "cancel" || m.pressRegion == "stop" {
		m.mu.Unlock()
		return
	}
	dy := float64(sy - m.pressSY)
	if !m.dragging && math.Abs(dy) < 4 {
		m.mu.Unlock()
		return
	}
	m.dragging = true
	limit := math.Max(0, (m.screenH-CanvasH)/2+CanvasH/2-60)
	if m.screenH == 0 {
		limit = 2000
	}
	m.cfg.OffsetY = clamp(m.pressOffset+dy, -limit, limit)
	m.mu.Unlock()
	m.changed()
}

// MouseUp completes a click (action) or a drag (position).
func (m *Model) MouseUp(x, y float32) {
	m.mu.Lock()
	press, dragging := m.pressRegion, m.dragging
	m.pressRegion, m.dragging = "", false
	id := m.regionAt(x, y)
	act := m.act
	var a *Action
	switch {
	case dragging:
		a = &Action{Kind: "position", N: m.cfg.OffsetY}
	case press == "" || press != id:
	case id == "cancel":
		a = &Action{Kind: "cancel"}
	case id == "stop" || id == "body":
		if m.state == "listening" {
			a = &Action{Kind: "push"} // finishes whichever session is recording
		}
	case id == "meeting":
		a = &Action{Kind: "meeting"}
	case id == "mic" || id == "handle" || id == "hover" || id == "hint":
		switch {
		case m.setup != "":
			a = &Action{Kind: "settings"}
		case m.state == "off":
			a = &Action{Kind: "toggle"}
		default:
			a = &Action{Kind: "dictate"}
		}
	case (id == "result" || id == "bubble") && m.result.Permission != "" && m.clock().Before(m.resultUntil):
		a = &Action{Kind: "permission", Arg: m.result.Permission}
	case (id == "result" || id == "bubble") && m.result.Reveal != "" && m.clock().Before(m.resultUntil):
		a = &Action{Kind: "reveal", Arg: m.result.Reveal}
	}
	m.mu.Unlock()
	if a != nil && act != nil {
		act(*a)
	}
	m.changed()
}

// ---- frame ----------------------------------------------------------------

// Frame describes one rendered frame.
type Frame struct {
	Visible   bool    // anything drawn
	Active    bool    // a session pill (not the resting handle) is showing
	Animating bool    // keep frames coming
	OffsetY   float64 // offset from the screen's vertical centre (down is positive)
}

func (m *Model) currentVisual(now time.Time) visual {
	if !m.cfg.Enabled {
		return vHidden
	}
	if now.Before(m.resultUntil) {
		return vResult
	}
	if m.session || m.state == "listening" || m.state == "processing" {
		if m.state == "processing" {
			return vProcessing
		}
		return vRecording
	}
	if !m.cfg.IdleHandle && m.setup == "" {
		return vHidden
	}
	if m.expanded {
		return vHover
	}
	return vHandle
}

// Draw renders the current frame into l (cleared first).
func (m *Model) Draw(l *List) Frame {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	dt := 1.0 / 60
	if !m.lastFrame.IsZero() {
		dt = clamp(now.Sub(m.lastFrame).Seconds(), 0.001, 0.1)
	}
	m.lastFrame = now
	if m.expanded && !m.collapseAt.IsZero() && now.After(m.collapseAt) {
		m.expanded, m.collapseAt = false, time.Time{}
	}
	v := m.currentVisual(now)
	if v != m.shownVisual {
		m.shownVisual, m.visualSince = v, now
	}
	l.Reset()
	m.regions = m.regions[:0]
	f := Frame{OffsetY: m.cfg.OffsetY, Visible: v != vHidden,
		Active: v == vRecording || v == vProcessing || v == vResult}
	if v == vHidden {
		m.pillW.init, m.pillH.init = false, false
		return f
	}
	f.Animating = m.paint(l, v, now, dt) || m.dragging || !m.collapseAt.IsZero()
	return f
}

func (m *Model) elapsed(now time.Time) string {
	d := now.Sub(m.sessionStart)
	if d < 0 || m.sessionStart.IsZero() {
		d = 0
	}
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// tailFit drops leading words until text fits width (with an ellipsis).
func (m *Model) tailFit(text string, size float32, w Weight, width float32) string {
	if m.measure.TextWidth(text, size, w, false) <= width {
		return text
	}
	words := strings.Fields(text)
	for len(words) > 1 {
		words = words[1:]
		s := "…" + strings.Join(words, " ")
		if m.measure.TextWidth(s, size, w, false) <= width {
			return s
		}
	}
	return text
}
