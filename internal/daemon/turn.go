package daemon

import "gitlab.com/integral-inference/voice-io/internal/audio"

// Turn accumulates one utterance's audio. It opens empty at the trigger;
// until speech starts only a short onset ring is retained, then frames
// accumulate until a VAD endpoint (or Stop) closes it.
type Turn struct {
	buf     [][]float32
	samples int
	open    bool
}

// Open starts an empty turn.
func (t *Turn) Open() {
	t.buf, t.samples, t.open = nil, 0, true
}

// Add appends a block when the turn is open. The block is retained, not copied.
func (t *Turn) Add(samples []float32) {
	if t.open {
		t.buf = append(t.buf, samples)
		t.samples += len(samples)
	}
}

// RetainLast bounds the pre-speech onset buffer without copying samples.
func (t *Turn) RetainLast(seconds float64) {
	excess := t.samples - int(seconds*audio.SampleRate)
	for excess > 0 && len(t.buf) > 0 {
		first := t.buf[0]
		if excess >= len(first) {
			excess -= len(first)
			t.samples -= len(first)
			t.buf = t.buf[1:]
			continue
		}
		t.buf[0] = first[excess:]
		t.samples -= excess
		break
	}
}

// IsOpen reports whether a turn is collecting audio.
func (t *Turn) IsOpen() bool { return t.open }

// Seconds is the buffered duration.
func (t *Turn) Seconds() float64 {
	if !t.open {
		return 0
	}
	return float64(t.samples) / audio.SampleRate
}

// Audio returns the buffered samples as one slice (nil when empty).
func (t *Turn) Audio() []float32 {
	switch len(t.buf) {
	case 0:
		return nil
	case 1:
		return t.buf[0]
	}
	out := make([]float32, 0, t.samples)
	for _, b := range t.buf {
		out = append(out, b...)
	}
	return out
}

// Close discards the turn.
func (t *Turn) Close() {
	t.buf, t.samples, t.open = nil, 0, false
}
