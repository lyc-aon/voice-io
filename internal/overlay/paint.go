package overlay

import (
	"math"
	"time"
)

const (
	labelSize  = 12.5
	chipSize   = 11
	buttonD    = 30 // resting pill buttons
	ctrlD      = 26 // cancel and stop on the recording pill
	barThick   = 2.1
	barGap     = 1.6
	barMaxLen  = 20
	barsBlock  = bars*barThick + (bars-1)*barGap // ≈ 69
	recPad     = 14
	revealStep = ctrlD + 6
	timerH     = 18
)

// pillLayout is the target geometry for a visual.
type pillLayout struct {
	w, h     float64
	controls bool // recording: cancel/stop revealed on hover
	timer    bool
}

func (m *Model) recordingLayout() pillLayout {
	l := pillLayout{w: pillW, h: recPad + barsBlock + recPad}
	if m.sessionMode == "meeting" || m.sessionMode == "collaborative" {
		l.timer = true
		l.h += timerH
	}
	switch m.hoverRegion {
	case "body", "cancel", "stop":
		l.controls = true
		l.h += 2 * revealStep
	}
	return l
}

func (m *Model) layout(v visual) pillLayout {
	switch v {
	case vHandle:
		return pillLayout{w: handleW, h: handleH}
	case vHover:
		if m.setup != "" || m.state == "off" {
			return pillLayout{w: hoverD, h: 8 + buttonD + 8}
		}
		return pillLayout{w: hoverD, h: 8 + buttonD + 8 + buttonD + 8}
	case vRecording:
		return m.recordingLayout()
	case vProcessing:
		return pillLayout{w: pillW, h: 52}
	case vResult:
		return pillLayout{w: resultD, h: resultD}
	}
	return pillLayout{}
}

func (m *Model) procText() string {
	if m.procLabel == "" {
		return "Transcribing…"
	}
	return m.procLabel + "…"
}

// paint draws visual v and reports whether animation continues.
func (m *Model) paint(l *List, v visual, now time.Time, dt float64) bool {
	target := m.layout(v)
	m.pillW.set(target.w, false)
	m.pillH.set(target.h, false)
	moving := m.pillW.step(dt)
	moving = m.pillH.step(dt) || moving
	w, h := float32(m.pillW.x), float32(m.pillH.x)
	right := float32(CanvasW - edgeMargin)
	x := right - w
	cy := float32(CanvasH) / 2
	y := cy - h/2
	r := w / 2

	age := now.Sub(m.visualSince).Seconds()
	grow := float32(1)
	if float64(h) < target.h {
		grow = float32(clamp((float64(h)-0.6*target.h)/(0.4*target.h), 0, 1))
	}
	alpha := float32(easeOut(ramp(age, 0.18))) * grow
	animating := moving || age < 0.25

	switch v {
	case vHandle:
		animating = m.paintHandle(l, x, y, w, h, now) || animating
		// Generous target: reaching for the screen edge finds it.
		m.addRegion("handle", x-26, cy-36, float32(CanvasW)-(x-26), 72)
	case vHover:
		// The pill region first: button regions added later win hit tests.
		m.addRegion("hover", x-6, y-6, float32(CanvasW)-(x-6), h+12)
		m.paintSurface(l, x, y, w, h, r)
		l.ClipRRect(x, y, w, h, r)
		l.PushAlpha(alpha)
		m.paintHover(l, x, y, w)
		l.PopAlpha()
		l.PopClip()
		m.paintHoverHint(l, x, y, w, alpha)
	case vRecording:
		m.paintSurface(l, x, y, w, h, r)
		l.ClipRRect(x, y, w, h, r)
		l.PushAlpha(alpha)
		m.paintRecording(l, x, y, w, h, target, now, dt)
		l.PopAlpha()
		l.PopClip()
		animating = true
	case vProcessing:
		m.paintSurface(l, x, y, w, h, r)
		l.ClipRRect(x, y, w, h, r)
		l.PushAlpha(alpha)
		m.paintProcessing(l, x, y, w, h, now)
		l.PopAlpha()
		l.PopClip()
		m.bubble(l, x, cy, m.procText(), "", IconNone, Color{}, alpha, "")
		animating = true
	case vResult:
		m.paintSurface(l, x, y, w, h, r)
		l.ClipRRect(x, y, w, h, r)
		l.PushAlpha(alpha)
		m.paintResultIcon(l, x+w/2, cy)
		l.PopAlpha()
		l.PopClip()
		m.addRegion("result", x, y, w, h)
		if m.result.Text != "" {
			chip := ""
			switch {
			case m.result.Permission != "":
				chip = "Allow"
			case m.result.Reveal != "":
				chip = "Open"
			}
			m.bubble(l, x, cy, m.result.Text, chip, IconNone, Color{}, alpha, "bubble")
		}
		animating = true
	}
	if v == vRecording {
		animating = m.paintSideText(l, x, cy, now) || animating
	}
	return animating
}

func (m *Model) addRegion(id string, x, y, w, h float32) {
	m.regions = append(m.regions, region{id: id, x: x, y: y, w: w, h: h})
}

func (m *Model) paintSurface(l *List, x, y, w, h, r float32) {
	t := m.theme
	l.RRect(x, y, w, h, r, t.Surface, t.Border, 1, &Shadow{Color: t.Shadow, Blur: 16, DY: 5})
}

func (m *Model) paintHandle(l *List, x, y, w, h float32, now time.Time) bool {
	t := m.theme
	fill, rim := t.Handle, t.HandleRim
	animating := false
	if now.Before(m.readyUntil) {
		left := m.readyUntil.Sub(now).Seconds()
		phase := 0.5 + 0.5*math.Sin(2*math.Pi*1.1*(readyTime.Seconds()-left))
		k := float32(phase * math.Min(1, left/0.6))
		fill = Mix(fill, t.Glow.WithAlpha(0.85), k*0.6)
		animating = true
	}
	if m.state == "off" {
		fill, rim = fill.WithAlpha(0.45), rim.WithAlpha(0.45)
	}
	l.RRect(x, y, w, h, w/2, fill, rim, 1, &Shadow{Color: t.Shadow.WithAlpha(0.5), Blur: 6, DY: 1})
	if m.setup != "" {
		l.Circle(x+w/2, y-7, 3, t.Warning, Color{}, 0)
	}
	return animating
}

// hoverButtons returns the resting buttons: dictate (mic) and meeting notes.
func (m *Model) hoverButtons(x, y, w float32) []struct {
	id     string
	cx, cy float32
} {
	cx := x + w/2
	first := y + 8 + buttonD/2
	if m.setup != "" || m.state == "off" {
		return []struct {
			id     string
			cx, cy float32
		}{{"mic", cx, first}}
	}
	return []struct {
		id     string
		cx, cy float32
	}{{"mic", cx, first}, {"meeting", cx, first + buttonD + 8}}
}

func (m *Model) paintHover(l *List, x, y, w float32) {
	t := m.theme
	for _, b := range m.hoverButtons(x, y, w) {
		if m.hoverRegion == b.id {
			l.Circle(b.cx, b.cy, buttonD/2, t.ButtonHot, Color{}, 0)
		}
		m.addRegion(b.id, b.cx-buttonD/2-4, b.cy-buttonD/2-4, buttonD+8+float32(edgeMargin), buttonD+8)
		switch {
		case b.id == "mic" && m.setup != "":
			l.DrawIcon(IconWarning, b.cx, b.cy, 17, t.Warning)
		case b.id == "mic" && m.state == "off":
			l.DrawIcon(IconMicOff, b.cx, b.cy, 17, t.Secondary)
		case b.id == "mic":
			l.DrawIcon(IconMic, b.cx, b.cy, 17, t.Ink)
		case b.id == "meeting":
			l.Circle(b.cx, b.cy, 8.5, Color{}, t.Ink, 1.7)
			l.Circle(b.cx, b.cy, 4.8, t.Record, Color{}, 0)
		}
	}
}

// paintHoverHint shows the tooltip for the hovered resting button.
func (m *Model) paintHoverHint(l *List, x, y, w, alpha float32) {
	for _, b := range m.hoverButtons(x, y, w) {
		if m.hoverRegion != b.id {
			continue
		}
		label, chip := "", ""
		switch {
		case b.id == "mic" && m.setup != "":
			label, chip = m.setup, "Settings"
		case b.id == "mic" && m.state == "off":
			label = "Voice is off — click to turn on"
		case b.id == "mic":
			label, chip = "Dictate", m.cfg.Shortcut
		case b.id == "meeting":
			label = "Start meeting notes"
		}
		m.bubble(l, x, b.cy, label, chip, IconNone, Color{}, alpha, "hint")
	}
}

// advanceLevels moves the level playhead by dt: one received level per
// 20 ms of frame time. A late burst holds the last level (gently decaying)
// instead of dropping to silence; a backlog is skipped so the waveform never
// lags the voice by more than a few levels.
func (m *Model) advanceLevels(dt float64) {
	if len(m.levels) > 8 {
		m.levels = m.levels[len(m.levels)-4:]
	}
	m.levelAcc += dt
	for m.levelAcc >= levelStep {
		m.levelAcc -= levelStep
		v := m.history[0] * 0.8
		if len(m.levels) > 0 {
			v = m.levels[0]
			m.levels = m.levels[1:]
		}
		copy(m.history[1:], m.history[:historyLen-1])
		m.history[0] = v
		// Automatic gain follows the loudest recent level and relaxes over
		// a few seconds, so a soft voice still fills the pill.
		m.peak = math.Max(v, m.peak-levelStep*0.3)
	}
}

// waveform draws the voice as thin horizontal bars stacked vertically and
// colored top to bottom. The newest 20 ms is at the centre and older levels
// ripple outward, so speech reads as a wave passing through the pill; while
// it is quiet the resting dots shimmer to show the microphone is open.
func (m *Model) waveform(l *List, cx, cy float32, dt float64, live bool, now time.Time) {
	t := m.theme
	if live {
		m.advanceLevels(dt)
	}
	gain := 1 / math.Max(m.peak, 0.45)
	rise := 1 - math.Exp(-dt*32)
	fall := 1 - math.Exp(-dt*11)
	clock := now.Sub(m.sessionStart).Seconds()
	energy := 0.0
	for _, v := range m.history {
		energy += v * gain
	}
	quiet := clamp(1-energy/2.5, 0, 1)
	center := bars / 2
	for i := range bars {
		dist := i - center
		if dist < 0 {
			dist = -dist
		}
		target := 0.0
		if live {
			v := m.history[dist] * gain * (1 - 0.028*float64(dist))
			// Each bar breathes at its own rate so a steady sound still looks alive.
			v *= 0.86 + 0.14*math.Sin(clock*(5.1+0.85*float64(i%4))+float64(i)*1.7)
			target = clamp(v, 0, 1)
		}
		k := fall
		if target > m.heights[i] {
			k = rise
		}
		m.heights[i] += (target - m.heights[i]) * k
		h := m.heights[i]
		length := float32(barThick + (barMaxLen-barThick)*math.Pow(h, 0.85))
		by := cy - barsBlock/2 + float32(i)*(barThick+barGap)
		base := t.waveColor(float32(i) / float32(bars-1))
		alpha := 0.5 + 0.5*math.Min(1, h*1.8)
		if live && quiet > 0 {
			alpha += quiet * 0.4 * math.Pow(math.Max(0, math.Sin(clock*2.4-float64(i)*0.42)), 4)
		}
		var glow *Shadow
		if h > 0.15 {
			strength := 0.65
			if !t.Dark {
				strength = 0.35
			}
			glow = &Shadow{Color: base.WithAlpha(float32(strength * h)), Blur: float32(2 + 6*h)}
		}
		l.RRect(cx-length/2, by, length, barThick, barThick/2, base.WithAlpha(float32(math.Min(1, alpha))), Color{}, 0, glow)
	}
}

func (m *Model) paintRecording(l *List, x, y, w, h float32, target pillLayout, now time.Time, dt float64) {
	t := m.theme
	cx := x + w/2
	// Revealed controls ease in with the extra height.
	extra := float32(0)
	if target.controls {
		extra = float32(clamp((float64(h)-(target.h-2*revealStep))/(2*revealStep), 0, 1))
	}
	barsCY := y + h/2
	if target.timer {
		barsCY -= timerH / 2
	}
	m.waveform(l, cx, barsCY, dt, m.state == "listening", now)
	if target.timer {
		ty := barsCY + barsBlock/2 + 5
		timer := m.elapsed(now)
		pulse := float32(0.6 + 0.4*math.Sin(2*math.Pi*0.9*now.Sub(m.sessionStart).Seconds()))
		l.Text(timer, x, ty, w, 12, TextStyle{Size: 9.5, Weight: Semibold, Color: t.Record.WithAlpha(pulse),
			Align: AlignCenter, Mono: true})
	}
	m.addRegion("body", x-4, y, w+8+float32(edgeMargin), h)
	if extra > 0 {
		l.PushAlpha(extra)
		ccy := y + 5 + ctrlD/2
		fill := t.Button
		if m.hoverRegion == "cancel" {
			fill = t.ButtonHot
		}
		l.Circle(cx, ccy, ctrlD/2-1, fill, Color{}, 0)
		l.DrawIcon(IconClose, cx, ccy, 12, t.Secondary)
		m.addRegion("cancel", x-4, ccy-ctrlD/2-2, w+8+float32(edgeMargin), ctrlD+4)
		scy := y + h - 5 - ctrlD/2
		stop := t.Ink
		if m.hoverRegion == "stop" {
			stop = Mix(t.Ink, t.Surface, 0.15)
		}
		l.Circle(cx, scy, ctrlD/2-1, stop, Color{}, 0)
		l.DrawIcon(IconStop, cx, scy, 15, t.OnInk)
		m.addRegion("stop", x-4, scy-ctrlD/2-2, w+8+float32(edgeMargin), ctrlD+4)
		l.PopAlpha()
	}
}

func (m *Model) paintProcessing(l *List, x, y, w, h float32, now time.Time) {
	t := m.theme
	cx, cy := x+w/2, y+h/2
	spin := now.Sub(m.procSince).Seconds()
	a0 := float32(math.Mod(spin*2*math.Pi/0.9, 2*math.Pi))
	l.Circle(cx, cy, 8, Color{}, t.Ink.WithAlpha(0.16), 2)
	l.Arc(cx, cy, 8, a0, a0+float32(1.3*math.Pi), 2, t.Glow)
	m.addRegion("cancel", x-4, y, w+8+float32(edgeMargin), h)
}

func (m *Model) paintResultIcon(l *List, cx, cy float32) {
	t := m.theme
	tone := t.Tone(m.result.Style)
	l.Circle(cx, cy, 11, tone, Color{}, 0)
	if m.result.Style == "ok" {
		l.DrawIcon(IconCheck, cx, cy, 14.5, RGB(255, 255, 255))
		return
	}
	l.BeginPath(cx-12, cy-12, 1).Move(12, 7).Line(12, 13.2).Stroke(RGB(255, 255, 255), 2.2)
	l.Circle(cx, cy+4.6, 1.3, RGB(255, 255, 255), Color{}, 0)
}

// bubble draws a text bubble to the left of the pill, vertically centred
// on cy. chip adds a small key/action badge; id makes it clickable.
func (m *Model) bubble(l *List, pillX, cy float32, text, chip string, icon Icon, iconC Color, alpha float32, id string) {
	if text == "" || alpha <= 0 {
		return
	}
	t := m.theme
	text = m.tailFit(text, labelSize, Medium, maxBubbleW-40)
	tw := m.measure.TextWidth(text, labelSize, Medium, false)
	bw := tw + 26
	if icon != IconNone {
		bw += 22
	}
	cw := float32(0)
	if chip != "" {
		cw = m.measure.TextWidth(chip, chipSize, Semibold, false) + 12
		bw += cw + 8
	}
	bx := pillX - bubbleGap - bw
	by := cy - bubbleH/2
	l.PushAlpha(alpha)
	l.RRect(bx, by, bw, bubbleH, bubbleH/2, t.Surface, t.Border, 1, &Shadow{Color: t.Shadow.WithAlpha(0.8), Blur: 12, DY: 3})
	tx := bx + 13
	if icon != IconNone {
		l.DrawIcon(icon, tx+8, cy, 15, iconC)
		tx += 22
	}
	l.Text(text, tx, by, tw+2, bubbleH, TextStyle{Size: labelSize, Weight: Medium, Color: t.Text})
	if chip != "" {
		chipX := bx + bw - 9 - cw
		fill := t.Button
		if id == "bubble" {
			fill = t.Ink.WithAlpha(0.16)
		}
		l.RRect(chipX, cy-9.5, cw, 19, 6, fill, Color{}, 0, nil)
		l.Text(chip, chipX, cy-9.5, cw, 19, TextStyle{Size: chipSize, Weight: Semibold, Color: t.Secondary, Align: AlignCenter})
	}
	l.PopAlpha()
	if id != "" {
		m.addRegion(id, bx, by, bw, bubbleH)
	}
}

// paintSideText shows a toast (priority) or the live preview beside the pill.
func (m *Model) paintSideText(l *List, pillX, cy float32, now time.Time) bool {
	t := m.theme
	fade := func(start, until time.Time, forever bool) float32 {
		in := easeOut(ramp(now.Sub(start).Seconds(), 0.14))
		if forever {
			return float32(in)
		}
		return float32(math.Min(in, clamp(until.Sub(now).Seconds()/0.35, 0, 1)))
	}
	if m.toastActive(now) {
		tt := m.toast
		icon, ic := IconInfo, t.Ink
		switch {
		case tt.style == "warn" || tt.style == "error":
			icon, ic = IconWarning, t.Tone(tt.style)
		case m.sessionMode == "meeting" || m.sessionMode == "collaborative":
			icon, ic = IconPeople, t.Ink
		}
		m.bubble(l, pillX, cy, tt.text, "", icon, ic, fade(tt.until.Add(-toastTime), tt.until, tt.forever), "")
		return true
	}
	if m.preview != "" && now.Before(m.previewUntil) && m.state == "listening" {
		a := fade(m.previewAt, m.previewUntil, false)
		text := m.tailFit(m.preview, labelSize, Regular, maxBubbleW-28)
		tw := m.measure.TextWidth(text, labelSize, Regular, false)
		bw := tw + 26
		bx, by := pillX-bubbleGap-bw, cy-bubbleH/2
		l.PushAlpha(a)
		l.RRect(bx, by, bw, bubbleH, bubbleH/2, t.Surface.WithAlpha(0.9), t.Border, 1, &Shadow{Color: t.Shadow.WithAlpha(0.6), Blur: 12, DY: 3})
		l.Text(text, bx+13, by, tw+2, bubbleH, TextStyle{Size: labelSize, Color: t.Preview, Truncate: TruncHead})
		l.PopAlpha()
		return true
	}
	return false
}
