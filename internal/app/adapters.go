package app

import (
	"log"
	"sync"

	"gitlab.com/integral-inference/voice-io/internal/daemon"
	"gitlab.com/integral-inference/voice-io/internal/overlay"
	"gitlab.com/integral-inference/voice-io/internal/platform"
	"gitlab.com/integral-inference/voice-io/internal/sound"
)

// overlaySink forwards daemon presentation events to the overlay model and
// keeps the menu bar status and Settings in step.
type overlaySink struct {
	app   *App
	model *overlay.Model
}

func (o overlaySink) State(s string) {
	o.model.State(s)
	o.app.statusChanged()
}

func (o overlaySink) SessionOpen(mode string) {
	o.model.SessionOpen(mode)
	o.app.statusChanged()
}

func (o overlaySink) SessionClose() {
	o.model.SessionClose()
	o.app.statusChanged()
}

func (o overlaySink) Show(text, style string) { o.model.Show(text, style) }
func (o overlaySink) Processing(label string) { o.model.Processing(label) }
func (o overlaySink) Preview(text string)     { o.model.Preview(text) }
func (o overlaySink) Ready(active bool)       { o.model.Ready(active) }
func (o overlaySink) Clear()                  { o.model.Clear() }
func (o overlaySink) Level(rms float64)       { o.model.Level(rms) }

func (o overlaySink) Result(r daemon.Result) {
	o.model.Result(overlay.Result(r))
	o.app.statusChanged()
}

// logSink prints presentation events for headless (--no-ui) runs.
type logSink struct{ log *log.Logger }

func (l logSink) State(s string)          { l.log.Printf("  [overlay:state] %s", s) }
func (l logSink) SessionOpen(mode string) { l.log.Printf("  [overlay:session] open %s", mode) }
func (l logSink) SessionClose()           { l.log.Printf("  [overlay:session] close") }
func (l logSink) Show(text, style string) { l.log.Printf("  [overlay:%s] %s", style, text) }
func (l logSink) Processing(label string) { l.log.Printf("  [overlay:processing] %s", label) }
func (l logSink) Preview(text string)     { l.log.Printf("  [overlay:preview] %s", text) }
func (l logSink) Level(float64)           {}
func (l logSink) Ready(bool)              {}
func (l logSink) Clear()                  {}
func (l logSink) Result(r daemon.Result)  { l.log.Printf("  [overlay:result:%s] %s", r.Style, r.Text) }

// cues plays synthesized sounds when enabled.
type cues struct {
	app    *App
	dryRun bool
	once   sync.Once
}

func (c *cues) Play(name string) {
	if c.dryRun {
		c.app.log.Printf("  [cue] %s", name)
		return
	}
	if !c.app.cfg().UI.Sounds {
		return
	}
	c.once.Do(func() {
		for _, n := range sound.Names() {
			platform.RegisterSound(n, sound.WAV(n))
		}
	})
	platform.PlaySound(name)
}
