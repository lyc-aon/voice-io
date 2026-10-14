// Package daemon is the capture state machine for dictation, meeting and
// collaborative sessions.
//
// The microphone feeds VAD and the wake/end keyword spotter. Streaming ASR
// supplies tentative previews; each completed dictation turn is re-decoded
// offline (in capture order, on one worker) and saved as a recoverable
// draft. Closing the session cleans, optionally polishes and pastes once at
// the focused cursor. Meeting and collaborative captures are diarized after
// Stop.
package daemon

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/history"
	"gitlab.com/integral-inference/voice-io/internal/llm"
	"gitlab.com/integral-inference/voice-io/internal/meeting"
	"gitlab.com/integral-inference/voice-io/internal/recovery"
)

// blockSamples is the capture block (100 ms).
const blockSamples = audio.SampleRate / 10

// Engines are the loaded speech models.
type Engines struct {
	VAD       VAD
	KWS       Spotter
	Streaming Streaming
	Refiner   Refiner
	Meeting   *meeting.Processor
	close     func()
}

// Close frees the engines.
func (e *Engines) Close() {
	if e != nil && e.close != nil {
		e.close()
	}
}

// Options wires the daemon's collaborators.
type Options struct {
	Config   *config.Config
	Overlay  Overlay
	Cues     Cues
	Focus    Focus
	Inserter Inserter
	Recovery *recovery.Store
	History  *history.Log
	Log      Logger
	// BuildEngines loads models for a configuration.
	BuildEngines func(cfg *config.Config) (*Engines, error)
	// OpenSource opens the microphone (or a file in headless runs).
	OpenSource func(cfg *config.Config) (audio.Source, error)
	// Persistent sources stay open while idle even without wake listening
	// (files, tests); the microphone otherwise opens only when needed.
	PersistentSource bool
	// RecordPath records the next session's audio to a WAV (diagnostics).
	RecordPath string
	// OnStatus is called after state changes (may be nil).
	OnStatus func()
}

type command struct {
	name string
	cfg  *config.Config
	done chan struct{}
}

// Daemon is safe to control from any goroutine via Send/Reconfigure.
type Daemon struct {
	opts Options
	conf atomic.Pointer[config.Config]
	log  Logger

	ctl     chan command
	pending *config.Config

	// Loop-owned engine and capture state.
	engines       *Engines
	engineErr     error
	source        audio.Source
	sourceErr     error
	nextSourceTry time.Time
	turn          Turn
	streamOpen    bool
	lastPreview   string
	wakeKW        string
	preroll       [][]float32
	noiseFloor    float64
	sessionT0     time.Time
	sessionWarned bool
	smode         string // this session's mode: dictation | meeting | collaborative
	ptt           bool   // started by "ptt"; "ptt_stop" finishes it
	recBuf        [][]float32
	recordDone    bool
	recorder      *meeting.Recorder
	quit          bool

	// Shared with workers; guarded by mu.
	mu             sync.Mutex
	state          State
	enabled        bool
	token          int64
	parts          []string
	draftID        int64
	draftWarned    bool
	dictationCount int
	spillDir       string
	processingPath string
	sessionMode    string
	startedAt      time.Time

	jobs   *queue[func()]
	pastes *queue[*pasteItem]
	wg     sync.WaitGroup
	polish *llm.Client
}

// New creates a daemon; call Run to start capturing.
func New(o Options) *Daemon {
	d := &Daemon{opts: o, log: o.Log, ctl: make(chan command, 64), state: Idle, enabled: true,
		smode: config.ModeDictation, jobs: newQueue[func()](), pastes: newQueue[*pasteItem]()}
	if d.log == nil {
		d.log = nopLogger{}
	}
	d.setConfig(o.Config)
	d.wg.Add(2)
	go d.worker()
	go d.pasteLoop()
	return d
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

func (d *Daemon) cfg() *config.Config { return d.conf.Load() }

func (d *Daemon) setConfig(c *config.Config) {
	d.conf.Store(c)
	d.polish = &llm.Client{Endpoint: c.LLM.Endpoint, Model: c.LLM.Model, APIKeyEnv: c.LLM.APIKeyEnv}
}

// Send queues a control command: push, toggle, cancel, ptt, ptt_stop,
// identify, quit. It never blocks the caller.
func (d *Daemon) Send(name string) {
	select {
	case d.ctl <- command{name: name}:
	default:
		d.log.Printf("control queue full; dropped %s", name)
	}
}

// Reconfigure applies a new configuration as soon as the daemon is idle.
func (d *Daemon) Reconfigure(c *config.Config) {
	d.ctl <- command{name: "reload", cfg: c}
}

func (d *Daemon) getState() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

func (d *Daemon) setState(s State) {
	d.mu.Lock()
	d.state = s
	d.mu.Unlock()
	d.notify()
}

func (d *Daemon) notify() {
	if d.opts.OnStatus != nil {
		d.opts.OnStatus()
	}
}

// Status snapshots the daemon for the control socket and Settings.
func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := Status{State: d.state, Mode: d.cfg().Meeting.Mode, Enabled: d.enabled}
	if d.state != Idle && !d.startedAt.IsZero() {
		s.SessionStart = float64(d.startedAt.UnixMilli()) / 1000
		s.Mode = d.sessionMode
	}
	return s
}

// EngineError reports why models could not be loaded (nil when ready).
func (d *Daemon) EngineError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.engineErr
}

// SourceError reports why the microphone is unavailable (nil when fine).
func (d *Daemon) SourceError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sourceErr
}

// Run captures until ctx ends, a quit command arrives, or a file source is
// exhausted (after its session finishes processing).
func (d *Daemon) Run(ctx context.Context) error {
	defer d.closeSource()
	d.loadEngines()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for !d.quit {
		if ctx.Err() != nil {
			return nil
		}
		if !d.wantSource() {
			d.closeSource()
			select {
			case <-ctx.Done():
				return nil
			case c := <-d.ctl:
				d.handle(c)
			case <-ticker.C:
			}
			continue
		}
		if d.source == nil && !d.openSource() {
			select {
			case <-ctx.Done():
				return nil
			case c := <-d.ctl:
				d.handle(c)
			case <-ticker.C:
			}
			continue
		}
		samples := make([]float32, blockSamples)
		if err := d.source.Read(samples); err != nil {
			if errors.Is(err, io.EOF) {
				d.finishInput()
				return nil
			}
			d.log.Printf("microphone error: %v", err)
			d.closeSource()
			d.mu.Lock()
			d.sourceErr = err
			d.mu.Unlock()
			if d.getState() == Listening {
				d.opts.Overlay.Show("Microphone stopped — finishing", "warn")
				d.finishCapture(false)
			}
			d.notify()
			continue
		}
		d.process(samples)
	}
	return nil
}

// process handles one 100 ms block. Commands are applied first: a reload
// may replace the engines or close the microphone, and a manual start must
// take its pre-roll from the blocks before this one (listen adds this one).
func (d *Daemon) process(samples []float32) {
	d.drainControl()
	e := d.engines
	if d.quit || e == nil || d.source == nil {
		return
	}
	e.VAD.Accept(samples)
	d.preroll = append(d.preroll, samples)
	total := 0
	for _, p := range d.preroll {
		total += len(p)
	}
	for total > int(0.6*audio.SampleRate) && len(d.preroll) > 0 {
		total -= len(d.preroll[0])
		d.preroll = d.preroll[1:]
	}
	cfg := d.cfg()
	switch d.getState() {
	case Idle:
		d.mu.Lock()
		enabled := d.enabled
		d.mu.Unlock()
		if !enabled || !cfg.KWS.Enabled {
			return
		}
		labels, err := e.KWS.Accept(samples)
		if err != nil {
			d.log.Printf("keyword spotter: %v", err)
		}
		for _, kw := range labels {
			if kw == "SEND" {
				d.log.Printf("SEND ignored (idle)")
				continue
			}
			d.log.Printf("WAKE detected")
			d.wake("WAKE", cfg.Meeting.Mode)
			break
		}
	case Listening:
		if d.smode != config.ModeDictation {
			d.listen(samples)
		} else {
			labels, err := e.KWS.Accept(samples)
			if err != nil {
				d.log.Printf("keyword spotter: %v", err)
			}
			switch {
			case contains(labels, "SEND"):
				d.log.Printf("SEND detected")
				d.finishCapture(true)
			case len(labels) > 0:
				d.log.Printf("WAKE ignored (in session)")
			default:
				d.listen(samples)
			}
		}
		d.checkSessionLimit(time.Now())
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// wantSource reports whether the microphone must be open.
func (d *Daemon) wantSource() bool {
	if d.engines == nil {
		return false
	}
	if d.opts.PersistentSource {
		return true
	}
	d.mu.Lock()
	state, enabled := d.state, d.enabled
	d.mu.Unlock()
	if state != Idle {
		return true
	}
	return enabled && d.cfg().KWS.Enabled
}

func (d *Daemon) openSource() bool {
	if time.Now().Before(d.nextSourceTry) {
		return false
	}
	src, err := d.opts.OpenSource(d.cfg())
	d.mu.Lock()
	d.sourceErr = err
	d.mu.Unlock()
	if err != nil {
		d.log.Printf("microphone unavailable: %v", err)
		d.nextSourceTry = time.Now().Add(2 * time.Second)
		if d.getState() == Listening {
			d.opts.Overlay.Result(Result{Text: "Microphone unavailable", Style: "error", AutoClose: 3})
			d.cancelCapture()
		}
		d.notify()
		return false
	}
	d.source = src
	d.notify()
	return true
}

func (d *Daemon) closeSource() {
	if d.source != nil {
		_ = d.source.Close()
		d.source = nil
		d.preroll = nil
		if d.engines != nil {
			d.engines.VAD.Clear()
		}
	}
}

// finishInput ends a file-driven run: an open session finishes like Stop,
// then every queued turn, cleanup and paste completes.
func (d *Daemon) finishInput() {
	if d.getState() == Listening {
		d.finishCapture(false)
	}
	d.jobs.wait()
	d.pastes.wait()
	d.jobs.wait()
}

func (d *Daemon) loadEngines() {
	if d.opts.BuildEngines == nil {
		return
	}
	e, err := d.opts.BuildEngines(d.cfg())
	d.mu.Lock()
	d.engineErr = err
	d.mu.Unlock()
	if err != nil {
		d.log.Printf("speech models unavailable: %v", err)
		d.notify()
		return
	}
	d.engines = e
	d.submit(func() {
		if err := e.Refiner.Warm(); err != nil {
			d.log.Printf("final ASR unavailable: %v", err)
		}
	})
	d.log.Printf("voice-io daemon ready")
	d.opts.Overlay.State("idle")
	d.opts.Overlay.Ready(true)
	d.notify()
}

// SetEngines installs pre-built engines (tests and headless tools).
func (d *Daemon) SetEngines(e *Engines) { d.engines = e }

func (d *Daemon) drainControl() {
	for {
		select {
		case c := <-d.ctl:
			d.handle(c)
		default:
			return
		}
	}
}

func (d *Daemon) handle(c command) {
	defer func() {
		d.applyPending()
		if c.done != nil {
			close(c.done)
		}
	}()
	switch c.name {
	case "reload":
		d.pending = c.cfg
		return
	case "quit":
		d.quit = true
		return
	case "identify":
		if d.getState() == Idle {
			d.mu.Lock()
			enabled := d.enabled
			d.mu.Unlock()
			d.opts.Overlay.Ready(enabled)
		}
		return
	case "toggle":
		d.mu.Lock()
		d.enabled = !d.enabled
		enabled := d.enabled
		state := d.state
		d.mu.Unlock()
		if !enabled && (state == Listening || state == Parsing) {
			d.cancelCapture()
		}
		if enabled {
			d.opts.Overlay.State("idle")
		} else {
			d.opts.Overlay.State("off")
		}
		d.log.Printf("voice-io %s", map[bool]string{true: "on", false: "off"}[enabled])
		d.notify()
		return
	}
	d.mu.Lock()
	enabled := d.enabled
	d.mu.Unlock()
	if !enabled {
		return
	}
	if d.engines == nil {
		switch c.name {
		case "push", "ptt", "dictate", "meeting", "collaborative":
			d.opts.Overlay.Result(Result{Text: "Finish setup in Settings", Style: "warn", AutoClose: 3, ShowText: true})
		}
		return
	}
	state := d.getState()
	switch c.name {
	case "push", "dictate", "meeting", "collaborative":
		// push uses the default mode; the others pick the session's mode.
		// While recording, any of them finishes the session.
		mode := d.cfg().Meeting.Mode
		if c.name != "push" {
			mode = map[string]string{"dictate": config.ModeDictation, "meeting": config.ModeMeeting,
				"collaborative": config.ModeCollaborative}[c.name]
		}
		switch state {
		case Idle:
			if d.ensureSource() {
				d.wake("", mode)
			}
		case Listening:
			d.finishCapture(false)
		}
	case "ptt":
		if state == Idle && d.ensureSource() {
			d.wake("", d.cfg().Meeting.Mode)
			d.ptt = true
		}
	case "ptt_stop":
		if d.ptt && (state == Listening || state == Parsing) {
			d.finishCapture(false)
		}
	case "cancel":
		if state == Listening || state == Parsing {
			d.cancelCapture()
			d.opts.Cues.Play("idle")
			d.log.Printf("voice-io session cancelled")
		}
	}
}

// ensureSource opens the microphone on demand for a manual start.
func (d *Daemon) ensureSource() bool {
	if d.source != nil {
		return true
	}
	d.nextSourceTry = time.Time{}
	if !d.openSource() {
		d.opts.Overlay.Result(Result{Text: "Microphone unavailable", Style: "error", AutoClose: 3})
		d.opts.Cues.Play("error")
		return false
	}
	return true
}

// applyPending installs a queued configuration once idle.
func (d *Daemon) applyPending() {
	if d.pending == nil || d.getState() != Idle {
		return
	}
	next := d.pending
	d.pending = nil
	prev := d.cfg()
	d.setConfig(next)
	if engineConfigChanged(prev, next) || d.engines == nil {
		d.closeSource()
		if d.engines != nil {
			// A cancelled session's worker may still be inside a decoder;
			// let it return before the models are freed.
			d.jobs.wait()
			d.engines.Close()
			d.engines = nil
		}
		d.loadEngines()
	} else if prev.Audio.Device != next.Audio.Device {
		d.closeSource()
	}
	d.log.Printf("configuration applied")
	d.notify()
}

func engineConfigChanged(a, b *config.Config) bool {
	return a.VAD != b.VAD || a.KWS.ModelDir != b.KWS.ModelDir || a.KWS.Chunk != b.KWS.Chunk ||
		a.KWS.Score != b.KWS.Score || a.KWS.Threshold != b.KWS.Threshold ||
		a.KWS.WakePhrase != b.KWS.WakePhrase || a.KWS.EndPhrase != b.KWS.EndPhrase ||
		a.ASR != b.ASR || a.Meeting.SegmentationModel != b.Meeting.SegmentationModel ||
		a.Meeting.EmbeddingModel != b.Meeting.EmbeddingModel
}

// submit queues an ordered background job.
func (d *Daemon) submit(job func()) { d.jobs.put(job) }

func (d *Daemon) worker() {
	defer d.wg.Done()
	for {
		job, ok := d.jobs.get()
		if !ok {
			return
		}
		job()
	}
}

// Close cancels an active capture, finishes accepted work and stops workers.
func (d *Daemon) Close() {
	if d.getState() == Listening {
		d.cancelCapture()
	}
	d.stopRecording()
	d.jobs.wait()
	d.jobs.close()
	d.pastes.wait()
	d.pastes.close()
	d.wg.Wait()
	d.mu.Lock()
	d.cleanupSpill()
	d.mu.Unlock()
	if d.engines != nil {
		d.engines.Close()
	}
}
