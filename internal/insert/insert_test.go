package insert

import (
	"errors"
	"strings"
	"testing"
)

// fakeBackend models a clipboard and keyboard that refuse to paste into a
// window other than the expected one.
type fakeBackend struct {
	focus     []string // successive focus probes
	probes    int
	clipboard string
	pasted    []string
	keys      []string
	pasteErr  error
}

func (b *fakeBackend) probe() string {
	f := b.focus[min(b.probes, len(b.focus)-1)]
	b.probes++
	return f
}

func (b *fakeBackend) Paste(text, chord, expected string, probe func() string) error {
	b.clipboard = text
	if b.pasteErr != nil {
		return b.pasteErr
	}
	if probe != nil && probe() != expected {
		return ErrFocusChanged // text stays on the clipboard for a manual paste
	}
	b.pasted = append(b.pasted, chord+":"+text)
	return nil
}

func (b *fakeBackend) Copy(text string) error { b.clipboard = text; return nil }

func (b *fakeBackend) Key(chord string) error { b.keys = append(b.keys, chord); return nil }

func TestFocusChangeBeforePasteLeavesTextForManualPaste(t *testing.T) {
	b := &fakeBackend{focus: []string{"another editor"}}
	e := &Executor{Backend: b, AutoSubmit: "enter"}
	res := e.Dictate("Final cleaned transcript", Request{Chord: "ctrl+v", Submit: SubmitEnter, Expected: "original editor", Probe: b.probe})
	if res.OK || res.Executed || !strings.Contains(res.Message, "copied to clipboard") {
		t.Fatalf("result %+v", res)
	}
	if len(b.pasted) != 0 || len(b.keys) != 0 || b.clipboard != "Final cleaned transcript" {
		t.Fatalf("pasted=%v keys=%v clipboard=%q", b.pasted, b.keys, b.clipboard)
	}
}

func TestSpokenEndPressesEnterOnlyInTheSameWindow(t *testing.T) {
	b := &fakeBackend{focus: []string{"tern|window:1"}}
	e := &Executor{Backend: b, AutoSubmit: "ctrl+enter"}
	res := e.Dictate("ship it", Request{Chord: "ctrl+shift+v", Submit: SubmitEnter, Expected: "tern|window:1", Probe: b.probe})
	if !res.OK || !res.Executed || strings.Join(b.keys, ",") != "enter" {
		t.Fatalf("result %+v keys %v", res, b.keys)
	}

	moved := &fakeBackend{focus: []string{"tern|window:1", "notes|window:2"}}
	e = &Executor{Backend: moved, AutoSubmit: "enter"}
	res = e.Dictate("ship it", Request{Chord: "ctrl+shift+v", Submit: SubmitEnter, Expected: "tern|window:1", Probe: moved.probe})
	if !res.OK || !strings.Contains(res.Message, "submit suppressed") || len(moved.keys) != 0 {
		t.Fatalf("after focus moved: %+v keys %v", res, moved.keys)
	}
}

func TestAutoSubmitFollowsSettingsAndNeverOverridesNoSubmit(t *testing.T) {
	for _, c := range []struct {
		auto   string
		submit Submit
		keys   string
	}{
		{"", SubmitAuto, ""},
		{"enter", SubmitAuto, "enter"},
		{"ctrl+enter", SubmitAuto, "ctrl+enter"},
		{"ctrl+enter", SubmitNever, ""},
		{"", SubmitEnter, "enter"},
	} {
		b := &fakeBackend{focus: []string{"app|1"}}
		e := &Executor{Backend: b, AutoSubmit: c.auto}
		e.Dictate("text", Request{Chord: "ctrl+v", Submit: c.submit, Expected: "app|1", Probe: b.probe})
		if got := strings.Join(b.keys, ","); got != c.keys {
			t.Errorf("auto=%q submit=%d sent %q; want %q", c.auto, c.submit, got, c.keys)
		}
	}
}

func TestFailedPasteIsNotSuccess(t *testing.T) {
	b := &fakeBackend{focus: []string{"app|1"}, pasteErr: errors.New("Accessibility permission required")}
	res := (&Executor{Backend: b}).Dictate("text", Request{Chord: "cmd+v", Submit: SubmitEnter, Expected: "app|1", Probe: b.probe})
	if res.OK || !strings.Contains(res.Message, "Accessibility") || len(b.keys) != 0 {
		t.Fatalf("result %+v keys %v", res, b.keys)
	}
	if res := (&Executor{Backend: b}).Dictate("  ", Request{}); res.OK {
		t.Fatal("empty transcript pasted")
	}
}

func TestDryRunNeverTouchesTheDesktop(t *testing.T) {
	b := &fakeBackend{focus: []string{"app|1"}}
	e := &Executor{Backend: b, DryRun: true, AutoSubmit: "enter"}
	if res := e.Dictate("hello", Request{Submit: SubmitEnter}); !res.OK || !strings.Contains(res.Message, "hello") {
		t.Fatalf("dry run %+v", res)
	}
	if res := e.CopyOnly("hello"); !res.OK {
		t.Fatalf("dry copy %+v", res)
	}
	if b.clipboard != "" || len(b.pasted)+len(b.keys) != 0 {
		t.Fatal("dry run reached the backend")
	}
}

func TestLinuxChordMatchesTheFocusedApp(t *testing.T) {
	for focus, want := range map[string]string{
		"":                          "shift+Insert",
		"tern|gnome:window:2":       "ctrl+shift+v",
		"org.gnome.Ptyxis|window:3": "ctrl+shift+v",
		"Alacritty|0x2":             "ctrl+shift+v",
		"firefox|gnome:window:9":    "ctrl+v",
		"gedit|1":                   "ctrl+v",
	} {
		if got := LinuxChord(focus); got != want {
			t.Errorf("LinuxChord(%q) = %q; want %q", focus, got, want)
		}
	}
}
