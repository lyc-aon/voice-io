package daemon

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/history"
	"gitlab.com/integral-inference/voice-io/internal/insert"
	"gitlab.com/integral-inference/voice-io/internal/meeting"
	"gitlab.com/integral-inference/voice-io/internal/paths"
	"gitlab.com/integral-inference/voice-io/internal/recovery"
)

// ---- fakes ------------------------------------------------------------------

const (
	wakeMark = 0.0123
	sendMark = 0.0456
	idSlot   = 3 // sample carrying each script block's id (id/1000)
)

func loud(b []float32) bool {
	for _, s := range b {
		if s > 0.1 || s < -0.1 {
			return true
		}
	}
	return false
}

// fakeVAD reports speech while blocks are loud and pops one segment (its
// duration) after minSilence quiet blocks.
type fakeVAD struct {
	minSilence int
	speaking   bool
	run        int
	quiet      int
	segs       []float64
}

func (v *fakeVAD) Accept(b []float32) {
	switch {
	case loud(b):
		v.speaking, v.quiet = true, 0
		v.run++
	case v.speaking:
		v.quiet++
		if v.quiet >= v.minSilence {
			v.segs = append(v.segs, float64(v.run)/10)
			v.speaking, v.run, v.quiet = false, 0, 0
		}
	}
}
func (v *fakeVAD) IsSpeech() bool { return v.speaking }
func (v *fakeVAD) PopSegments() []float64 {
	s := v.segs
	v.segs = nil
	return s
}
func (v *fakeVAD) Clear() { *v = fakeVAD{minSilence: v.minSilence} }

// fakeKWS spots marker blocks. Using it after its engines were closed
// fails the test, as a freed native spotter would crash the process.
type fakeKWS struct {
	t      *testing.T
	closed bool
}

func (k *fakeKWS) Accept(b []float32) ([]string, error) {
	if k.closed {
		k.t.Fatal("keyword spotter used after its engines were closed")
	}
	switch b[1] {
	case wakeMark:
		return []string{"WAKE"}, nil
	case sendMark:
		return []string{"SEND"}, nil
	}
	return nil, nil
}

type fakeStreaming struct{ final string }

func (s *fakeStreaming) NewStream() {}
func (s *fakeStreaming) Accept(b []float32) string {
	if loud(b) {
		return "SOME WORDS"
	}
	return ""
}
func (s *fakeStreaming) Finalize(float64) string { return s.final }

// fakeRefiner returns scripted transcripts in call order and records which
// script blocks each turn contained; gate (when set) holds each call until
// closed.
type fakeRefiner struct {
	mu    sync.Mutex
	texts []string
	calls int
	turns [][]int
	gate  chan struct{}
}

func (r *fakeRefiner) Transcribe(samples []float32) (string, error) {
	if r.gate != nil {
		<-r.gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	var ids []int
	for at := 0; at+idSlot < len(samples); at += blockSamples {
		ids = append(ids, int(math.Round(float64(samples[at+idSlot])*1000)))
	}
	r.turns = append(r.turns, ids)
	if len(r.texts) == 0 {
		return "", nil
	}
	t := r.texts[0]
	r.texts = r.texts[1:]
	return t, nil
}
func (r *fakeRefiner) Warm() error { return nil }
func (r *fakeRefiner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type fakeOverlay struct {
	mu     sync.Mutex
	events []string
}

func (o *fakeOverlay) add(format string, args ...any) {
	o.mu.Lock()
	o.events = append(o.events, fmt.Sprintf(format, args...))
	o.mu.Unlock()
}
func (o *fakeOverlay) State(s string)          { o.add("state:%s", s) }
func (o *fakeOverlay) SessionOpen(mode string) { o.add("open:%s", mode) }
func (o *fakeOverlay) SessionClose()           { o.add("close") }
func (o *fakeOverlay) Show(text, style string) { o.add("show:%s:%s", style, text) }
func (o *fakeOverlay) Processing(label string) { o.add("processing:%s", label) }
func (o *fakeOverlay) Preview(text string)     { o.add("preview:%s", text) }
func (o *fakeOverlay) Level(float64)           {}
func (o *fakeOverlay) Result(r Result) {
	if r.Permission != "" {
		o.add("result:%s:%s [allow %s]", r.Style, r.Text, r.Permission)
		return
	}
	o.add("result:%s:%s", r.Style, r.Text)
}
func (o *fakeOverlay) Ready(bool) {}
func (o *fakeOverlay) Clear()     { o.add("clear") }
func (o *fakeOverlay) has(event string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Contains(o.events, event)
}
func (o *fakeOverlay) count(event string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, e := range o.events {
		if e == event {
			n++
		}
	}
	return n
}
func (o *fakeOverlay) last() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.events) - 1; i >= 0; i-- {
		if strings.HasPrefix(o.events[i], "state:") {
			return o.events[i]
		}
	}
	return ""
}

type paste struct {
	text   string
	submit insert.Submit
	copy   bool
}

type fakeInserter struct {
	mu     sync.Mutex
	pastes []paste
	fail   string
}

func (f *fakeInserter) Dictate(text string, r insert.Request) insert.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pastes = append(f.pastes, paste{text: text, submit: r.Submit})
	if f.fail != "" {
		return insert.Result{Message: f.fail}
	}
	return insert.Result{OK: true, Executed: true, Message: "inserted"}
}
func (f *fakeInserter) CopyOnly(text string) insert.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pastes = append(f.pastes, paste{text: text, copy: true})
	return insert.Result{OK: true, Message: "copied"}
}
func (f *fakeInserter) all() []paste {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pastes)
}

type fakeFocus struct {
	focus   string
	blocked string // missing permission id
}

func (f fakeFocus) Probe() string       { return f.focus }
func (f fakeFocus) Chord(string) string { return "ctrl+v" }
func (f fakeFocus) PasteBlocked() (string, string) {
	if f.blocked == "" {
		return "", ""
	}
	return f.blocked, "allow Accessibility to paste"
}
func (f fakeFocus) PasteHint() string { return "⌘V" }

type fakeCues struct {
	mu    sync.Mutex
	names []string
}

func (c *fakeCues) Play(name string) {
	c.mu.Lock()
	c.names = append(c.names, name)
	c.mu.Unlock()
}

// script is a microphone that plays blocks and runs hooks between them on
// the capture goroutine.
type step struct {
	block []float32
	hook  func()
}

type script struct {
	steps  []step
	i      int
	closes int
}

func (s *script) Read(dst []float32) error {
	for s.i < len(s.steps) && s.steps[s.i].hook != nil {
		h := s.steps[s.i].hook
		s.i++
		h()
	}
	if s.i >= len(s.steps) {
		return io.EOF
	}
	copy(dst, s.steps[s.i].block)
	s.i++
	return nil
}
func (s *script) Close() error { s.closes++; return nil }

func blocks(n int, fill func([]float32)) []step {
	out := make([]step, n)
	for i := range out {
		b := make([]float32, blockSamples)
		if fill != nil {
			fill(b)
		}
		out[i].block = b
	}
	return out
}

func silence(n int) []step { return blocks(n, nil) }

func talk(n int) []step {
	return blocks(n, func(b []float32) {
		for i := range b {
			b[i] = 0.3 * float32(1-2*(i%2))
		}
	})
}

func marker(v float32) step {
	return blocks(1, func(b []float32) { b[1] = v })[0]
}

func hook(f func()) step { return step{hook: f} }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- harness ----------------------------------------------------------------

type harness struct {
	t      *testing.T
	cfg    *config.Config
	ov     *fakeOverlay
	ins    *fakeInserter
	cues   *fakeCues
	rec    *recovery.Store
	hist   *history.Log
	asr    *fakeStreaming
	ref    *fakeRefiner
	vad    *fakeVAD
	meet   *meeting.Processor
	focus  fakeFocus
	builds int
	d      *Daemon
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("VOICE_IO_CONFIG", "")
	cfg := config.Default()
	cfg.KWS.Enabled = false
	cfg.Polish.Enabled = false
	if mutate != nil {
		mutate(cfg)
	}
	rec, err := recovery.Open(filepath.Join(home, "recovery"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, cfg: cfg, ov: &fakeOverlay{}, ins: &fakeInserter{}, cues: &fakeCues{}, rec: rec,
		hist: &history.Log{Path: cfg.HistoryPath()}, asr: &fakeStreaming{final: "SOME WORDS"},
		ref: &fakeRefiner{}, vad: &fakeVAD{minSilence: 3}, meet: &meeting.Processor{},
		focus: fakeFocus{focus: "editor|1"}}
}

// run plays steps through a fresh daemon until the script ends and every
// queued transcription and paste has finished.
func (h *harness) run(steps ...[]step) {
	h.t.Helper()
	src := &script{}
	id := 0
	for _, s := range steps {
		for _, st := range s {
			if st.block != nil {
				st.block[idSlot] = float32(id) / 1000
				id++
			}
			src.steps = append(src.steps, st)
		}
	}
	h.d = New(Options{Config: h.cfg, Overlay: h.ov, Cues: h.cues, Focus: h.focus, Inserter: h.ins,
		Recovery: h.rec, History: h.hist, PersistentSource: true,
		BuildEngines: func(*config.Config) (*Engines, error) {
			h.builds++
			kws := &fakeKWS{t: h.t}
			return &Engines{VAD: h.vad, KWS: kws, Streaming: h.asr, Refiner: h.ref, Meeting: h.meet,
				close: func() { kws.closed = true }}, nil
		},
		OpenSource: func(*config.Config) (audio.Source, error) { return src, nil },
	})
	if err := h.d.Run(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.d.Close()
}

func (h *harness) send(cmd string) step { return hook(func() { h.d.Send(cmd) }) }

func (h *harness) recovery() []recovery.Entry { return h.rec.List() }

func (h *harness) historyTexts() []string {
	entries, err := h.hist.List(100)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Text)
	}
	return out
}

// ---- dictation ----------------------------------------------------------------

func TestWakePhraseTurnsAndSpokenEndPasteOnceAndPressEnter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.KWS.Enabled = true })
	h.ref.texts = []string{"Hey voice, ship the report.", "And email the team. Voice send."}
	h.run(silence(3), []step{marker(wakeMark)}, talk(8), silence(4), talk(6), []step{marker(sendMark)}, silence(3))

	got := h.ins.all()
	if len(got) != 1 || got[0].text != "ship the report. And email the team" || got[0].submit != insert.SubmitEnter {
		t.Fatalf("pastes %+v", got)
	}
	if h.ref.count() != 2 {
		t.Errorf("refiner calls %d", h.ref.count())
	}
	if hist := h.historyTexts(); len(hist) != 1 || hist[0] != got[0].text {
		t.Errorf("history %q", hist)
	}
	if left := h.recovery(); len(left) != 0 {
		t.Errorf("recovery after a verified paste: %+v", left)
	}
	if !h.ov.has("open:dictation") || !h.ov.has("result:ok:Inserted") || h.ov.last() != "state:idle" {
		t.Errorf("overlay %q", h.ov.events)
	}
	if h.ov.has("preview:SOME WORDS") {
		t.Error("preview shown in capitals")
	}
}

func TestManualStopPastesOnceWithoutEnterAfterCleanup(t *testing.T) {
	h := newHarness(t, nil)
	if err := os.MkdirAll(paths.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfg.VocabularyPath(), []byte(`{"hextee": "HexT"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ref.texts = []string{"um hextee first.", "uh second."}
	h.run(silence(3), []step{h.send("push")}, talk(8), silence(4), talk(8), silence(4), []step{h.send("push")}, silence(2))

	got := h.ins.all()
	if len(got) != 1 || got[0].text != "HexT first. second." || got[0].submit != insert.SubmitAuto {
		t.Fatalf("pastes %+v", got)
	}
	if hist := h.historyTexts(); len(hist) != 1 || hist[0] != "HexT first. second." {
		t.Errorf("history %q", hist)
	}
}

func TestManualStartKeepsPrerollWithoutDuplicatingAudio(t *testing.T) {
	h := newHarness(t, nil)
	h.ref.texts = []string{"words"}
	h.run(silence(5), []step{h.send("push")}, talk(8), silence(4), []step{h.send("push")}, silence(2))

	// Blocks 0-4 precede the press: the last 0.4 s of them is kept so a
	// word started with the press is not clipped, then every block once.
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	if len(h.ref.turns) != 1 || !slices.Equal(h.ref.turns[0], want) {
		t.Fatalf("turn blocks %v; want %v", h.ref.turns, want)
	}
}

func TestEngineReloadNeverUsesFreedModels(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.KWS.Enabled = true })
	next := h.cfg.Clone()
	next.KWS.WakePhrase = "hey computer"
	h.run(silence(3), []step{hook(func() { h.d.Reconfigure(next) })}, silence(5))

	if h.builds != 2 || h.d.cfg().KWS.WakePhrase != "hey computer" {
		t.Fatalf("builds %d, wake phrase %q", h.builds, h.d.cfg().KWS.WakePhrase)
	}
}

func TestShortTurnWithoutWordsIsDiscarded(t *testing.T) {
	h := newHarness(t, nil)
	h.vad.minSilence = 2
	h.asr.final = "" // breath or a click: the live decoder heard nothing
	h.run(silence(3), []step{h.send("push")}, talk(5), silence(3), []step{h.send("push")}, silence(2))

	if h.ref.count() != 0 {
		t.Errorf("offline decoder ran %d times on a wordless turn", h.ref.count())
	}
	if got := h.ins.all(); len(got) != 0 {
		t.Fatalf("pasted %+v", got)
	}
	if !h.ov.has("clear") || h.ov.last() != "state:idle" {
		t.Errorf("overlay %q", h.ov.events)
	}
}

func TestCancelDiscardsTheDraftButKeepsEarlierRecovery(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.rec.Spool("failed", "Previous text I still need.", "dictation", ""); err != nil {
		t.Fatal(err)
	}
	h.ref.texts = []string{"Draft release notes."}
	draftSaved := func() bool {
		for _, e := range h.rec.List() {
			if e.Kind == "draft" && e.Text == "Draft release notes." {
				return true
			}
		}
		return false
	}
	h.run(silence(3), []step{h.send("push")}, talk(8), silence(4),
		[]step{hook(func() { waitFor(t, "draft", draftSaved) }), h.send("cancel")}, silence(3))

	if got := h.ins.all(); len(got) != 0 {
		t.Fatalf("cancelled session pasted %+v", got)
	}
	left := h.recovery()
	if len(left) != 1 || left[0].Text != "Previous text I still need." {
		t.Fatalf("recovery after cancel %+v", left)
	}
	if !slices.Contains(h.cues.names, "idle") || h.ov.last() != "state:idle" {
		t.Errorf("cues %q overlay %q", h.cues.names, h.ov.events)
	}
}

func TestStaleRefinementAfterCancelCannotRestoreTheDraft(t *testing.T) {
	h := newHarness(t, nil)
	h.ref.texts = []string{"late words"}
	h.ref.gate = make(chan struct{})
	h.run(silence(3), []step{h.send("push")}, talk(8), silence(4), []step{h.send("cancel")}, silence(1),
		[]step{hook(func() { close(h.ref.gate) })}, silence(2))

	if h.ref.count() > 1 {
		t.Fatalf("refiner calls %d", h.ref.count())
	}
	if left := h.recovery(); len(left) != 0 {
		t.Fatalf("stale refinement saved %+v", left)
	}
	if got := h.ins.all(); len(got) != 0 {
		t.Fatalf("stale refinement pasted %+v", got)
	}
}

func TestUnidentifiedFocusStillPastesButNeverSubmits(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.KWS.Enabled = true })
	h.focus = fakeFocus{focus: ""}
	h.ref.texts = []string{"Ship it. Voice send."}
	h.run(silence(3), []step{h.send("push")}, talk(8), []step{marker(sendMark)}, silence(3))

	got := h.ins.all()
	if len(got) != 1 || got[0].copy || got[0].text != "Ship it" || got[0].submit != insert.SubmitNever {
		t.Fatalf("pastes %+v", got)
	}
	if left := h.recovery(); len(left) != 1 || left[0].Kind != "commit" {
		t.Fatalf("unverified paste dropped its recovery copy: %+v", left)
	}
	if !h.ov.has("result:ok:Paste sent") {
		t.Errorf("overlay %q", h.ov.events)
	}
	if hist := h.historyTexts(); len(hist) != 1 {
		t.Errorf("history %q", hist)
	}
}

func TestMissingPermissionCopiesAndOffersToAllow(t *testing.T) {
	h := newHarness(t, nil)
	h.focus = fakeFocus{focus: "editor|1", blocked: "accessibility"}
	h.ref.texts = []string{"Final cleaned draft."}
	h.run(silence(3), []step{h.send("push")}, talk(8), silence(4), []step{h.send("push")}, silence(2))

	got := h.ins.all()
	if len(got) != 1 || !got[0].copy || got[0].text != "Final cleaned draft." {
		t.Fatalf("pastes %+v", got)
	}
	if !h.ov.has("result:warn:Copied — allow Accessibility to paste [allow accessibility]") {
		t.Errorf("overlay %q", h.ov.events)
	}
	left := h.recovery()
	if len(left) != 1 || left[0].Kind != "failed" || left[0].Text != "Final cleaned draft." {
		t.Fatalf("recovery %+v", left)
	}
	if hist := h.historyTexts(); len(hist) != 0 {
		t.Errorf("copy counted as inserted: %q", hist)
	}
}

func TestCopyOnlySettingLeavesTextOnTheClipboard(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Exec.AutoPaste = false })
	h.ref.texts = []string{"Final cleaned draft."}
	h.run(silence(3), []step{h.send("push")}, talk(8), silence(4), []step{h.send("push")}, silence(2))

	got := h.ins.all()
	if len(got) != 1 || !got[0].copy {
		t.Fatalf("pastes %+v", got)
	}
	if !h.ov.has("result:ok:Copied — paste with ⌘V") {
		t.Errorf("overlay %q", h.ov.events)
	}
	if hist := h.historyTexts(); len(hist) != 1 || hist[0] != "Final cleaned draft." {
		t.Errorf("history %q", hist)
	}
	if left := h.recovery(); len(left) != 1 || left[0].Kind != "commit" {
		t.Fatalf("copied text lost its recovery copy: %+v", left)
	}
	if slices.Contains(h.cues.names, "error") {
		t.Errorf("cues %q", h.cues.names)
	}
}

func TestFailedPasteIsSavedAndNotCountedAsInserted(t *testing.T) {
	h := newHarness(t, nil)
	h.ins.fail = "key sender unavailable"
	h.ref.texts = []string{"Final cleaned draft."}
	h.run(silence(3), []step{h.send("push")}, talk(8), silence(4), []step{h.send("push")}, silence(2))

	left := h.recovery()
	if len(left) != 1 || left[0].Kind != "failed" || left[0].Text != "Final cleaned draft." {
		t.Fatalf("recovery %+v", left)
	}
	if hist := h.historyTexts(); len(hist) != 0 {
		t.Errorf("history %q", hist)
	}
	if !slices.Contains(h.cues.names, "error") {
		t.Errorf("cues %q", h.cues.names)
	}
}

func TestSessionCeilingWarnsOnceThenFinishesWithOnePaste(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Dictation.SessionMaxS = 100 })
	h.ref.texts = []string{"captured words"}
	at := func(s float64) step {
		return hook(func() { h.d.checkSessionLimit(h.d.sessionT0.Add(time.Duration(s * float64(time.Second)))) })
	}
	h.run(silence(3), []step{h.send("push")}, talk(8), []step{at(91), at(95)}, silence(1), []step{at(100)}, silence(3))

	if n := h.ov.count("show:warn:Session ending soon — press Stop to finish"); n != 1 {
		t.Errorf("%d warnings", n)
	}
	if !h.ov.has("show:warn:Session limit reached — finishing dictation") {
		t.Errorf("overlay %q", h.ov.events)
	}
	if got := h.ins.all(); len(got) != 1 || got[0].text != "captured words" {
		t.Fatalf("pastes %+v", got)
	}
	spills, _ := filepath.Glob(filepath.Join(h.rec.Dir, "voice-io-pcm-*"))
	if len(spills) != 0 {
		t.Errorf("spill directories left: %v", spills)
	}
}

func TestToggleOffCancelsAndIgnoresShortcutsUntilOn(t *testing.T) {
	h := newHarness(t, nil)
	h.ref.texts = []string{"never pasted"}
	h.run(silence(3), []step{h.send("push")}, talk(4), []step{h.send("toggle")}, silence(2),
		[]step{h.send("push")}, talk(8), silence(4), []step{h.send("toggle")}, silence(2))

	if got := h.ins.all(); len(got) != 0 {
		t.Fatalf("pasted while off: %+v", got)
	}
	if !h.ov.has("state:off") || h.ov.last() != "state:idle" || h.ov.count("open:dictation") != 1 {
		t.Errorf("overlay %q", h.ov.events)
	}
}

func TestConfigurationWaitsForTheSessionToEnd(t *testing.T) {
	h := newHarness(t, nil)
	h.ref.texts = []string{"one"}
	next := h.cfg.Clone()
	next.UI.LivePreview = false
	var during, after bool
	h.run(silence(3), []step{h.send("push")}, talk(8),
		[]step{hook(func() { h.d.Reconfigure(next) })}, silence(2),
		[]step{hook(func() { during = h.d.cfg() == next })}, silence(2), []step{h.send("push")}, silence(2),
		[]step{hook(func() { waitFor(t, "idle", func() bool { return h.d.getState() == Idle }) })}, silence(2),
		[]step{hook(func() { after = h.d.cfg() == next })}, silence(1))

	if during || !after {
		t.Fatalf("applied during session=%v, after=%v", during, after)
	}
}

// ---- meetings -----------------------------------------------------------------

func TestMeetingStopWritesNotesWithoutPasting(t *testing.T) {
	h := newHarness(t, nil)
	h.meet.DiarizeFunc = func([]float32, func() bool) ([]meeting.SpeechLine, error) {
		return []meeting.SpeechLine{{Start: 0, End: 1, Speaker: 0, Text: "Team decided to review the design."}}, nil
	}
	h.run(silence(3), []step{h.send("meeting")}, talk(10), silence(10), []step{h.send("push")}, silence(2))

	if got := h.ins.all(); len(got) != 0 {
		t.Fatalf("meeting pasted %+v", got)
	}
	notes, _ := filepath.Glob(filepath.Join(paths.MeetingsDir(), "*.md"))
	wavs, _ := filepath.Glob(filepath.Join(paths.MeetingsDir(), "*.wav"))
	if len(notes) != 1 || len(wavs) != 0 {
		t.Fatalf("notes %v wavs %v", notes, wavs)
	}
	doc, _ := os.ReadFile(notes[0])
	if !strings.Contains(string(doc), "Speaker 1: Team decided to review the design.") {
		t.Fatalf("notes:\n%s", doc)
	}
	if !h.ov.has("open:meeting") || !h.ov.has("result:ok:Meeting notes saved") || h.ov.last() != "state:idle" {
		t.Errorf("overlay %q", h.ov.events)
	}
}

func TestMeetingNeedsSpeakerModels(t *testing.T) {
	h := newHarness(t, nil)
	h.meet.Segmentation = filepath.Join(t.TempDir(), "missing.onnx")
	h.run(silence(3), []step{h.send("meeting")}, talk(5), silence(3))

	if !h.ov.has("result:error:Download the speaker models in Settings") || h.ov.has("open:meeting") {
		t.Fatalf("overlay %q", h.ov.events)
	}
}

func TestCollaborativeStopPastesLabelledDraftWithoutEnter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.KWS.Enabled = true })
	h.meet.DiarizeFunc = func([]float32, func() bool) ([]meeting.SpeechLine, error) {
		return []meeting.SpeechLine{{Start: 0, End: 1, Speaker: 4, Text: "let's design the tool."},
			{Start: 1.5, End: 2.5, Speaker: 1, Text: "I will test it."}}, nil
	}
	h.run(silence(3), []step{h.send("collaborative")}, talk(10), []step{marker(sendMark)}, silence(5),
		[]step{h.send("push")}, silence(2))

	got := h.ins.all()
	want := "Speaker 1: let's design the tool.\nSpeaker 2: I will test it."
	if len(got) != 1 || got[0].text != want || got[0].submit != insert.SubmitNever {
		t.Fatalf("pastes %+v", got)
	}
	if left := h.recovery(); len(left) != 0 {
		t.Errorf("recovery %+v", left)
	}
	if wavs, _ := filepath.Glob(filepath.Join(paths.MeetingsDir(), "*.wav")); len(wavs) != 0 {
		t.Errorf("recording kept: %v", wavs)
	}
}

func TestCollaborativePasteFailureKeepsTheEditableDraft(t *testing.T) {
	h := newHarness(t, nil)
	h.ins.fail = "paste blocked"
	h.meet.DiarizeFunc = func([]float32, func() bool) ([]meeting.SpeechLine, error) {
		return []meeting.SpeechLine{{Start: 0, End: 1, Speaker: 0, Text: "let's design the tool."},
			{Start: 1.5, End: 2.5, Speaker: 1, Text: "I will test it."}}, nil
	}
	h.run(silence(3), []step{h.send("collaborative")}, talk(10), silence(3), []step{h.send("push")}, silence(2))

	left := h.recovery()
	if len(left) != 1 || left[0].Kind != "failed" || left[0].Text != "Speaker 1: let's design the tool.\nSpeaker 2: I will test it." {
		t.Fatalf("recovery %+v", left)
	}
	if wavs, _ := filepath.Glob(filepath.Join(paths.MeetingsDir(), "*.wav")); len(wavs) != 0 {
		t.Errorf("recording kept after the draft was saved: %v", wavs)
	}
}

func TestCollaborativeDiarizerFailureKeepsPrivateAudio(t *testing.T) {
	h := newHarness(t, nil)
	h.meet.DiarizeFunc = func([]float32, func() bool) ([]meeting.SpeechLine, error) {
		return nil, fmt.Errorf("decode failed")
	}
	h.run(silence(3), []step{h.send("collaborative")}, talk(10), silence(3), []step{h.send("push")}, silence(2))

	if wavs, _ := filepath.Glob(filepath.Join(paths.MeetingsDir(), "*.wav")); len(wavs) != 1 {
		t.Fatalf("recording not kept: %v", wavs)
	}
	if got := h.ins.all(); len(got) != 0 || h.ov.last() != "state:idle" {
		t.Fatalf("pastes %+v overlay %q", got, h.ov.events)
	}
}

func TestCollaborativeCancelDiscardsRecording(t *testing.T) {
	h := newHarness(t, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	h.meet.DiarizeFunc = func(_ []float32, cancelled func() bool) ([]meeting.SpeechLine, error) {
		close(started)
		<-release
		if cancelled() {
			return nil, meeting.ErrCancelled
		}
		return []meeting.SpeechLine{{Start: 0, End: 1, Speaker: 0, Text: "too late"}}, nil
	}
	h.run(silence(3), []step{h.send("collaborative")}, talk(10), silence(3), []step{h.send("push")}, silence(1),
		[]step{hook(func() { <-started }), h.send("cancel")}, silence(1), []step{hook(func() { close(release) })}, silence(2))

	if got := h.ins.all(); len(got) != 0 {
		t.Fatalf("cancelled draft pasted %+v", got)
	}
	if wavs, _ := filepath.Glob(filepath.Join(paths.MeetingsDir(), "*.wav")); len(wavs) != 0 {
		t.Fatalf("cancelled recording kept: %v", wavs)
	}
}
