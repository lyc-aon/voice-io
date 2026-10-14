package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/insert"
	"gitlab.com/integral-inference/voice-io/internal/llm"
	"gitlab.com/integral-inference/voice-io/internal/meeting"
	"gitlab.com/integral-inference/voice-io/internal/text"
)

func stripLeading(t, phrase string) string { return text.StripLeadingPhrase(t, phrase) }

func stripEnd(t, phrase string, spoken bool) string {
	if spoken {
		return text.StripSpokenEndPhrase(t, phrase)
	}
	return text.StripEndPhrase(t, phrase)
}

// outcome records one dictation for the log.
type outcome struct {
	final      string
	inserted   bool
	unverified bool
	label      string
	result     insert.Result
}

type pasteItem struct {
	text      string
	ev        *outcome
	submit    insert.Submit
	recoverID int64
	samples   []float32 // dictation audio kept for a failed paste
	wav       string    // collaborative recording, deleted once text is safe
	mode      string
	token     int64
}

// dictationWorker re-decodes one turn in capture order and saves the
// growing draft for recovery. It never pastes.
func (d *Daemon) dictationWorker(spill string, samples []float32, final string, closeSession, spokenEnd bool,
	token int64, stripWake bool) {
	d.mu.Lock()
	current := d.token == token
	d.mu.Unlock()
	if !current {
		if spill != "" {
			_ = os.Remove(spill)
		}
		return
	}
	cancelled := d.cancelledFunc(token)
	if spill != "" {
		var err error
		samples, err = readSpill(spill)
		_ = os.Remove(spill)
		if err != nil {
			if cancelled() {
				return // cancel removed the spill directory
			}
			d.log.Printf("    turn audio unavailable (%v); streaming final fallback", err)
			d.opts.Overlay.Show("Audio unavailable — using live transcript", "warn")
		}
	}
	refined := final
	if samples != nil {
		if t, err := d.engines.Refiner.Transcribe(samples); err != nil {
			d.log.Printf("    refine failed (%v); streaming final fallback", err)
		} else {
			refined = t
		}
	}
	refined = d.stripPhrases(refined, stripWake, closeSession, spokenEnd)
	refined = strings.TrimSpace(refined)
	if refined != "" && refined != final {
		d.log.Printf("    refine:   %q", refined)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != token || refined == "" {
		return
	}
	d.parts = append(d.parts, refined)
	id, serr := d.opts.Recovery.Spool("draft", strings.Join(d.parts, " "), d.sessionMode, "")
	if serr != nil {
		d.log.Printf("    live draft recovery unavailable (%v)", serr)
		if !d.draftWarned {
			d.draftWarned = true
			d.opts.Overlay.Show("Draft recovery unavailable", "warn")
		}
		return
	}
	if d.draftID != 0 {
		d.opts.Recovery.Drop(d.draftID)
	}
	d.draftID = id
}

// finishDictation joins the ordered turns, cleans and polishes once, saves
// the commit for recovery and queues a single paste.
func (d *Daemon) finishDictation(token int64, samples []float32, mode string, submit bool) {
	d.mu.Lock()
	if d.token != token {
		d.mu.Unlock()
		return
	}
	joined := strings.TrimSpace(strings.Join(d.parts, " "))
	d.mu.Unlock()
	cfg := d.cfg()
	words, err := text.LoadMap(cfg.VocabularyPath())
	if err != nil {
		d.log.Printf("vocabulary unavailable (%v)", err)
	}
	snippets, err := text.LoadMap(cfg.SnippetsPath())
	if err != nil {
		d.log.Printf("snippets unavailable (%v)", err)
	}
	result := joined
	snippet, isSnippet := text.SnippetMatch(joined, snippets)
	switch {
	case isSnippet:
		result = snippet
	case result != "" && cfg.Polish.Deterministic:
		result = text.Cleanup(result, text.Options{Words: words, Filler: cfg.Polish.Filler, Emoji: cfg.Polish.Emoji})
	}
	if result != "" && !isSnippet {
		result = d.polishText(token, result)
	}
	ev := &outcome{final: joined}
	sub := insert.SubmitAuto
	if submit {
		sub = insert.SubmitEnter
	}
	d.mu.Lock()
	if d.token != token {
		d.mu.Unlock()
		return
	}
	if result != "" {
		id, err := d.opts.Recovery.Spool("commit", result, mode, "")
		if err != nil {
			d.log.Printf("    recovery storage unavailable (%v)", err)
			id = 0
			d.opts.Overlay.Show("Recovery unavailable — finishing paste", "warn")
		}
		if id != 0 && d.draftID != 0 {
			d.opts.Recovery.Drop(d.draftID)
			d.draftID = 0
		}
		d.pastes.put(&pasteItem{text: result, ev: ev, submit: sub, recoverID: id, samples: samples,
			mode: mode, token: token})
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	ev.label = "Didn't understand"
	d.opts.Overlay.Result(Result{Text: "Didn't catch that", Style: "error", AutoClose: 2})
	d.finishUI(token)
	d.printOutcome(ev)
}

// polishText runs the optional LLM copy-edit on short sessions only; a
// 600-token completion cannot cover an hour-long transcript.
func (d *Daemon) polishText(token int64, t string) string {
	cfg := d.cfg()
	if !cfg.Polish.Enabled || cfg.LLM.Endpoint == "" || len(t) < 8 || len(t) > 6000 {
		return t
	}
	d.opts.Overlay.Processing("Polishing")
	system, user := text.PolishPrompt(t)
	client := d.polish
	out, err := client.Complete(context.Background(), llm.Request{System: system, User: user,
		MaxTokens: 600, Temperature: 0.2, Timeout: 12 * time.Second})
	if err != nil {
		d.log.Printf("    polish unavailable (%v)", err)
		return t
	}
	d.mu.Lock()
	stale := d.token != token
	d.mu.Unlock()
	if stale {
		return t
	}
	return text.PolishGuard(t, out)
}

func (d *Daemon) cancelledFunc(token int64) func() bool {
	return func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.token != token
	}
}

// processMeeting publishes the transcript (and summary) without pasting.
func (d *Daemon) processMeeting(token int64, recording string) {
	cancelled := d.cancelledFunc(token)
	proc := d.meetingProcessor()
	res, err := proc.Process(recording, cancelled)
	switch {
	case errors.Is(err, meeting.ErrCancelled):
		_ = os.Remove(recording)
		return
	case err != nil:
		if !cancelled() {
			d.log.Printf("meeting processing failed (%v); audio: %s", err, recording)
			d.opts.Overlay.Result(Result{Text: "Meeting processing failed — audio kept", Style: "error", AutoClose: 4})
			d.opts.Cues.Play("error")
		}
	default:
		if cancelled() {
			_ = os.Remove(res.Document)
			return
		}
		d.log.Printf("meeting transcript saved: %s", res.Document)
		if res.SummaryErr != nil {
			d.log.Printf("meeting summary unavailable (%v)", res.SummaryErr)
			d.opts.Overlay.Result(Result{Text: "Transcript saved; summary unavailable", Style: "warn",
				AutoClose: 4, ShowText: true, Reveal: res.Document})
			d.opts.Cues.Play("error")
		} else {
			d.opts.Overlay.Result(Result{Text: "Meeting notes saved", Style: "ok", AutoClose: 4,
				ShowText: true, Reveal: res.Document})
			d.opts.Cues.Play("executed")
		}
	}
	if !cancelled() {
		d.mu.Lock()
		d.processingPath = ""
		d.mu.Unlock()
		d.finishUI(token)
	}
}

// processCollaborative diarizes after Stop and pastes one editable,
// speaker-labelled draft. The private WAV is kept until the text is safe.
func (d *Daemon) processCollaborative(token int64, recording string) {
	cancelled := d.cancelledFunc(token)
	queued := false
	defer func() {
		if !queued && !cancelled() {
			d.mu.Lock()
			d.processingPath = ""
			d.mu.Unlock()
			d.finishUI(token)
		}
	}()
	err := func() error {
		if cancelled() {
			return meeting.ErrCancelled
		}
		samples, rate, err := audio.ReadWAV(recording)
		if err != nil {
			return err
		}
		if rate != audio.SampleRate {
			return errors.New("unexpected sample rate in collaborative recording")
		}
		lines, err := d.meetingProcessor().Diarize(samples, cancelled)
		if err != nil {
			return err
		}
		if cancelled() {
			return meeting.ErrCancelled
		}
		cfg := d.cfg()
		opts := text.Options{}
		if cfg.Polish.Deterministic {
			words, _ := text.LoadMap(cfg.VocabularyPath())
			opts = text.Options{Words: words, Filler: cfg.Polish.Filler, Emoji: cfg.Polish.Emoji}
		}
		draft := meeting.CollaborativeText(lines, opts)
		if draft == "" {
			return errors.New("no intelligible speech found")
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.token != token {
			return meeting.ErrCancelled
		}
		id, err := d.opts.Recovery.Spool("commit", draft, config.ModeCollaborative, "")
		if err != nil {
			d.log.Printf("    collaborative recovery unavailable (%v)", err)
			id = 0
			d.opts.Overlay.Show("Recovery unavailable — finishing paste", "warn")
		}
		d.pastes.put(&pasteItem{text: draft, ev: &outcome{}, submit: insert.SubmitNever, recoverID: id,
			wav: recording, mode: config.ModeCollaborative, token: token})
		queued = true
		return nil
	}()
	switch {
	case errors.Is(err, meeting.ErrCancelled):
		_ = os.Remove(recording)
	case err != nil && !cancelled():
		d.log.Printf("collaborative processing failed (%v); audio: %s", err, recording)
		d.opts.Overlay.Result(Result{Text: "Transcription failed — audio kept", Style: "error", AutoClose: 4})
		d.opts.Cues.Play("error")
	}
}

// meetingProcessor returns the speaker pipeline with the current summary
// endpoint (summary settings apply without reloading models). Worker only.
func (d *Daemon) meetingProcessor() *meeting.Processor {
	p := d.engines.Meeting
	cfg := d.cfg()
	p.Summarizer = &meeting.Summarizer{Client: &llm.Client{Endpoint: cfg.Meeting.SummaryEndpoint,
		Model: cfg.Meeting.SummaryModel, APIKeyEnv: cfg.Meeting.SummaryAPIKeyEnv}}
	return p
}

// pasteLoop inserts each finished session once, off the capture thread.
func (d *Daemon) pasteLoop() {
	defer d.wg.Done()
	for {
		item, ok := d.pastes.get()
		if !ok {
			return
		}
		d.pasteOne(item)
	}
}

func (d *Daemon) pasteOne(item *pasteItem) {
	d.mu.Lock()
	current := d.token == item.token
	d.mu.Unlock()
	if !current {
		d.opts.Recovery.Drop(item.recoverID)
		if item.wav != "" {
			_ = os.Remove(item.wav)
		}
		return
	}
	d.opts.Overlay.Processing("Inserting")
	res := d.pasteAtFocus(item.text, item.ev, item.submit)
	inserted := res.OK && item.ev.inserted
	if inserted {
		d.opts.Cues.Play("executed")
	} else {
		d.opts.Cues.Play("error")
	}
	persisted := inserted || item.recoverID != 0
	if inserted {
		// An unverified paste can be delivered without anything confirming
		// the target accepted it; keep its recovery copy quietly.
		if item.recoverID != 0 && !item.ev.unverified {
			d.opts.Recovery.Drop(item.recoverID)
		}
		d.appendHistory(item.text, item.mode)
	} else {
		audioPath := ""
		if item.wav == "" && len(item.samples) > 0 {
			if p, err := d.opts.Recovery.SaveAudio(item.samples); err != nil {
				d.log.Printf("    recovery audio unavailable (%v)", err)
			} else {
				audioPath = p
			}
		}
		if _, err := d.opts.Recovery.Spool("failed", item.text, item.mode, audioPath); err != nil {
			d.log.Printf("    recovery transcript unavailable (%v)", err)
			if item.recoverID == 0 {
				copied := d.opts.Inserter.CopyOnly(item.text)
				if copied.OK {
					d.opts.Overlay.Result(Result{Text: "Paste failed — text copied; paste manually", Style: "warn", AutoClose: 4})
				} else {
					d.opts.Overlay.Result(Result{Text: "Paste failed — cannot save text", Style: "error", AutoClose: 4})
				}
			}
		} else {
			persisted = true
			d.opts.Recovery.Drop(item.recoverID)
		}
	}
	if item.wav != "" {
		if persisted {
			_ = os.Remove(item.wav)
		} else {
			d.log.Printf("    collaborative audio retained: %s", item.wav)
		}
		d.mu.Lock()
		if d.token == item.token {
			d.processingPath = ""
		}
		d.mu.Unlock()
	}
	if persisted {
		d.mu.Lock()
		if d.token == item.token && d.draftID != 0 {
			d.opts.Recovery.Drop(d.draftID)
			d.draftID = 0
		}
		d.mu.Unlock()
	}
	d.finishUI(item.token)
	d.printOutcome(item.ev)
}

// pasteAtFocus pastes into the focus present at completion, not at the
// start. A window that can be identified is re-checked right before the
// paste chord; when none can be, the paste still goes to the current
// keyboard focus but never presses Return.
func (d *Daemon) pasteAtFocus(t string, ev *outcome, submit insert.Submit) insert.Result {
	if !d.cfg().Exec.AutoPaste {
		// Copy-only by choice: delivered, but the clipboard can be
		// overwritten, so the recovery copy stays.
		res := d.opts.Inserter.CopyOnly(t)
		if res.OK {
			d.opts.Overlay.Result(Result{Text: "Copied — paste with " + d.opts.Focus.PasteHint(), Style: "ok",
				AutoClose: 2.5, ShowText: true})
		} else {
			d.opts.Overlay.Result(Result{Text: "Copy failed — transcript saved for recovery", Style: "error", AutoClose: 3.5})
		}
		ev.inserted, ev.unverified, ev.result = res.OK, res.OK, res
		return res
	}
	if perm, why := d.opts.Focus.PasteBlocked(); perm != "" {
		res := d.opts.Inserter.CopyOnly(t)
		text := "Copied — " + why
		if !res.OK {
			text = "Paste unavailable — transcript saved for recovery"
		}
		d.opts.Overlay.Result(Result{Text: text, Style: "warn", AutoClose: 6, ShowText: true, Permission: perm})
		ev.inserted, ev.result = false, res
		return res
	}
	focus := d.opts.Focus.Probe()
	req := insert.Request{Chord: d.opts.Focus.Chord(focus), Submit: submit, Expected: focus}
	if focus != "" {
		req.Probe = d.opts.Focus.Probe
	} else {
		req.Submit = insert.SubmitNever // cannot verify the receiving window
	}
	res := d.opts.Inserter.Dictate(t, req)
	switch {
	case !res.OK:
		d.opts.Overlay.Result(Result{Text: "Paste failed — transcript saved for recovery", Style: "error", AutoClose: 3})
	case focus != "":
		d.opts.Overlay.Result(Result{Text: "Inserted", Style: "ok", AutoClose: 1.4})
	default:
		d.opts.Overlay.Result(Result{Text: "Paste sent", Style: "ok", AutoClose: 1.4})
	}
	ev.inserted = res.OK
	ev.unverified = res.OK && focus == ""
	ev.result = res
	return res
}

func (d *Daemon) appendHistory(t, mode string) {
	cfg := d.cfg()
	if !cfg.Dictation.KeepHistory || d.opts.History == nil {
		return
	}
	d.opts.History.SetPath(cfg.HistoryPath())
	if err := d.opts.History.Append(t, mode); err != nil {
		d.log.Printf("    history unavailable (%v)", err)
	}
}

func (d *Daemon) printOutcome(ev *outcome) {
	if ev == nil {
		return
	}
	switch {
	case !ev.inserted && ev.label != "":
		d.log.Printf("    inserted: false (%s)", ev.label)
	case !ev.inserted:
		d.log.Printf("    inserted: false (%s)", ev.result.Message)
	default:
		d.log.Printf("    inserted: %s", firstLine(ev.result.Message))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "…"
	}
	return s
}
