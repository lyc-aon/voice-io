package overlay

// Icon names. Icons are drawn on a 24×24 grid with round caps.
type Icon uint8

const (
	IconNone Icon = iota
	IconClose
	IconCheck
	IconMic
	IconMicOff
	IconWarning
	IconPeople
	IconDoc
	IconStop
	IconInfo
	IconSparkle
)

// kappa approximates a quarter circle with a cubic Bézier.
const kappa = 0.5523

func circlePath(p *PathBuilder, cx, cy, r float32) {
	k := r * kappa
	p.Move(cx+r, cy).
		Cubic(cx+r, cy+k, cx+k, cy+r, cx, cy+r).
		Cubic(cx-k, cy+r, cx-r, cy+k, cx-r, cy).
		Cubic(cx-r, cy-k, cx-k, cy-r, cx, cy-r).
		Cubic(cx+k, cy-r, cx+r, cy-k, cx+r, cy).Close()
}

func roundRectPath(p *PathBuilder, x, y, w, h, r float32) {
	k := r * kappa
	p.Move(x+r, y).Line(x+w-r, y).
		Cubic(x+w-r+k, y, x+w, y+r-k, x+w, y+r).
		Line(x+w, y+h-r).
		Cubic(x+w, y+h-r+k, x+w-r+k, y+h, x+w-r, y+h).
		Line(x+r, y+h).
		Cubic(x+r-k, y+h, x, y+h-r+k, x, y+h-r).
		Line(x, y+r).
		Cubic(x, y+r-k, x+r-k, y, x+r, y).Close()
}

// DrawIcon renders icon centred at (cx, cy) with the given size in points.
func (l *List) DrawIcon(icon Icon, cx, cy, size float32, c Color) {
	s := size / 24
	ox, oy := cx-12*s, cy-12*s
	stroke := float32(2)
	switch icon {
	case IconClose:
		l.BeginPath(ox, oy, s).Move(7, 7).Line(17, 17).Move(17, 7).Line(7, 17).Stroke(c, 2.2)
	case IconCheck:
		l.BeginPath(ox, oy, s).Move(5.5, 12.5).Line(10, 17).Line(18.5, 7.5).Stroke(c, 2.6)
	case IconMic, IconMicOff:
		p := l.BeginPath(ox, oy, s)
		roundRectPath(p, 9, 3, 6, 11.5, 3)
		p.Stroke(c, stroke)
		l.BeginPath(ox, oy, s).Move(5.5, 11).
			Cubic(5.5, 14.6, 8.4, 17.5, 12, 17.5).
			Cubic(15.6, 17.5, 18.5, 14.6, 18.5, 11).
			Move(12, 17.5).Line(12, 21).Stroke(c, stroke)
		if icon == IconMicOff {
			l.BeginPath(ox, oy, s).Move(4, 4).Line(20, 20).Stroke(c, stroke)
		}
	case IconWarning:
		l.BeginPath(ox, oy, s).Move(12, 3.5).Line(21.5, 20).Line(2.5, 20).Close().Stroke(c, stroke)
		l.BeginPath(ox, oy, s).Move(12, 9.5).Line(12, 14).Stroke(c, 2.2)
		l.Circle(ox+12*s, oy+17*s, 1.25*s, c, Color{}, 0)
	case IconInfo:
		p := l.BeginPath(ox, oy, s)
		circlePath(p, 12, 12, 9)
		p.Stroke(c, stroke)
		l.BeginPath(ox, oy, s).Move(12, 11).Line(12, 16.5).Stroke(c, 2.2)
		l.Circle(ox+12*s, oy+7.8*s, 1.25*s, c, Color{}, 0)
	case IconPeople:
		p := l.BeginPath(ox, oy, s)
		circlePath(p, 9, 8.5, 3.5)
		p.Stroke(c, stroke)
		l.BeginPath(ox, oy, s).Move(2.5, 20).Cubic(2.5, 16, 5.5, 14, 9, 14).Cubic(12.5, 14, 15.5, 16, 15.5, 20).Stroke(c, stroke)
		p = l.BeginPath(ox, oy, s)
		p.Move(15.5, 5.2).Cubic(17.3, 5.2, 18.8, 6.7, 18.8, 8.5).Cubic(18.8, 10.3, 17.3, 11.8, 15.5, 11.8)
		p.Stroke(c, stroke)
		l.BeginPath(ox, oy, s).Move(18, 14.4).Cubic(20.2, 15.1, 21.5, 17.1, 21.5, 20).Stroke(c, stroke)
	case IconDoc:
		l.BeginPath(ox, oy, s).Move(6, 3).Line(14, 3).Line(19, 8).Line(19, 21).Line(6, 21).Close().Stroke(c, stroke)
		l.BeginPath(ox, oy, s).Move(14, 3).Line(14, 8).Line(19, 8).Move(9, 13).Line(16, 13).Move(9, 17).Line(14, 17).Stroke(c, stroke)
	case IconStop:
		p := l.BeginPath(ox, oy, s)
		roundRectPath(p, 7, 7, 10, 10, 2.5)
		p.Fill(c)
	case IconSparkle:
		l.BeginPath(ox, oy, s).Move(12, 3).Cubic(12.8, 8.2, 15.8, 11.2, 21, 12).
			Cubic(15.8, 12.8, 12.8, 15.8, 12, 21).Cubic(11.2, 15.8, 8.2, 12.8, 3, 12).
			Cubic(8.2, 11.2, 11.2, 8.2, 12, 3).Close().Fill(c)
	}
}
