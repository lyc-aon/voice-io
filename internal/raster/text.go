package raster

import (
	"errors"
	"image"
	"math"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"gitlab.com/integral-inference/voice-io/internal/overlay"
)

// Fonts holds one typeface per overlay weight and caches sized faces.
type Fonts struct {
	byWeight [4]*opentype.Font

	mu    sync.Mutex
	faces map[faceKey]font.Face
}

type faceKey struct {
	weight overlay.Weight
	size   float32
}

// NewFonts parses TrueType/OpenType data for the regular, medium, semibold
// and bold weights. A nil entry uses the nearest lighter weight given.
func NewFonts(regular, medium, semibold, bold []byte) (*Fonts, error) {
	f := &Fonts{faces: map[faceKey]font.Face{}}
	var last *opentype.Font
	for i, data := range [4][]byte{regular, medium, semibold, bold} {
		if data != nil {
			parsed, err := opentype.Parse(data)
			if err != nil {
				return nil, err
			}
			last = parsed
		}
		f.byWeight[i] = last
	}
	if f.byWeight[0] == nil {
		return nil, errors.New("raster: a regular font is required")
	}
	return f, nil
}

// face returns the face for weight at size pixels.
func (f *Fonts) face(w overlay.Weight, size float32) font.Face {
	w = min(w, overlay.Bold)
	key := faceKey{w, size}
	f.mu.Lock()
	defer f.mu.Unlock()
	if face, ok := f.faces[key]; ok {
		return face
	}
	face, err := opentype.NewFace(f.byWeight[w], &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil
	}
	if len(f.faces) > 64 {
		clear(f.faces)
	}
	f.faces[key] = face
	return face
}

func fx(v fixed.Int26_6) float32 { return float32(v) / 64 }

// TextWidth measures s in logical pixels.
func (f *Fonts) TextWidth(s string, size float32, w overlay.Weight) float32 {
	face := f.face(w, size)
	if face == nil || s == "" {
		return 0
	}
	return fx(font.MeasureString(face, s))
}

const ellipsis = "…"

// fit shortens s with an ellipsis until it is at most width wide, keeping
// its tail when head is set and its start otherwise.
func fit(face font.Face, s string, width float32, head bool) string {
	if fx(font.MeasureString(face, s)) <= width {
		return s
	}
	runes := []rune(s)
	lo, hi := 0, len(runes) // the longest kept count that fits, by bisection
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fx(font.MeasureString(face, clipRunes(runes, mid, head))) <= width {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return clipRunes(runes, lo, head)
}

func clipRunes(runes []rune, n int, head bool) string {
	if head {
		return ellipsis + strings.TrimLeft(string(runes[len(runes)-n:]), " ")
	}
	return strings.TrimRight(string(runes[:n]), " ") + ellipsis
}

// wrap breaks s into at most n lines no wider than width, at spaces where it
// can; the last line ends with an ellipsis when text remains.
func wrap(face font.Face, s string, width float32, n int) []string {
	words := strings.Fields(s)
	var lines []string
	for len(words) > 0 && len(lines) < n {
		line := words[0]
		used := 1
		for used < len(words) {
			next := line + " " + words[used]
			if fx(font.MeasureString(face, next)) > width {
				break
			}
			line, used = next, used+1
		}
		words = words[used:]
		if len(lines) == n-1 && len(words) > 0 {
			line = fit(face, line+" "+strings.Join(words, " "), width, false)
			words = nil
		} else {
			line = fit(face, line, width, false) // one word wider than the box
		}
		lines = append(lines, line)
	}
	return lines
}

// text draws a text op: aligned in its box, vertically centred, truncated
// or wrapped like the Pango and AppKit hosts.
func (r *Renderer) text(op *overlay.Op, s string) {
	if r.Fonts == nil || s == "" || op.W <= 0 {
		return
	}
	face := r.Fonts.face(overlay.Weight(op.Weight), op.FontSize*r.scale)
	if face == nil {
		return
	}
	width := op.W * r.scale
	var lines []string
	switch {
	case op.Lines > 1:
		lines = wrap(face, s, width, int(op.Lines))
	case op.Truncate == overlay.TruncNone:
		lines = []string{s}
	default:
		lines = []string{fit(face, s, width, op.Truncate == overlay.TruncHead)}
	}
	m := face.Metrics()
	lineH := fx(m.Ascent + m.Descent)
	th := min(lineH*float32(len(lines)), op.H*r.scale)
	top := op.Y*r.scale + (op.H*r.scale-th)/2

	// Glyphs may overhang their advance a little; only unwrapped,
	// untruncated text is clipped to its box, as in the other hosts.
	pad := int(lineH)
	box := image.Rect(int(math.Floor(float64(op.X*r.scale)))-pad, int(math.Floor(float64(top)))-pad,
		int(math.Ceil(float64((op.X+op.W)*r.scale)))+pad, int(math.Ceil(float64(top+lineH*float32(len(lines)))))+pad)
	if op.Truncate == overlay.TruncNone && op.Lines <= 1 {
		box = box.Intersect(image.Rect(int(math.Floor(float64(op.X*r.scale))), int(math.Floor(float64(op.Y*r.scale))),
			int(math.Ceil(float64((op.X+op.W)*r.scale))), int(math.Ceil(float64((op.Y+op.H)*r.scale)))))
	}
	r.area = box.Intersect(r.limit())
	if r.area.Empty() {
		return
	}
	dst := r.cover.SubImage(r.area).(*image.Alpha)
	for y := r.area.Min.Y; y < r.area.Max.Y; y++ {
		i := dst.PixOffset(r.area.Min.X, y)
		clear(dst.Pix[i : i+r.area.Dx()])
	}
	d := font.Drawer{Dst: dst, Src: image.Opaque, Face: face}
	for i, line := range lines {
		lw := fx(font.MeasureString(face, line))
		x := op.X * r.scale
		switch op.Align {
		case overlay.AlignCenter:
			x += (width - lw) / 2
		case overlay.AlignRight:
			x += width - lw
		}
		baseline := top + lineH*float32(i) + fx(m.Ascent)
		d.Dot = fixed.Point26_6{X: fixed.Int26_6(x * 64), Y: fixed.Int26_6(baseline * 64)}
		d.DrawString(line)
	}
	r.paint(op.Fill)
}
