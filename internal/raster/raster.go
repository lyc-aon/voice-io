// Package raster replays overlay display lists in pure Go: anti-aliased
// fills and strokes, soft shadows, rounded clips and text. The Windows host
// draws the overlay with it; it runs anywhere, so the drawing can be tested
// on every platform.
package raster

import (
	"image"
	"image/draw"
	"math"

	"golang.org/x/image/vector"

	"gitlab.com/integral-inference/voice-io/internal/overlay"
)

// Renderer replays display lists onto an RGBA canvas. It keeps scratch
// buffers between frames and is not safe for concurrent use.
type Renderer struct {
	Fonts *Fonts

	dst   *image.RGBA
	scale float32
	clips []clip
	cover *image.Alpha // coverage of the op being drawn, canvas sized
	area  image.Rectangle
	rast  vector.Rasterizer

	cur    []pt   // subpath being built, canvas pixels
	pen    *pt    // where a segment after close_path starts (its subpath's start)
	subs   [][]pt // finished subpaths
	closed []bool
	polys  [][]pt // polygons to rasterize
}

type clip struct {
	mask *image.Alpha
	rect image.Rectangle
}

type pt struct{ x, y float32 }

// Draw clears dst and replays l onto it, scaling logical pixels by scale.
func (r *Renderer) Draw(dst *image.RGBA, l *overlay.List, scale float32) {
	clear(dst.Pix)
	r.dst, r.scale = dst, scale
	if r.cover == nil || r.cover.Rect != dst.Rect {
		r.cover = image.NewAlpha(dst.Rect)
	}
	r.clips = r.clips[:0]
	for i := range l.Ops {
		op := &l.Ops[i]
		switch op.Kind {
		case overlay.OpRRect, overlay.OpCircle:
			if op.Flags&overlay.FlagShadow != 0 {
				r.shadow(op)
			}
			if op.Flags&overlay.FlagFill != 0 {
				r.shape(op, 0, 0)
				r.fillPolys(op.Fill)
			}
			if op.Flags&overlay.FlagStroke != 0 {
				r.ring(op)
				r.fillPolys(op.Stroke)
			}
		case overlay.OpPath:
			if op.Flags&overlay.FlagFill != 0 {
				r.path(l.PathBuf[op.Off : op.Off+op.Len])
				r.fillPolys(op.Fill)
			}
			if op.Flags&overlay.FlagStroke != 0 {
				r.path(l.PathBuf[op.Off : op.Off+op.Len])
				r.strokeSubs(op.LineW)
				r.paint(op.Stroke)
			}
		case overlay.OpArc:
			r.arc(op)
			r.strokeSubs(op.LineW)
			r.paint(op.Stroke)
		case overlay.OpText:
			r.text(op, string(l.TextBuf[op.Off:op.Off+op.Len]))
		case overlay.OpClipPush:
			r.pushClip(op)
		case overlay.OpClipPop:
			if len(r.clips) > 0 {
				r.clips = r.clips[:len(r.clips)-1]
			}
		}
	}
}

// ---- outlines -------------------------------------------------------------

// kappa places cubic control points that approximate a quarter circle.
const kappa = 0.5522847498

func (r *Renderer) begin() {
	r.cur, r.subs, r.closed, r.pen = r.cur[:0], r.subs[:0], r.closed[:0], nil
}

func (r *Renderer) moveTo(x, y float32) {
	r.endSub(false)
	r.cur, r.pen = append(r.cur, pt{x * r.scale, y * r.scale}), nil
}

// startAt begins a subpath at the pen, or at (x, y) when there is none.
func (r *Renderer) startAt(x, y float32) {
	if r.pen != nil {
		r.cur, r.pen = append(r.cur, *r.pen), nil
		return
	}
	r.moveTo(x, y)
}

func (r *Renderer) lineTo(x, y float32) {
	if len(r.cur) == 0 {
		r.startAt(x, y)
		if len(r.cur) == 1 && r.cur[0] == (pt{x * r.scale, y * r.scale}) {
			return
		}
	}
	r.cur = append(r.cur, pt{x * r.scale, y * r.scale})
}

func (r *Renderer) cubicTo(c1x, c1y, c2x, c2y, x, y float32) {
	if len(r.cur) == 0 {
		r.startAt(c1x, c1y)
	}
	p0 := r.cur[len(r.cur)-1]
	p1 := pt{c1x * r.scale, c1y * r.scale}
	p2 := pt{c2x * r.scale, c2y * r.scale}
	p3 := pt{x * r.scale, y * r.scale}
	n := int(math.Ceil(float64(dist(p0, p1)+dist(p1, p2)+dist(p2, p3)) / 2))
	n = min(max(n, 4), 64)
	for i := 1; i <= n; i++ {
		t := float32(i) / float32(n)
		u := 1 - t
		a, b, c, d := u*u*u, 3*u*u*t, 3*u*t*t, t*t*t
		r.cur = append(r.cur, pt{a*p0.x + b*p1.x + c*p2.x + d*p3.x, a*p0.y + b*p1.y + c*p2.y + d*p3.y})
	}
}

// endSub finishes the current subpath.
func (r *Renderer) endSub(closed bool) {
	if len(r.cur) > 0 {
		r.subs = append(r.subs, append([]pt(nil), r.cur...))
		r.closed = append(r.closed, closed)
	}
	r.cur = r.cur[:0]
}

func dist(a, b pt) float32 { return float32(math.Hypot(float64(a.x-b.x), float64(a.y-b.y))) }

// rrect adds a rounded rectangle subpath in logical pixels, clockwise on
// screen, or anticlockwise when reverse is set.
func (r *Renderer) rrect(x, y, w, h, rad float32, reverse bool) {
	if w <= 0 || h <= 0 {
		return
	}
	rad = max(min(rad, min(w, h)/2), 0)
	k := rad * kappa
	if !reverse {
		r.moveTo(x+rad, y)
		r.lineTo(x+w-rad, y)
		r.cubicTo(x+w-rad+k, y, x+w, y+rad-k, x+w, y+rad)
		r.lineTo(x+w, y+h-rad)
		r.cubicTo(x+w, y+h-rad+k, x+w-rad+k, y+h, x+w-rad, y+h)
		r.lineTo(x+rad, y+h)
		r.cubicTo(x+rad-k, y+h, x, y+h-rad+k, x, y+h-rad)
		r.lineTo(x, y+rad)
		r.cubicTo(x, y+rad-k, x+rad-k, y, x+rad, y)
	} else {
		r.moveTo(x+rad, y)
		r.cubicTo(x+rad-k, y, x, y+rad-k, x, y+rad)
		r.lineTo(x, y+h-rad)
		r.cubicTo(x, y+h-rad+k, x+rad-k, y+h, x+rad, y+h)
		r.lineTo(x+w-rad, y+h)
		r.cubicTo(x+w-rad+k, y+h, x+w, y+h-rad+k, x+w, y+h-rad)
		r.lineTo(x+w, y+rad)
		r.cubicTo(x+w, y+rad-k, x+w-rad+k, y, x+w-rad, y)
	}
	r.endSub(true)
}

// shape outlines a rounded rect or circle op grown outward by d and moved
// down by dy.
func (r *Renderer) shape(op *overlay.Op, d, dy float32) {
	r.begin()
	if op.Kind == overlay.OpCircle {
		rad := op.R + d
		r.rrect(op.X-rad, op.Y+dy-rad, 2*rad, 2*rad, rad, false)
	} else {
		r.rrect(op.X-d, op.Y+dy-d, op.W+2*d, op.H+2*d, op.R+d, false)
	}
}

// ring outlines the stroke of a rounded rect or circle op: the shape grown
// by half the line width, less the shape shrunk by it.
func (r *Renderer) ring(op *overlay.Op) {
	h := op.LineW / 2
	r.begin()
	if op.Kind == overlay.OpCircle {
		r.rrect(op.X-op.R-h, op.Y-op.R-h, 2*(op.R+h), 2*(op.R+h), op.R+h, false)
		if in := op.R - h; in > 0 {
			r.rrect(op.X-in, op.Y-in, 2*in, 2*in, in, true)
		}
		return
	}
	r.rrect(op.X-h, op.Y-h, op.W+2*h, op.H+2*h, op.R+h, false)
	if op.W > op.LineW && op.H > op.LineW {
		r.rrect(op.X+h, op.Y+h, op.W-op.LineW, op.H-op.LineW, op.R-h, true)
	}
}

// path flattens a display-list path.
func (r *Renderer) path(p []float32) {
	r.begin()
	for i := 0; i < len(p); {
		switch p[i] {
		case overlay.PathMove:
			r.moveTo(p[i+1], p[i+2])
			i += 3
		case overlay.PathLine:
			r.lineTo(p[i+1], p[i+2])
			i += 3
		case overlay.PathCubic:
			r.cubicTo(p[i+1], p[i+2], p[i+3], p[i+4], p[i+5], p[i+6])
			i += 7
		case overlay.PathClose:
			if len(r.cur) > 0 {
				start := r.cur[0]
				r.endSub(true)
				r.pen = &start
			}
			i++
		default:
			i = len(p)
		}
	}
	r.endSub(false)
}

// arc flattens a circular arc. As in cairo, angles grow clockwise on screen
// and a1 is raised past a0 by whole turns.
func (r *Renderer) arc(op *overlay.Op) {
	r.begin()
	a0, a1 := float64(op.A0), float64(op.A1)
	for a1 < a0 {
		a1 += 2 * math.Pi
	}
	n := max(int(math.Ceil((a1-a0)*float64(op.R*r.scale)/2)), 8)
	for i := 0; i <= n; i++ {
		a := a0 + (a1-a0)*float64(i)/float64(n)
		r.lineTo(op.X+op.R*float32(math.Cos(a)), op.Y+op.R*float32(math.Sin(a)))
	}
	r.endSub(false)
}

// strokeSubs turns the subpaths into the round-capped, round-joined outline
// of a line lineW wide: a quad per segment and a disc per vertex, all wound
// the same way so the rasterizer's coverage adds up to their union.
func (r *Renderer) strokeSubs(lineW float32) {
	h := lineW * r.scale / 2
	r.polys = r.polys[:0]
	for si, sub := range r.subs {
		for i := range sub {
			r.polys = append(r.polys, disc(sub[i], h))
			if i > 0 {
				r.polys = appendQuad(r.polys, sub[i-1], sub[i], h)
			}
		}
		if r.closed[si] && len(sub) > 1 {
			r.polys = appendQuad(r.polys, sub[len(sub)-1], sub[0], h)
		}
	}
	r.rasterize()
}

// appendQuad adds the rectangle around segment a→b.
func appendQuad(polys [][]pt, a, b pt, h float32) [][]pt {
	dx, dy := b.x-a.x, b.y-a.y
	l := float32(math.Hypot(float64(dx), float64(dy)))
	if l == 0 {
		return polys
	}
	nx, ny := -dy/l*h, dx/l*h
	return append(polys, []pt{{a.x + nx, a.y + ny}, {b.x + nx, b.y + ny}, {b.x - nx, b.y - ny}, {a.x - nx, a.y - ny}})
}

// disc is a polygonal disc of radius h around c, wound like appendQuad's
// rectangles.
func disc(c pt, h float32) []pt {
	n := min(max(int(math.Ceil(float64(h)*2*math.Pi/1.5)), 8), 64)
	poly := make([]pt, n)
	for i := range n {
		a := -2 * math.Pi * float64(i) / float64(n)
		poly[i] = pt{c.x + h*float32(math.Cos(a)), c.y + h*float32(math.Sin(a))}
	}
	return poly
}

// ---- coverage and compositing -------------------------------------------

// fillPolys rasterizes the subpaths as filled polygons (non-zero winding)
// and paints them.
func (r *Renderer) fillPolys(c [4]float32) {
	r.polys = append(r.polys[:0], r.subs...)
	r.rasterize()
	r.paint(c)
}

// limit is the drawable area: the canvas, narrowed by the innermost clip.
func (r *Renderer) limit() image.Rectangle {
	if n := len(r.clips); n > 0 {
		return r.clips[n-1].rect
	}
	return r.dst.Rect
}

// rasterize writes the coverage of r.polys into r.cover over r.area.
func (r *Renderer) rasterize() {
	r.area = image.Rectangle{}
	if len(r.polys) == 0 {
		return
	}
	minX, minY := float32(math.Inf(1)), float32(math.Inf(1))
	maxX, maxY := float32(math.Inf(-1)), float32(math.Inf(-1))
	for _, poly := range r.polys {
		for _, p := range poly {
			minX, minY, maxX, maxY = min(minX, p.x), min(minY, p.y), max(maxX, p.x), max(maxY, p.y)
		}
	}
	box := image.Rect(int(math.Floor(float64(minX))), int(math.Floor(float64(minY))),
		int(math.Ceil(float64(maxX))), int(math.Ceil(float64(maxY))))
	r.area = box.Intersect(r.limit())
	if r.area.Empty() {
		return
	}
	ox, oy := float32(r.area.Min.X), float32(r.area.Min.Y)
	r.rast.Reset(r.area.Dx(), r.area.Dy())
	r.rast.DrawOp = draw.Src
	for _, poly := range r.polys {
		if len(poly) < 3 {
			continue
		}
		r.rast.MoveTo(poly[0].x-ox, poly[0].y-oy)
		for _, p := range poly[1:] {
			r.rast.LineTo(p.x-ox, p.y-oy)
		}
		r.rast.ClosePath()
	}
	r.rast.Draw(r.cover, r.area, image.Opaque, image.Point{})
}

// paint composites color c (straight alpha) through r.cover over r.area,
// masked by the innermost clip.
func (r *Renderer) paint(c [4]float32) {
	if r.area.Empty() || c[3] <= 0 {
		return
	}
	a := min(c[3], 1) * 255
	sr, sg, sb := c[0]*a, c[1]*a, c[2]*a
	var cl *clip
	if n := len(r.clips); n > 0 {
		cl = &r.clips[n-1]
	}
	for y := r.area.Min.Y; y < r.area.Max.Y; y++ {
		ci := r.cover.PixOffset(r.area.Min.X, y)
		di := r.dst.PixOffset(r.area.Min.X, y)
		mi := 0
		if cl != nil {
			mi = cl.mask.PixOffset(r.area.Min.X, y)
		}
		for x := r.area.Min.X; x < r.area.Max.X; x, ci, di, mi = x+1, ci+1, di+4, mi+1 {
			cov := float32(r.cover.Pix[ci])
			if cl != nil {
				cov = cov * float32(cl.mask.Pix[mi]) / 255
			}
			if cov == 0 {
				continue
			}
			k := cov / 255
			pa := a * k
			inv := 1 - pa/255
			d := r.dst.Pix[di : di+4 : di+4]
			d[0] = uint8(sr*k + float32(d[0])*inv + 0.5)
			d[1] = uint8(sg*k + float32(d[1])*inv + 0.5)
			d[2] = uint8(sb*k + float32(d[2])*inv + 0.5)
			d[3] = uint8(pa + float32(d[3])*inv + 0.5)
		}
	}
}

// shadow paints a soft drop shadow as translucent layers grown by the blur
// radius, approximating a gaussian falloff (the cairo host does the same).
func (r *Renderer) shadow(op *overlay.Op) {
	const layers = 10
	blur := op.ShadowB
	if blur <= 0 || op.Shadow[3] <= 0 {
		return
	}
	for i := layers; i >= 1; i-- {
		t := float32(i) / layers
		c := op.Shadow
		c[3] = op.Shadow[3] * 2.2 / layers * (1 - t) * (1 - t)
		if c[3] <= 0 {
			continue
		}
		r.shape(op, blur*t-blur*0.35, op.ShadowDY)
		r.fillPolys(c)
	}
}

// pushClip narrows drawing to a rounded rectangle, within any outer clip.
func (r *Renderer) pushClip(op *overlay.Op) {
	r.begin()
	r.rrect(op.X, op.Y, op.W, op.H, op.R, false)
	r.polys = append(r.polys[:0], r.subs...)
	r.rasterize()
	mask := image.NewAlpha(r.dst.Rect)
	if !r.area.Empty() {
		for y := r.area.Min.Y; y < r.area.Max.Y; y++ {
			i := r.cover.PixOffset(r.area.Min.X, y)
			copy(mask.Pix[i:i+r.area.Dx()], r.cover.Pix[i:i+r.area.Dx()])
		}
		if n := len(r.clips); n > 0 {
			outer := r.clips[n-1].mask
			for y := r.area.Min.Y; y < r.area.Max.Y; y++ {
				i := mask.PixOffset(r.area.Min.X, y)
				for x := range r.area.Dx() {
					mask.Pix[i+x] = uint8(uint16(mask.Pix[i+x]) * uint16(outer.Pix[i+x]) / 255)
				}
			}
		}
	}
	r.clips = append(r.clips, clip{mask: mask, rect: r.area})
}
