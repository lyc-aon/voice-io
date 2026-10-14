// Package ui hosts voice-io's native surfaces: the overlay window (which
// replays overlay display lists), the macOS menu bar item, context menus
// and the Settings window.
package ui

import "gitlab.com/integral-inference/voice-io/internal/overlay"

// MenuItem describes one native menu entry.
type MenuItem struct {
	Title     string     `json:"title,omitempty"`
	Subtitle  string     `json:"subtitle,omitempty"`
	Action    string     `json:"action,omitempty"`
	Checked   bool       `json:"checked,omitempty"`
	Disabled  bool       `json:"disabled,omitempty"`
	Separator bool       `json:"separator,omitempty"`
	Header    bool       `json:"header,omitempty"`
	Key       string     `json:"key,omitempty"`  // key equivalent shown in the menu
	Mods      int        `json:"mods,omitempty"` // 1 ctrl, 2 alt, 4 shift, 8 cmd
	Items     []MenuItem `json:"items,omitempty"`
}

// Separator is a menu divider.
var Separator = MenuItem{Separator: true}

// Hooks connect native events to the app.
type Hooks struct {
	Model *overlay.Model
	// Ready runs once on the main thread after the native loop starts.
	Ready func()
	// StatusMenu builds the menu bar item's menu each time it opens.
	StatusMenu func() []MenuItem
	// MenuAction receives a chosen menu action. It runs on the UI thread
	// and must not block.
	MenuAction func(action string)
	// SettingsClosed fires when the Settings window closes.
	SettingsClosed func()
	// Terminate fires when the system asks the app to quit (logout); the
	// app cleans up and then calls ReplyTerminate.
	Terminate func()
}

// Options selects the native surfaces.
type Options struct {
	Overlay    bool
	StatusItem bool
}

var hooks Hooks
