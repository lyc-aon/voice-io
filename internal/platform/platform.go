// Package platform adapts voice-io to the desktop: focused-window identity,
// clipboard paste, global shortcuts, sounds, privacy permissions, login
// items and file reveal. macOS uses AppKit/Accessibility/Carbon through cgo;
// Linux (GNOME) uses the session bus and the wl-clipboard/ydotool tools.
package platform

import "errors"

// Permission is one privacy grant or system dependency.
type Permission struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	State    string `json:"state"` // granted | denied | prompt | missing | optional
	Detail   string `json:"detail"`
	Required bool   `json:"required"`
	Action   string `json:"action,omitempty"` // request | settings | install
}

// HotkeyAction names the shortcut commands.
type HotkeyAction string

const (
	ActionPush   HotkeyAction = "push"
	ActionToggle HotkeyAction = "toggle"
	ActionCancel HotkeyAction = "cancel"
)

// ErrUnsupported means the feature does not exist on this desktop.
var ErrUnsupported = errors.New("not supported on this system")

// Focus implements the daemon's focus policy for this platform.
type Focus struct{}

// Probe returns the focused window identity, or "" when it cannot be verified.
func (Focus) Probe() string { return probeFocus() }

// Chord returns the paste shortcut for the focused target.
func (Focus) Chord(focus string) string { return pasteChord(focus) }

// PasteBlocked names a missing permission that stops keys from being sent
// (its id and a short message), or returns empty strings.
func (Focus) PasteBlocked() (id, message string) { return pasteBlocked() }

// PasteHint names the manual paste shortcut.
func (Focus) PasteHint() string { return pasteHint }
