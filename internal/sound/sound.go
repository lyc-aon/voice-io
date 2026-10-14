// Package sound synthesizes voice-io's short, non-verbal cues as WAV data.
// Each cue is a soft bell (a sine with gentle harmonics and an exponential
// decay), so starts, finishes, cancels and errors are distinguishable by
// pitch contour without being harsh.
package sound

import (
	"encoding/binary"
	"math"
)

const rate = 44100

type note struct {
	freq   float64 // Hz
	at     float64 // start (s)
	length float64 // duration (s)
	decay  float64 // exponential decay time constant (s)
	gain   float64
	bright float64 // weight of upper harmonics
}

// Cues maps cue names to the notes that compose them.
var cues = map[string][]note{
	// Start listening: a light rising fifth.
	"listening": {{880, 0, 0.12, 0.08, 0.55, 0.20}, {1318.5, 0.075, 0.22, 0.11, 0.6, 0.18}},
	// Text inserted / notes saved: a bright two-note "plink".
	"executed": {{1568, 0, 0.12, 0.06, 0.45, 0.15}, {2093, 0.05, 0.2, 0.08, 0.5, 0.12}},
	// Cancel / back to idle: the start contour, falling and quieter.
	"idle": {{1318.5, 0, 0.12, 0.07, 0.38, 0.15}, {880, 0.07, 0.2, 0.09, 0.4, 0.15}},
	// Attention (session ending soon): two soft taps.
	"input": {{987.8, 0, 0.09, 0.05, 0.45, 0.2}, {987.8, 0.13, 0.12, 0.06, 0.45, 0.2}},
	// Error: a low, rounded falling pair.
	"error": {{392, 0, 0.16, 0.10, 0.55, 0.45}, {293.7, 0.12, 0.26, 0.14, 0.6, 0.45}},
}

// Names lists every cue.
func Names() []string { return []string{"listening", "executed", "idle", "input", "error"} }

// WAV renders a cue as a 44.1 kHz mono 16-bit WAV (nil for unknown names).
func WAV(name string) []byte {
	notes, ok := cues[name]
	if !ok {
		return nil
	}
	end := 0.0
	for _, n := range notes {
		end = math.Max(end, n.at+n.length)
	}
	samples := make([]float64, int(end*rate)+1)
	for _, n := range notes {
		start := int(n.at * rate)
		count := int(n.length * rate)
		attack := 0.004 * rate
		release := 0.012 * rate
		for i := range count {
			t := float64(i) / rate
			env := math.Exp(-t / n.decay)
			if fi := float64(i); fi < attack {
				env *= fi / attack
			}
			if left := float64(count - i); left < release {
				env *= left / release
			}
			ph := 2 * math.Pi * n.freq * t
			v := math.Sin(ph) + n.bright*0.5*math.Sin(2*ph) + n.bright*0.2*math.Sin(3*ph)
			if j := start + i; j < len(samples) {
				samples[j] += n.gain * env * v
			}
		}
	}
	peak := 0.0
	for _, s := range samples {
		peak = math.Max(peak, math.Abs(s))
	}
	scale := 0.0
	if peak > 0 {
		scale = 0.16 / peak // about -16 dBFS: present but never loud
	}
	out := make([]byte, 44, 44+2*len(samples))
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+2*len(samples)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], 1)
	binary.LittleEndian.PutUint32(out[24:], rate)
	binary.LittleEndian.PutUint32(out[28:], rate*2)
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(2*len(samples)))
	for _, s := range samples {
		out = binary.LittleEndian.AppendUint16(out, uint16(int16(math.Round(s*scale*32767))))
	}
	return out
}
