package daemon

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/meeting"
)

// shortTurn bounds turns that need recognized words to count (seconds).
const shortTurn = 1.2

// previewTail shows the latest tentative words in readable case. The
// streaming decoder emits capitals; the final text is properly cased.
func previewTail(text string) string {
	words := strings.Fields(text)
	if len(words) > 6 {
		words = words[len(words)-6:]
	}
	tail := strings.Join(words, " ")
	if tail == strings.ToUpper(tail) {
		tail = strings.ToLower(tail)
	}
	return tail
}

// wake opens a session in mode. keyword is "WAKE" for a spoken trigger.
func (d *Daemon) wake(keyword, mode string) {
	if mode != config.ModeDictation {
		if err := d.engines.Meeting.CheckModels(); err != nil {
			d.log.Printf("%s unavailable (%v)", mode, err)
			d.opts.Overlay.Result(Result{Text: "Download the speaker models in Settings", Style: "error", AutoClose: 3.5, ShowText: true})
			return
		}
		rec, err := meeting.NewRecorder("")
		if err != nil {
			d.log.Printf("%s storage unavailable (%v)", mode, err)
			d.opts.Overlay.Result(Result{Text: "Audio storage unavailable", Style: "error", AutoClose: 3, ShowText: true})
			return
		}
		d.recorder = rec
	}
	d.mu.Lock()
	d.token++
	d.parts = nil
	d.draftID = 0
	d.draftWarned = false
	d.cleanupSpill()
	d.dictationCount = 0
	d.sessionMode = mode
	d.startedAt = time.Now()
	d.mu.Unlock()
	d.wakeKW = keyword
	// The trigger's own audio is a VAD segment; discard it so it cannot
	// close the session immediately.
	d.engines.VAD.PopSegments()
	d.opts.Overlay.SessionOpen(mode)
	d.opts.Overlay.State("listening")
	switch mode {
	case config.ModeMeeting:
		d.opts.Overlay.Show("Meeting recording", "info")
	case config.ModeCollaborative:
		d.opts.Overlay.Show("Collaborative recording", "info")
	}
	d.opts.Cues.Play("listening")
	d.smode, d.ptt = mode, false
	// A spoken trigger opens the turn empty so wake audio is never
	// transcribed. A key or click keeps a moment of pre-trigger audio so
	// speech that began with the press is not clipped. Either way the
	// stream opens at the first confirmed speech.
	d.turn.Open()
	if keyword == "" {
		for _, p := range d.preroll {
			d.turn.Add(p)
		}
		d.turn.RetainLast(0.4)
	}
	d.streamOpen = false
	d.lastPreview = ""
	d.sessionT0 = time.Now()
	d.sessionWarned = false
	d.noiseFloor = roomFloor(d.preroll)
	if d.opts.RecordPath != "" && !d.recordDone && d.recBuf == nil {
		d.recBuf = append([][]float32{}, d.preroll...)
	}
	d.setState(Listening)
}

// meterSamples is the audio behind each waveform level (20 ms).
const meterSamples = audio.SampleRate / 50

// roomFloor estimates the background level from the pre-roll: the quietest
// block, which is room noise even if speech began before the trigger.
func roomFloor(blocks [][]float32) float64 {
	floor := 0.0
	for i, b := range blocks {
		if r := audio.RMS(b); i == 0 || r < floor {
			floor = r
		}
	}
	return floor
}

// listen consumes one block of an open session.
func (d *Daemon) listen(samples []float32) {
	cfg := d.cfg()
	e := d.engines
	if d.smode != config.ModeDictation && d.recorder != nil {
		if err := d.recorder.Append(samples); err != nil {
			d.log.Printf("meeting recording interrupted (%v)", err)
			d.opts.Overlay.Show("Audio storage failed — saving captured portion", "warn")
			d.finishSession(nil, false)
			return
		}
	}
	if d.recBuf != nil {
		d.recBuf = append(d.recBuf, samples)
	}
	speech := e.VAD.IsSpeech()
	if !speech && !d.streamOpen {
		// Track the room: drop quickly to a quieter floor, rise slowly.
		rms := audio.RMS(samples)
		k := 0.05
		if rms < d.noiseFloor {
			k = 0.5
		}
		d.noiseFloor += k * (rms - d.noiseFloor)
	}
	// The waveform shows each 20 ms above the room's noise floor without
	// waiting for the VAD, so quiet syllables move it as they are spoken.
	for k := 0; k < len(samples); k += meterSamples {
		part := samples[k:min(k+meterSamples, len(samples))]
		d.opts.Overlay.Level(math.Max(0, audio.RMS(part)-1.5*d.noiseFloor))
	}
	d.turn.Add(samples)
	if !d.streamOpen && !speech {
		d.turn.RetainLast(0.6)
	}
	partial := ""
	if !d.streamOpen {
		if !(d.turn.IsOpen() && speech) {
			if len(e.VAD.PopSegments()) > 0 {
				d.keepListening() // blip: discard its buffered audio
			}
			return
		}
		// VAD-gated open: the buffered onset seeds the stream once; only a
		// wake-triggered turn can contain wake-tail overlap to drop.
		e.Streaming.NewStream()
		d.streamOpen = true
		seed := d.turn.Audio()
		drop := 0
		if d.wakeKW != "" {
			drop = int(cfg.Turn.FrontDrop * audio.SampleRate)
		}
		if len(seed) > drop {
			partial = e.Streaming.Accept(seed[drop:])
		}
	} else {
		partial = e.Streaming.Accept(samples)
	}
	if partial != "" && cfg.UI.LivePreview && partial != d.lastPreview {
		d.lastPreview = partial
		d.opts.Overlay.Preview(previewTail(partial))
	}
	// Turn authority: a VAD endpoint (min_silence_duration) or the per-turn
	// cap commits the turn. Neither closes the session.
	segs := e.VAD.PopSegments()
	long := false
	for _, s := range segs {
		long = long || s >= 0.5
	}
	switch {
	case long:
		d.log.Printf("    segpop dur=%.2fs", segs[len(segs)-1])
		d.commitTurn(false, false)
	case len(segs) > 0:
		d.keepListening() // blip: do not seed the next turn with it
	case d.turn.IsOpen() && d.turn.Seconds() >= cfg.Turn.MaxSec:
		d.log.Printf("    turn cap %.1fs", d.turn.Seconds())
		d.commitTurn(false, false)
	}
}

// keepListening starts a fresh turn within the open session.
func (d *Daemon) keepListening() {
	d.turn.Open()
	d.engines.VAD.Clear()
	d.streamOpen = false
	d.setState(Listening)
	d.opts.Overlay.State("listening")
	d.preroll = nil
	d.lastPreview = ""
	d.wakeKW = ""
}

// commitTurn ends a VAD-ended turn, or the whole session when closeSession.
func (d *Daemon) commitTurn(closeSession, submit bool) {
	if !d.turn.IsOpen() || !d.streamOpen {
		// No stream means VAD never confirmed speech in this turn;
		// finalizing the previous stream would paste its text twice.
		if closeSession {
			d.afterTurn(true, nil, submit)
		} else if d.turn.IsOpen() {
			d.keepListening()
		}
		return
	}
	turnAudio := d.turn.Audio()
	d.turn.Close()
	d.streamOpen = false
	d.setState(Parsing)
	if closeSession {
		d.opts.Overlay.State("processing")
	}
	if float64(len(turnAudio))/audio.SampleRate < 0.5 {
		if closeSession {
			d.afterTurn(true, nil, submit)
		} else {
			d.keepListening() // blip
		}
		return
	}
	cfg := d.cfg()
	if d.smode != config.ModeDictation {
		d.wakeKW = ""
		d.afterTurn(closeSession, nil, false)
		return
	}
	// The streaming final is only a fallback for a failed offline re-decode.
	final := d.engines.Streaming.Finalize(cfg.Turn.TailS)
	kw := d.wakeKW
	d.wakeKW = ""
	final = strings.TrimSpace(d.stripPhrases(final, kw == "WAKE", closeSession, submit))
	seconds := float64(len(turnAudio)) / audio.SampleRate
	d.log.Printf("turn capture=%.1fs close=%v final=%q", seconds, closeSession, final)
	// A short turn in which the streaming decoder heard no words is breath,
	// a click or the wake phrase's tail; the offline model would only
	// hallucinate filler ("Mm-hmm.") from it.
	if final == "" && seconds < shortTurn {
		d.log.Printf("    discarded %.1fs turn without words", seconds)
		if closeSession {
			d.afterTurn(true, nil, submit)
		} else {
			d.keepListening()
		}
		return
	}
	d.mu.Lock()
	d.dictationCount++
	token := d.token
	d.mu.Unlock()
	// Queued PCM goes to private disk so a slow re-decode cannot grow RAM.
	spill, spillFailed := d.spillTurn(turnAudio)
	stripWake := kw == "WAKE"
	if spill != "" {
		d.submit(func() { d.dictationWorker(spill, nil, final, closeSession, submit, token, stripWake) })
	} else {
		d.submit(func() { d.dictationWorker("", turnAudio, final, closeSession, submit, token, stripWake) })
	}
	if spillFailed {
		d.opts.Overlay.Show("Audio storage unavailable — finishing dictation", "warn")
	}
	d.afterTurn(closeSession || spillFailed, turnAudio, submit)
}

func (d *Daemon) stripPhrases(text string, wake, closing, spokenEnd bool) string {
	cfg := d.cfg()
	if wake {
		text = stripLeading(text, cfg.KWS.WakePhrase)
	}
	if closing {
		text = stripEnd(text, cfg.KWS.EndPhrase, spokenEnd)
	}
	return text
}

// spillTurn writes one turn's float32 PCM to the session's private spill
// directory, returning "" and true when storage fails (the audio then stays
// in memory and the session finishes).
func (d *Daemon) spillTurn(samples []float32) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.spillDir == "" {
		dir, err := d.opts.Recovery.SpillDir()
		if err != nil {
			d.log.Printf("    audio spill failed (%v); finishing session", err)
			return "", true
		}
		d.spillDir = dir
	}
	path := filepath.Join(d.spillDir, fmt.Sprintf("%08d.f32", d.dictationCount))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		buf := make([]byte, 4*len(samples))
		for i, s := range samples {
			binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(s))
		}
		_, err = f.Write(buf)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		d.log.Printf("    audio spill failed (%v); finishing session", err)
		_ = os.Remove(path)
		return "", true
	}
	return path, false
}

func readSpill(path string) ([]float32, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make([]float32, len(data)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return out, nil
}

// cleanupSpill removes queued turn audio. Caller holds d.mu.
func (d *Daemon) cleanupSpill() {
	if d.spillDir != "" {
		_ = os.RemoveAll(d.spillDir)
		d.spillDir = ""
	}
}

func (d *Daemon) afterTurn(closeSession bool, turnAudio []float32, submit bool) {
	if closeSession {
		d.finishSession(turnAudio, submit)
	} else {
		d.keepListening()
	}
}

// finishCapture stops the active session (Stop / spoken end phrase).
func (d *Daemon) finishCapture(submit bool) {
	if d.smode != config.ModeDictation {
		d.finishSession(nil, false)
		return
	}
	d.commitTurn(true, submit)
}

func (d *Daemon) finishSession(turnAudio []float32, submit bool) {
	if d.smode != config.ModeDictation {
		d.finishMeetingSession()
		return
	}
	d.mu.Lock()
	count := d.dictationCount
	token := d.token
	d.mu.Unlock()
	if count == 0 {
		d.cancelCapture()
		return
	}
	d.turn.Close()
	d.stopRecording()
	d.setState(Parsing)
	d.opts.Overlay.State("processing")
	d.opts.Overlay.Processing("Transcribing")
	mode := d.smode
	d.submit(func() { d.finishDictation(token, turnAudio, mode, submit) })
}

func (d *Daemon) finishMeetingSession() {
	rec := d.recorder
	d.recorder = nil
	if rec == nil {
		return
	}
	if rec.Samples() < audio.SampleRate/2 {
		_ = rec.Discard()
		d.cancelCapture()
		return
	}
	path, err := rec.Close()
	d.mu.Lock()
	token := d.token
	d.mu.Unlock()
	if err != nil {
		d.log.Printf("%s audio close failed (%v); kept %s", d.smode, err, path)
		d.opts.Overlay.Result(Result{Text: "Audio not finalized — recording kept", Style: "error", AutoClose: 4, ShowText: true})
		d.finishUI(token)
		return
	}
	d.mu.Lock()
	d.processingPath = path
	d.mu.Unlock()
	d.turn.Close()
	d.stopRecording()
	d.setState(Parsing)
	d.opts.Overlay.State("processing")
	d.opts.Overlay.Processing("Identifying speakers")
	if d.smode == config.ModeCollaborative {
		d.submit(func() { d.processCollaborative(token, path) })
	} else {
		d.submit(func() { d.processMeeting(token, path) })
	}
}

// cancelCapture discards the active session without saving cancelled speech.
func (d *Daemon) cancelCapture() {
	if rec := d.recorder; rec != nil {
		d.recorder = nil
		if err := rec.Discard(); err != nil {
			d.log.Printf("meeting discard failed (%v)", err)
		}
	}
	d.mu.Lock()
	if d.processingPath != "" {
		_ = os.Remove(d.processingPath)
		d.processingPath = ""
	}
	d.token++
	d.parts = nil
	if d.draftID != 0 {
		d.opts.Recovery.Drop(d.draftID)
		d.draftID = 0
	}
	d.cleanupSpill()
	d.dictationCount = 0
	d.startedAt = time.Time{}
	d.mu.Unlock()
	d.opts.Overlay.Clear()
	d.turn.Close()
	d.streamOpen = false
	d.stopRecording()
	d.opts.Overlay.SessionClose()
	d.opts.Overlay.State("idle")
	d.setState(Idle)
}

// finishUI returns to idle once the session identified by token completes.
func (d *Daemon) finishUI(token int64) {
	d.mu.Lock()
	if d.token != token {
		d.mu.Unlock()
		return
	}
	d.parts = nil
	d.draftID = 0
	d.cleanupSpill()
	d.dictationCount = 0
	d.startedAt = time.Time{}
	d.state = Idle
	d.mu.Unlock()
	d.opts.Overlay.State("idle")
	d.opts.Overlay.SessionClose()
	d.notify()
	// A configuration change that arrived mid-session applies now.
	select {
	case d.ctl <- command{name: "noop"}:
	default:
	}
}

// checkSessionLimit warns once before the ceiling, then finishes.
func (d *Daemon) checkSessionLimit(now time.Time) {
	if d.getState() != Listening {
		return
	}
	elapsed := now.Sub(d.sessionT0).Seconds()
	maxS := d.cfg().Dictation.SessionMaxS
	switch {
	case elapsed >= maxS:
		d.log.Printf("session max reached — finalizing")
		d.opts.Overlay.Show("Session limit reached — finishing "+d.smode, "warn")
		d.finishCapture(false)
		d.opts.Cues.Play("idle")
	case !d.sessionWarned && elapsed >= maxS-math.Min(60, maxS*0.1):
		d.sessionWarned = true
		d.opts.Overlay.Show("Session ending soon — press Stop to finish", "warn")
		d.opts.Cues.Play("input")
	}
}

// stopRecording writes the one-shot diagnostic recording, if armed.
func (d *Daemon) stopRecording() {
	if d.recBuf == nil {
		return
	}
	buf := d.recBuf
	d.recBuf = nil
	d.recordDone = true
	if d.opts.RecordPath == "" || len(buf) == 0 {
		return
	}
	var all []float32
	for _, b := range buf {
		all = append(all, b...)
	}
	if err := audio.WriteWAV(d.opts.RecordPath, all, audio.SampleRate); err != nil {
		d.log.Printf("record failed: %v", err)
		return
	}
	d.log.Printf("recorded: %s (%.1fs)", d.opts.RecordPath, float64(len(all))/audio.SampleRate)
}
