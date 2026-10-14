package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Mod is a modifier bitmask for a portable shortcut.
type Mod uint8

const (
	ModCtrl Mod = 1 << iota
	ModAlt
	ModShift
	ModSuper
)

var modAliases = map[string]Mod{
	"ctrl": ModCtrl, "control": ModCtrl,
	"alt": ModAlt, "option": ModAlt,
	"shift": ModShift,
	"super": ModSuper, "cmd": ModSuper, "command": ModSuper, "meta": ModSuper,
}

var namedKeys = func() map[string]bool {
	keys := map[string]bool{}
	for _, k := range []string{"space", "enter", "tab", "escape", "backspace", "delete",
		"up", "down", "left", "right", "home", "end", "pageup", "pagedown", "insert"} {
		keys[k] = true
	}
	for n := 1; n <= 24; n++ {
		keys[fmt.Sprintf("f%d", n)] = true
	}
	return keys
}()

var singleKey = regexp.MustCompile(`^[a-z0-9]$`)

// Hotkey is a desktop-independent modifier+key shortcut such as ctrl+alt+space.
type Hotkey struct {
	Mods Mod
	Key  string
}

// ParseHotkey parses a modifier+key binding. ctrl/alt/shift/super are the
// canonical modifiers; control, option, cmd/command and meta are aliases.
// Keys are a-z, 0-9, F1-F24 or a named key. At least one modifier is required.
func ParseHotkey(binding string) (Hotkey, error) {
	parts := strings.Split(binding, "+")
	for i := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(parts[i]))
	}
	if len(parts) < 2 {
		return Hotkey{}, fmt.Errorf("invalid hotkey %q; expected ctrl+alt+space", binding)
	}
	for _, p := range parts {
		if p == "" {
			return Hotkey{}, fmt.Errorf("invalid hotkey %q; expected ctrl+alt+space", binding)
		}
	}
	var mods Mod
	for _, p := range parts[:len(parts)-1] {
		m, ok := modAliases[p]
		if !ok || mods&m != 0 {
			return Hotkey{}, fmt.Errorf("invalid hotkey modifiers in %q", binding)
		}
		mods |= m
	}
	key := parts[len(parts)-1]
	if !singleKey.MatchString(key) && !namedKeys[key] {
		return Hotkey{}, fmt.Errorf("unsupported hotkey key %q in %q", key, binding)
	}
	return Hotkey{Mods: mods, Key: key}, nil
}

// String is the canonical config spelling, e.g. ctrl+alt+space.
func (h Hotkey) String() string {
	var b strings.Builder
	for _, m := range []struct {
		mod  Mod
		name string
	}{{ModCtrl, "ctrl"}, {ModAlt, "alt"}, {ModShift, "shift"}, {ModSuper, "super"}} {
		if h.Mods&m.mod != 0 {
			b.WriteString(m.name)
			b.WriteByte('+')
		}
	}
	b.WriteString(h.Key)
	return b.String()
}

var macKeyGlyphs = map[string]string{
	"space": "Space", "enter": "↩", "tab": "⇥", "escape": "⎋", "backspace": "⌫",
	"delete": "⌦", "up": "↑", "down": "↓", "left": "←", "right": "→",
	"home": "↖", "end": "↘", "pageup": "⇞", "pagedown": "⇟", "insert": "Insert",
}

var pcKeyNames = map[string]string{
	"space": "Space", "enter": "Enter", "tab": "Tab", "escape": "Esc", "backspace": "Backspace",
	"delete": "Delete", "up": "Up", "down": "Down", "left": "Left", "right": "Right",
	"home": "Home", "end": "End", "pageup": "Page Up", "pagedown": "Page Down", "insert": "Insert",
}

func keyLabel(key string, names map[string]string) string {
	if n, ok := names[key]; ok {
		return n
	}
	return strings.ToUpper(key)
}

// Display renders the shortcut for people: ⌃⌥Space on macOS, Ctrl+Alt+Space elsewhere.
func (h Hotkey) Display(goos string) string {
	if goos == "darwin" {
		var b strings.Builder
		for _, m := range []struct {
			mod   Mod
			glyph string
		}{{ModCtrl, "⌃"}, {ModAlt, "⌥"}, {ModShift, "⇧"}, {ModSuper, "⌘"}} {
			if h.Mods&m.mod != 0 {
				b.WriteString(m.glyph)
			}
		}
		b.WriteString(keyLabel(h.Key, macKeyGlyphs))
		return b.String()
	}
	var parts []string
	for _, m := range []struct {
		mod  Mod
		name string
	}{{ModCtrl, "Ctrl"}, {ModAlt, "Alt"}, {ModShift, "Shift"}, {ModSuper, "Super"}} {
		if h.Mods&m.mod != 0 {
			parts = append(parts, m.name)
		}
	}
	return strings.Join(append(parts, keyLabel(h.Key, pcKeyNames)), "+")
}

var gnomeKeys = map[string]string{
	"enter": "Return", "escape": "Escape", "pageup": "Page_Up", "pagedown": "Page_Down",
	"backspace": "BackSpace", "delete": "Delete", "tab": "Tab", "up": "Up", "down": "Down",
	"left": "Left", "right": "Right", "home": "Home", "end": "End", "insert": "Insert",
}

// GNOME translates ctrl+alt+space into GNOME's <Control><Alt>space.
func (h Hotkey) GNOME() string {
	var b strings.Builder
	for _, m := range []struct {
		mod  Mod
		name string
	}{{ModCtrl, "<Control>"}, {ModAlt, "<Alt>"}, {ModShift, "<Shift>"}, {ModSuper, "<Super>"}} {
		if h.Mods&m.mod != 0 {
			b.WriteString(m.name)
		}
	}
	key := h.Key
	switch {
	case len(key) > 1 && key[0] == 'f' && key[1] >= '0' && key[1] <= '9':
		key = strings.ToUpper(key)
	case gnomeKeys[key] != "":
		key = gnomeKeys[key]
	}
	b.WriteString(key)
	return b.String()
}
