package daemon

import (
	"gitlab.com/integral-inference/voice-io/internal/insert"
)

// State is the capture state.
type State string

const (
	Idle      State = "IDLE"
	Listening State = "LISTENING"
	Parsing   State = "PARSING"
)

// Overlay receives presentation events. Implementations must be safe for
// concurrent use and must never block.
type Overlay interface {
	// State is idle | listening | processing | off.
	State(state string)
	SessionOpen(mode string)
	SessionClose()
	// Show displays a transient status: info | warn | error | prompt.
	Show(text, style string)
	// Processing labels the processing phase ("Transcribing", "Polishing").
	Processing(label string)
	Preview(text string)
	Level(rms float64)
	Result(r Result)
	Ready(active bool)
	Clear()
}

// Result finishes a session in the overlay.
type Result struct {
	Text       string
	Style      string // ok | warn | error
	AutoClose  float64
	ShowText   bool   // display Text on success (status copy, never transcripts)
	Reveal     string // optional file the user can open from the result
	Permission string // optional permission the user can grant from the result
}

// Cues plays non-verbal sounds: listening, executed, idle, input, error.
type Cues interface {
	Play(name string)
}

// Focus identifies the focused window, returning "" when unknown.
type Focus interface {
	Probe() string
	// Chord returns the paste chord for the focused target.
	Chord(focus string) string
	// PasteBlocked names a missing permission that stops keys from being
	// sent (id and short message), or returns empty strings.
	PasteBlocked() (id, message string)
	// PasteHint names the manual paste shortcut ("⌘V", "Ctrl+V").
	PasteHint() string
}

// VAD is the voice activity detector.
type VAD interface {
	Accept(samples []float32)
	IsSpeech() bool
	PopSegments() []float64
	Clear()
}

// Spotter detects WAKE and SEND keywords.
type Spotter interface {
	Accept(samples []float32) ([]string, error)
}

// Streaming is the preview recognizer.
type Streaming interface {
	NewStream()
	Accept(samples []float32) string
	Finalize(tailSeconds float64) string
}

// Refiner is the authoritative offline recognizer.
type Refiner interface {
	Transcribe(samples []float32) (string, error)
	Warm() error
}

// Inserter pastes finished text.
type Inserter interface {
	Dictate(text string, r insert.Request) insert.Result
	CopyOnly(text string) insert.Result
}

// Logger receives daemon diagnostics.
type Logger interface {
	Printf(format string, args ...any)
}

// Status is a snapshot for the control socket and Settings.
type Status struct {
	State        State   `json:"state"`
	Mode         string  `json:"mode"`
	Enabled      bool    `json:"enabled"`
	Ready        bool    `json:"ready"`
	Setup        []Issue `json:"setup,omitempty"`
	Microphone   string  `json:"microphone"`
	SessionStart float64 `json:"session_start,omitempty"`
	Version      string  `json:"version"`
}

// Issue is something the user must fix before dictation works.
type Issue struct {
	ID      string `json:"id"` // models | meeting-models | microphone | accessibility | config
	Message string `json:"message"`
}
