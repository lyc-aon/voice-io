// Package insert pastes finished text at the focused cursor without typing
// into an unverified target. Platform backends move the clipboard and send
// keys; this package owns the cross-platform safety rules.
package insert

import (
	"errors"
	"strings"
)

// Submit selects what happens after a successful paste.
type Submit int

const (
	// SubmitNever suppresses both the spoken-send Enter and auto-submit
	// (used when the receiving window cannot be verified, and for drafts).
	SubmitNever Submit = iota
	// SubmitAuto sends the configured auto-submit chord, if any.
	SubmitAuto
	// SubmitEnter presses Enter because the user spoke the end phrase.
	SubmitEnter
)

// ErrFocusChanged means keyboard focus moved before the paste chord; the
// text is left on the clipboard for a manual paste.
var ErrFocusChanged = errors.New("focus changed before paste")

// Backend is the platform clipboard and keyboard.
type Backend interface {
	// Paste puts text on the clipboard and sends chord, first checking that
	// focus still equals expected (when probe is non-nil). It restores the
	// previous clipboard only if nothing else changed it meanwhile.
	Paste(text, chord, expected string, probe func() string) error
	// Copy leaves text on the clipboard for a manual paste.
	Copy(text string) error
	// Key sends a chord such as "enter" or "ctrl+enter".
	Key(chord string) error
}

// Result reports one insertion attempt.
type Result struct {
	OK       bool
	Executed bool // keys were sent
	Message  string
}

// Executor applies the paste/submit policy over a Backend.
type Executor struct {
	Backend    Backend
	DryRun     bool
	AutoSubmit string // "", "enter" or "ctrl+enter"
}

// Request describes one insertion.
type Request struct {
	Chord    string
	Submit   Submit
	Expected string        // focus identity when the paste was prepared ("" = unknown)
	Probe    func() string // re-reads focus; nil when focus cannot be verified
}

// Dictate pastes text at the current cursor, refusing a target that changed
// since the focus probe.
func (e *Executor) Dictate(text string, r Request) Result {
	if strings.TrimSpace(text) == "" {
		return Result{Message: "empty transcript"}
	}
	if e.DryRun {
		return Result{OK: true, Message: "[dry-run] would insert: " + text}
	}
	err := e.Backend.Paste(text, r.Chord, r.Expected, r.Probe)
	switch {
	case errors.Is(err, ErrFocusChanged):
		return Result{Message: "focus changed before paste; transcript copied to clipboard"}
	case err != nil:
		return Result{Message: "paste failed: " + err.Error() + "; transcript saved for recovery"}
	}
	if r.Submit != SubmitNever {
		if r.Expected != "" && r.Probe != nil && r.Probe() != r.Expected {
			return Result{OK: true, Executed: true, Message: "pasted; submit suppressed after focus change"}
		}
		e.afterPaste(r.Submit)
	}
	return Result{OK: true, Executed: true, Message: "inserted"}
}

func (e *Executor) afterPaste(s Submit) {
	switch {
	case s == SubmitEnter:
		_ = e.Backend.Key("enter")
	case e.AutoSubmit == "enter" || e.AutoSubmit == "ctrl+enter":
		_ = e.Backend.Key(e.AutoSubmit)
	}
}

// CopyOnly leaves text on the clipboard for a deliberate manual paste.
func (e *Executor) CopyOnly(text string) Result {
	if strings.TrimSpace(text) == "" {
		return Result{Message: "empty transcript"}
	}
	if e.DryRun {
		return Result{OK: true, Message: "[dry-run] would copy: " + text}
	}
	if err := e.Backend.Copy(text); err != nil {
		return Result{Message: "clipboard failed: " + err.Error()}
	}
	return Result{OK: true, Message: "transcript copied to clipboard"}
}

// terminalApps use Ctrl+Shift+V on Linux.
var terminalApps = []string{"gnome-terminal", "ghostty", "kitty", "alacritty", "foot",
	"wezterm", "tilix", "xterm", "konsole", "ptyxis"}

// LinuxChord picks the paste shortcut for the focused Linux app. Unknown
// focus uses Shift+Insert, which most toolkits honour.
func LinuxChord(focus string) string {
	if focus == "" {
		return "shift+Insert"
	}
	app, _, _ := strings.Cut(focus, "|")
	app = strings.ReplaceAll(strings.ToLower(app), " ", "-")
	if app == "tern" {
		return "ctrl+shift+v"
	}
	for _, t := range terminalApps {
		if strings.Contains(app, t) {
			return "ctrl+shift+v"
		}
	}
	return "ctrl+v"
}
