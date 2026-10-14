package overlay

import (
	"math"
	"time"
)

// Scenario is a named overlay state reached by replaying daemon events.
type Scenario struct {
	Name, Title string
	Setup       func(m *Model, advance func(time.Duration))
}

// speak delivers a speech-like level envelope the way the daemon does:
// five 20 ms levels with each 100 ms capture block.
func speak(m *Model, advance func(time.Duration), seconds float64) {
	for i := range int(seconds * 50) {
		t := float64(i) / 50
		syllable := math.Max(0, math.Sin(t*2*math.Pi*4.3))
		phrase := 0.55 + 0.45*math.Sin(t*2.1)
		env := phrase * (0.25 + 0.75*syllable)
		m.Level(0.002 + 0.06*env*env)
		if i%5 == 4 {
			advance(100 * time.Millisecond)
		}
	}
}

const (
	pillCX = CanvasW - edgeMargin - pillW/2
	pillCY = CanvasH / 2
)

// hoverButton expands the resting pill and points at one of its buttons.
func hoverButton(m *Model, adv func(time.Duration), index int) {
	m.Draw(&List{})
	m.Pointer(CanvasW-edgeMargin-handleW/2, pillCY, true)
	adv(450 * time.Millisecond)
	h := float32(8 + buttonD + 8 + buttonD + 8)
	if m.setup != "" || m.state == "off" {
		h = 8 + buttonD + 8
	}
	y := float32(pillCY) - h/2 + 8 + buttonD/2 + float32(index)*(buttonD+8)
	m.Pointer(CanvasW-edgeMargin-hoverD/2, y, true)
	adv(500 * time.Millisecond)
}

// Scenarios lists representative overlay states for previews and review.
func Scenarios() []Scenario {
	return []Scenario{
		{Name: "idle", Title: "Resting", Setup: func(m *Model, adv func(time.Duration)) {
			m.State("idle")
			adv(500 * time.Millisecond)
		}},
		{Name: "hover-dictate", Title: "Hover: dictate", Setup: func(m *Model, adv func(time.Duration)) {
			hoverButton(m, adv, 0)
		}},
		{Name: "hover-meeting", Title: "Hover: meeting notes", Setup: func(m *Model, adv func(time.Duration)) {
			hoverButton(m, adv, 1)
		}},
		{Name: "off", Title: "Voice off", Setup: func(m *Model, adv func(time.Duration)) {
			m.State("off")
			hoverButton(m, adv, 0)
		}},
		{Name: "setup", Title: "Setup needed", Setup: func(m *Model, adv func(time.Duration)) {
			m.SetSetup("Finish setup to start dictating")
			hoverButton(m, adv, 0)
		}},
		{Name: "recording", Title: "Dictating", Setup: func(m *Model, adv func(time.Duration)) {
			m.SessionOpen("dictation")
			m.State("listening")
			speak(m, adv, 4.2)
			m.Preview("the release notes for Friday")
			speak(m, adv, 0.4)
		}},
		{Name: "recording-controls", Title: "Dictating, hovered", Setup: func(m *Model, adv func(time.Duration)) {
			m.SessionOpen("dictation")
			m.State("listening")
			speak(m, adv, 2.6)
			m.Pointer(pillCX, pillCY, true)
			speak(m, adv, 0.6)
		}},
		{Name: "meeting", Title: "Meeting notes", Setup: func(m *Model, adv func(time.Duration)) {
			m.SessionOpen("meeting")
			m.State("listening")
			speak(m, adv, 3)
			m.Show("Meeting recording", "info")
			speak(m, adv, 0.4)
		}},
		{Name: "processing", Title: "Transcribing", Setup: func(m *Model, adv func(time.Duration)) {
			m.SessionOpen("dictation")
			m.State("listening")
			speak(m, adv, 2)
			m.State("processing")
			m.Processing("Transcribing")
			adv(400 * time.Millisecond)
		}},
		{Name: "inserted", Title: "Inserted", Setup: func(m *Model, adv func(time.Duration)) {
			m.Result(Result{Text: "Inserted", Style: "ok", AutoClose: 1.4})
			adv(400 * time.Millisecond)
		}},
		{Name: "copied", Title: "Copied (paste automatically off)", Setup: func(m *Model, adv func(time.Duration)) {
			m.Result(Result{Text: "Copied — paste with ⌘V", Style: "ok", AutoClose: 2.5, ShowText: true})
			adv(400 * time.Millisecond)
		}},
		{Name: "allow", Title: "Copied; Accessibility needed to paste", Setup: func(m *Model, adv func(time.Duration)) {
			m.Result(Result{Text: "Copied — allow Accessibility to paste", Style: "warn", AutoClose: 6, ShowText: true,
				Permission: "accessibility"})
			adv(400 * time.Millisecond)
		}},
		{Name: "notes", Title: "Meeting notes saved", Setup: func(m *Model, adv func(time.Duration)) {
			m.Result(Result{Text: "Meeting notes saved", Style: "ok", AutoClose: 4, ShowText: true, Reveal: "/tmp/notes.md"})
			adv(400 * time.Millisecond)
		}},
		{Name: "error", Title: "Error", Setup: func(m *Model, adv func(time.Duration)) {
			m.Result(Result{Text: "Paste failed — transcript saved for recovery", Style: "error", AutoClose: 3})
			adv(400 * time.Millisecond)
		}},
		{Name: "warning", Title: "Warning during a session", Setup: func(m *Model, adv func(time.Duration)) {
			m.SessionOpen("dictation")
			m.State("listening")
			speak(m, adv, 3)
			m.Show("Session ending soon — press Stop to finish", "warn")
			speak(m, adv, 0.4)
		}},
	}
}

// RenderScenario draws a scenario's settled frame into l using cfg.
func RenderScenario(s Scenario, cfg Config, dark bool, measure Measurer, l *List) Frame {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	m := New(measure, clock)
	m.SetConfig(cfg)
	m.SetSystemDark(dark)
	advance := func(d time.Duration) {
		for d > 0 {
			step := min(d, 16*time.Millisecond)
			now = now.Add(step)
			d -= step
			m.Draw(l)
		}
	}
	s.Setup(m, advance)
	return m.Draw(l)
}
