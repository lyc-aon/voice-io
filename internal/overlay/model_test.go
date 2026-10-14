package overlay

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const frame = 16 * time.Millisecond

type rig struct {
	t       *testing.T
	now     time.Time
	m       *Model
	l       List
	actions []Action
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, now: time.Unix(1_800_000_000, 0)}
	r.m = New(nil, func() time.Time { return r.now })
	r.m.SetHooks(nil, func(a Action) { r.actions = append(r.actions, a) })
	r.m.SetConfig(Config{Enabled: true, Theme: "dark", IdleHandle: true, LivePreview: true, Shortcut: "fn"})
	return r
}

// run draws frames at 60 fps for d, as the native host does while animating.
func (r *rig) run(d time.Duration) Frame {
	f := r.m.Draw(&r.l)
	for elapsed := time.Duration(0); elapsed < d; elapsed += frame {
		r.now = r.now.Add(frame)
		f = r.m.Draw(&r.l)
	}
	return f
}

func (r *rig) regions() map[string][2]float32 {
	out := map[string][2]float32{}
	for _, g := range r.m.regions {
		out[g.id] = [2]float32{g.x + g.w/2, g.y + g.h/2}
	}
	return out
}

func (r *rig) center(id string) (float32, float32) {
	r.t.Helper()
	c, ok := r.regions()[id]
	if !ok {
		r.t.Fatalf("no %q region; have %v", id, r.regions())
	}
	return c[0], c[1]
}

func (r *rig) hover(id string) {
	r.t.Helper()
	x, y := r.center(id)
	r.m.Pointer(x, y, true)
	r.run(400 * time.Millisecond)
}

func (r *rig) click(id string) *Action {
	r.t.Helper()
	x, y := r.center(id)
	before := len(r.actions)
	r.m.MouseDown(x, y, 500, 0)
	r.m.MouseUp(x, y)
	r.run(frame)
	if len(r.actions) == before {
		return nil
	}
	return &r.actions[len(r.actions)-1]
}

func (r *rig) text() string { return string(r.l.TextBuf) }

func TestHoverOffersDictationAndMeetingNotes(t *testing.T) {
	r := newRig(t)
	if f := r.run(0); !f.Visible || f.Active {
		t.Fatalf("idle frame %+v", f)
	}
	if _, ok := r.regions()["mic"]; ok {
		t.Fatal("buttons shown before hover")
	}
	r.hover("handle")
	r.hover("mic")
	if !strings.Contains(r.text(), "Dictate") || !strings.Contains(r.text(), "fn") {
		t.Errorf("mic tooltip %q", r.text())
	}
	if a := r.click("mic"); a == nil || a.Kind != "dictate" {
		t.Fatalf("mic click %+v", a)
	}
	r.hover("meeting")
	if !strings.Contains(r.text(), "Start meeting notes") {
		t.Errorf("meeting tooltip %q", r.text())
	}
	if a := r.click("meeting"); a == nil || a.Kind != "meeting" {
		t.Fatalf("meeting click %+v", a)
	}
	r.m.Pointer(0, 0, false)
	r.run(400 * time.Millisecond)
	if _, ok := r.regions()["mic"]; ok {
		t.Fatal("buttons stayed after the pointer left")
	}
}

func TestOffAndSetupChangeWhatTheMicDoes(t *testing.T) {
	r := newRig(t)
	r.m.State("off")
	r.run(0)
	r.hover("handle")
	if a := r.click("mic"); a == nil || a.Kind != "toggle" {
		t.Fatalf("off: %+v", a)
	}
	r.m.State("idle")
	r.m.SetSetup("Download speech models")
	r.hover("mic")
	if !strings.Contains(r.text(), "Download speech models") {
		t.Errorf("setup hint %q", r.text())
	}
	if a := r.click("mic"); a == nil || a.Kind != "settings" {
		t.Fatalf("setup: %+v", a)
	}
}

func TestRecordingPillStopsOrCancels(t *testing.T) {
	r := newRig(t)
	r.m.SessionOpen("dictation")
	r.m.State("listening")
	if f := r.run(300 * time.Millisecond); !f.Active || !f.Animating {
		t.Fatalf("recording frame %+v", f)
	}
	r.hover("body")
	if a := r.click("stop"); a == nil || a.Kind != "push" {
		t.Fatalf("stop: %+v", a)
	}
	if a := r.click("cancel"); a == nil || a.Kind != "cancel" {
		t.Fatalf("cancel: %+v", a)
	}
}

func TestMeetingPillShowsElapsedTime(t *testing.T) {
	r := newRig(t)
	r.m.SessionOpen("meeting")
	r.m.State("listening")
	r.run(300 * time.Millisecond)
	r.now = r.now.Add(75 * time.Second)
	r.run(frame)
	if !strings.Contains(r.text(), "1:15") {
		t.Fatalf("meeting timer missing: %q", r.text())
	}
}

func TestSuccessNeverDisplaysTheTranscript(t *testing.T) {
	r := newRig(t)
	r.m.SessionOpen("dictation")
	r.m.State("processing")
	r.m.Result(Result{Text: "private dictated text", Style: "ok", AutoClose: 2})
	r.m.SessionClose()
	r.m.State("idle")
	if f := r.run(1900 * time.Millisecond); !f.Active {
		t.Fatal("result closed early")
	}
	if strings.Contains(r.text(), "private dictated text") {
		t.Fatalf("transcript displayed: %q", r.text())
	}
	if f := r.run(200 * time.Millisecond); f.Active {
		t.Fatal("result did not auto-close")
	}
}

func TestMeetingNotesResultCanBeOpened(t *testing.T) {
	r := newRig(t)
	r.m.Result(Result{Text: "Meeting notes saved", Style: "ok", AutoClose: 2.5, ShowText: true, Reveal: "/tmp/notes.md"})
	r.run(400 * time.Millisecond)
	if !strings.Contains(r.text(), "Meeting notes saved") || !strings.Contains(r.text(), "Open") {
		t.Fatalf("status %q", r.text())
	}
	if a := r.click("bubble"); a == nil || a.Kind != "reveal" || a.Arg != "/tmp/notes.md" {
		t.Fatalf("open notes: %+v", a)
	}
	if f := r.run(2500 * time.Millisecond); f.Active {
		t.Fatal("notes result did not close")
	}
}

func TestPermissionResultOffersToAllow(t *testing.T) {
	r := newRig(t)
	r.m.Result(Result{Text: "Copied — allow Accessibility to paste", Style: "warn", AutoClose: 6, ShowText: true,
		Permission: "accessibility"})
	r.run(400 * time.Millisecond)
	if !strings.Contains(r.text(), "allow Accessibility") || !strings.Contains(r.text(), "Allow") {
		t.Fatalf("result %q", r.text())
	}
	if a := r.click("bubble"); a == nil || a.Kind != "permission" || a.Arg != "accessibility" {
		t.Fatalf("click: %+v", a)
	}
}

// barLengths returns the drawn waveform bar lengths, top to bottom.
func barLengths(l *List) []float32 {
	var out []float32
	for _, op := range l.Ops {
		if op.Kind == OpRRect && op.H == barThick {
			out = append(out, op.W)
		}
	}
	return out
}

// feed delivers levels like the daemon: five 20 ms levels per 100 ms block.
func (r *rig) feed(rms float64, d time.Duration) {
	for elapsed := time.Duration(0); elapsed < d; elapsed += 100 * time.Millisecond {
		for range 5 {
			r.m.Level(rms)
		}
		r.run(100 * time.Millisecond)
	}
}

func TestWaveformFollowsSpeechAndRestsInSilence(t *testing.T) {
	r := newRig(t)
	r.m.SessionOpen("dictation")
	r.m.State("listening")
	r.feed(0.03, 600*time.Millisecond) // ordinary speech, about -30 dBFS
	speaking := barLengths(&r.l)
	if len(speaking) != bars {
		t.Fatalf("%d bars", len(speaking))
	}
	if slices.Max(speaking) < 0.7*barMaxLen {
		t.Fatalf("speech barely moved the waveform: %v", speaking)
	}
	r.feed(0, 1500*time.Millisecond)
	for _, w := range barLengths(&r.l) {
		if w > barThick+1 {
			t.Fatalf("waveform did not settle in silence: %v", barLengths(&r.l))
		}
	}
}

func TestQuietVoiceStillFillsTheWaveform(t *testing.T) {
	r := newRig(t)
	r.m.SessionOpen("dictation")
	r.m.State("listening")
	r.feed(0.004, 2*time.Second) // a soft voice, about -48 dBFS above the room
	if got := slices.Max(barLengths(&r.l)); got < 0.6*barMaxLen {
		t.Fatalf("soft voice reached %.1f of %d", got, barMaxLen)
	}
}

func TestPreviewIsTransientAndYieldsToWarnings(t *testing.T) {
	r := newRig(t)
	r.m.SessionOpen("dictation")
	r.m.State("listening")
	r.m.Preview("release notes for friday")
	r.run(300 * time.Millisecond)
	if !strings.Contains(r.text(), "release notes for friday") {
		t.Fatalf("preview %q", r.text())
	}
	r.m.Show("ASR error — recovered", "warn")
	r.run(300 * time.Millisecond)
	if strings.Contains(r.text(), "release notes") || !strings.Contains(r.text(), "ASR error") {
		t.Fatalf("warning did not replace preview: %q", r.text())
	}
	r.run(toastTime)
	if strings.Contains(r.text(), "ASR error") {
		t.Fatal("warning did not expire")
	}
	r.m.Preview("and email the team")
	r.run(previewTime + 50*time.Millisecond)
	if strings.Contains(r.text(), "email the team") {
		t.Fatal("preview did not expire")
	}

	r.m.SetConfig(Config{Enabled: true, Theme: "dark", IdleHandle: true, LivePreview: false})
	r.m.Preview("hidden words")
	r.run(300 * time.Millisecond)
	if strings.Contains(r.text(), "hidden words") {
		t.Fatal("preview shown with live preview off")
	}
}

func TestDraggingMovesThePillAlongTheEdge(t *testing.T) {
	r := newRig(t)
	r.m.SetScreenHeight(1000)
	r.run(0)
	x, y := r.center("handle")
	r.m.MouseDown(x, y, 500, 0)
	r.m.MouseDragged(502) // under the drag threshold: still a click
	r.m.MouseDragged(620)
	r.m.MouseUp(x, y)
	if len(r.actions) != 1 || r.actions[0].Kind != "position" || r.actions[0].N != 120 {
		t.Fatalf("actions %+v", r.actions)
	}
	if r.m.OffsetY() != 120 {
		t.Fatalf("offset %v", r.m.OffsetY())
	}
	r.m.MouseDown(x, y, 500, 0)
	r.m.MouseDragged(-5000)
	r.m.MouseUp(x, y)
	if off := r.m.OffsetY(); off >= 0 || off < -(1000/2+CanvasH/2) {
		t.Fatalf("drag not bounded to the screen: %v", off)
	}
}

func TestHiddenRestingPillStillShowsSessions(t *testing.T) {
	r := newRig(t)
	r.m.SetConfig(Config{Enabled: true, Theme: "dark", IdleHandle: false, LivePreview: true})
	if f := r.run(0); f.Visible {
		t.Fatal("resting pill shown with idle_handle off")
	}
	r.m.SessionOpen("dictation")
	r.m.State("listening")
	if f := r.run(frame); !f.Visible || !f.Active {
		t.Fatalf("session hidden: %+v", f)
	}
	r.m.SetConfig(Config{Enabled: false})
	if f := r.run(frame); f.Visible {
		t.Fatal("disabled overlay drew a session")
	}
}
