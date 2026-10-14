// Package overlay is the recording HUD: a platform-neutral state machine
// that turns daemon events into a vector display list each frame. Native
// hosts (AppKit, GTK) replay the list on screen or into a PNG, so both
// desktops render the same design from one implementation.
package overlay

import "math"

// Op kinds. Keep in sync with vio_op in internal/ui/vio_draw.h.
const (
	OpRRect uint8 = iota + 1
	OpCircle
	OpPath
	OpArc
	OpText
	OpClipPush
	OpClipPop
)

// Op flags.
const (
	FlagFill   uint8 = 1 << 0
	FlagStroke uint8 = 1 << 1
	FlagMono   uint8 = 1 << 2 // tabular digits
	FlagShadow uint8 = 1 << 3
)

// Text alignment and truncation.
const (
	AlignLeft uint8 = iota
	AlignCenter
	AlignRight
)

const (
	TruncTail uint8 = iota
	TruncHead
	TruncNone
)

// Weight is a font weight.
type Weight uint8

const (
	Regular Weight = iota
	Medium
	Semibold
	Bold
)

// Path commands in the path arena.
const (
	PathMove  float32 = 0 // x y
	PathLine  float32 = 1 // x y
	PathCubic float32 = 2 // c1x c1y c2x c2y x y
	PathClose float32 = 3
)

// Op is one drawing command. Field order and types mirror the C struct
// vio_op exactly; it holds no Go pointers so it can be passed to C.
type Op struct {
	Kind     uint8
	Flags    uint8
	Align    uint8
	Truncate uint8
	Weight   uint8
	Lines    uint8
	_        uint16
	X, Y     float32
	W, H     float32
	R        float32
	LineW    float32
	Fill     [4]float32
	Stroke   [4]float32
	Shadow   [4]float32
	ShadowB  float32 // blur radius
	ShadowDY float32 // vertical offset (downward)
	FontSize float32
	A0, A1   float32 // arc angles (radians, clockwise from +x in y-down space)
	Off, Len uint32  // text bytes or path floats
}

// Color is premultiplication-free RGBA in 0..1.
type Color [4]float32

// WithAlpha scales alpha.
func (c Color) WithAlpha(a float32) Color { return Color{c[0], c[1], c[2], c[3] * a} }

// RGB builds an opaque color from 0..255 components.
func RGB(r, g, b uint8) Color { return Color{float32(r) / 255, float32(g) / 255, float32(b) / 255, 1} }

// Mix blends a toward b by t.
func Mix(a, b Color, t float32) Color {
	return Color{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t, a[2] + (b[2]-a[2])*t, a[3] + (b[3]-a[3])*t}
}

// List is a reusable display list.
type List struct {
	Ops     []Op
	TextBuf []byte
	PathBuf []float32
	alpha   []float32
}

// Reset clears the list, keeping capacity.
func (l *List) Reset() {
	l.Ops, l.TextBuf, l.PathBuf, l.alpha = l.Ops[:0], l.TextBuf[:0], l.PathBuf[:0], l.alpha[:0]
}

func (l *List) a() float32 {
	if len(l.alpha) == 0 {
		return 1
	}
	return l.alpha[len(l.alpha)-1]
}

// PushAlpha multiplies the opacity of subsequent ops.
func (l *List) PushAlpha(a float32) { l.alpha = append(l.alpha, l.a()*clamp32(a, 0, 1)) }

// PopAlpha restores the previous opacity.
func (l *List) PopAlpha() { l.alpha = l.alpha[:len(l.alpha)-1] }

func (l *List) col(c Color) [4]float32 { return [4]float32(c.WithAlpha(l.a())) }

// Shadow describes a soft drop shadow.
type Shadow struct {
	Color    Color
	Blur, DY float32
}

// RRect fills (and optionally strokes) a rounded rectangle.
func (l *List) RRect(x, y, w, h, r float32, fill Color, stroke Color, lineW float32, sh *Shadow) {
	op := Op{Kind: OpRRect, X: x, Y: y, W: w, H: h, R: r, LineW: lineW}
	if fill[3] > 0 {
		op.Flags |= FlagFill
		op.Fill = l.col(fill)
	}
	if stroke[3] > 0 && lineW > 0 {
		op.Flags |= FlagStroke
		op.Stroke = l.col(stroke)
	}
	if sh != nil && sh.Color[3] > 0 {
		op.Flags |= FlagShadow
		op.Shadow, op.ShadowB, op.ShadowDY = l.col(sh.Color), sh.Blur, sh.DY
	}
	if op.Flags&(FlagFill|FlagStroke) != 0 {
		l.Ops = append(l.Ops, op)
	}
}

// Circle fills or strokes a circle.
func (l *List) Circle(cx, cy, r float32, fill, stroke Color, lineW float32) {
	op := Op{Kind: OpCircle, X: cx, Y: cy, R: r, LineW: lineW}
	if fill[3] > 0 {
		op.Flags |= FlagFill
		op.Fill = l.col(fill)
	}
	if stroke[3] > 0 && lineW > 0 {
		op.Flags |= FlagStroke
		op.Stroke = l.col(stroke)
	}
	if op.Flags != 0 {
		l.Ops = append(l.Ops, op)
	}
}

// Arc strokes a circular arc from a0 to a1 (radians).
func (l *List) Arc(cx, cy, r, a0, a1, lineW float32, c Color) {
	l.Ops = append(l.Ops, Op{Kind: OpArc, Flags: FlagStroke, X: cx, Y: cy, R: r, A0: a0, A1: a1,
		LineW: lineW, Stroke: l.col(c)})
}

// PathBuilder appends commands for one path.
type PathBuilder struct {
	l     *List
	start int
	// Transform: point = origin + p*scale.
	ox, oy, s float32
}

// BeginPath starts a path whose points are scaled by s and offset by (ox, oy).
func (l *List) BeginPath(ox, oy, s float32) *PathBuilder {
	return &PathBuilder{l: l, start: len(l.PathBuf), ox: ox, oy: oy, s: s}
}

func (p *PathBuilder) pt(x, y float32) (float32, float32) { return p.ox + x*p.s, p.oy + y*p.s }

// Move starts a subpath.
func (p *PathBuilder) Move(x, y float32) *PathBuilder {
	x, y = p.pt(x, y)
	p.l.PathBuf = append(p.l.PathBuf, PathMove, x, y)
	return p
}

// Line adds a straight segment.
func (p *PathBuilder) Line(x, y float32) *PathBuilder {
	x, y = p.pt(x, y)
	p.l.PathBuf = append(p.l.PathBuf, PathLine, x, y)
	return p
}

// Cubic adds a Bézier segment.
func (p *PathBuilder) Cubic(c1x, c1y, c2x, c2y, x, y float32) *PathBuilder {
	c1x, c1y = p.pt(c1x, c1y)
	c2x, c2y = p.pt(c2x, c2y)
	x, y = p.pt(x, y)
	p.l.PathBuf = append(p.l.PathBuf, PathCubic, c1x, c1y, c2x, c2y, x, y)
	return p
}

// Close closes the subpath.
func (p *PathBuilder) Close() *PathBuilder {
	p.l.PathBuf = append(p.l.PathBuf, PathClose)
	return p
}

// Stroke emits the path as a round-capped stroke.
func (p *PathBuilder) Stroke(c Color, lineW float32) {
	p.l.Ops = append(p.l.Ops, Op{Kind: OpPath, Flags: FlagStroke, Stroke: p.l.col(c), LineW: lineW * p.s,
		Off: uint32(p.start), Len: uint32(len(p.l.PathBuf) - p.start)})
}

// Fill emits the path filled (non-zero winding).
func (p *PathBuilder) Fill(c Color) {
	p.l.Ops = append(p.l.Ops, Op{Kind: OpPath, Flags: FlagFill, Fill: p.l.col(c),
		Off: uint32(p.start), Len: uint32(len(p.l.PathBuf) - p.start)})
}

// TextStyle configures a text op.
type TextStyle struct {
	Size     float32
	Weight   Weight
	Color    Color
	Align    uint8
	Truncate uint8
	Lines    uint8
	Mono     bool
}

// Text draws s inside the box (x, y, w, h), vertically centred.
func (l *List) Text(s string, x, y, w, h float32, st TextStyle) {
	if s == "" {
		return
	}
	off := len(l.TextBuf)
	l.TextBuf = append(l.TextBuf, s...)
	op := Op{Kind: OpText, X: x, Y: y, W: w, H: h, FontSize: st.Size, Weight: uint8(st.Weight),
		Align: st.Align, Truncate: st.Truncate, Lines: max(st.Lines, 1), Fill: l.col(st.Color),
		Off: uint32(off), Len: uint32(len(s))}
	op.Flags = FlagFill
	if st.Mono {
		op.Flags |= FlagMono
	}
	l.Ops = append(l.Ops, op)
}

// ClipRRect clips subsequent ops to a rounded rectangle until PopClip.
func (l *List) ClipRRect(x, y, w, h, r float32) {
	l.Ops = append(l.Ops, Op{Kind: OpClipPush, X: x, Y: y, W: w, H: h, R: r})
}

// PopClip ends the innermost clip.
func (l *List) PopClip() { l.Ops = append(l.Ops, Op{Kind: OpClipPop}) }

func clamp32(v, lo, hi float32) float32 {
	return float32(math.Max(float64(lo), math.Min(float64(hi), float64(v))))
}
