package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/integral-inference/voice-io/internal/paths"
)

func TestHotkeysArePortableAndInvalidKeysRejected(t *testing.T) {
	h, err := ParseHotkey("Control+Option+Space")
	if err != nil {
		t.Fatal(err)
	}
	if h.Mods != ModCtrl|ModAlt || h.Key != "space" || h.String() != "ctrl+alt+space" {
		t.Fatalf("parsed %+v (%s)", h, h)
	}
	if got := h.Display("darwin"); got != "⌃⌥Space" {
		t.Errorf("mac display = %q", got)
	}
	if got := h.Display("linux"); got != "Ctrl+Alt+Space" {
		t.Errorf("linux display = %q", got)
	}
	f3, err := ParseHotkey("super+shift+f3")
	if err != nil {
		t.Fatal(err)
	}
	if got := f3.GNOME(); got != "<Shift><Super>F3" {
		t.Errorf("GNOME = %q", got)
	}
	cmd, _ := ParseHotkey("cmd+enter")
	if cmd.GNOME() != "<Super>Return" || cmd.Display("darwin") != "⌘↩" {
		t.Errorf("cmd+enter renders %q / %q", cmd.GNOME(), cmd.Display("darwin"))
	}
	for _, bad := range []string{"space", "ctrl+alt+", "ctrl+ctrl+a", "ctrl+hyper+a", "ctrl+alt+randomkey", "not a shortcut", ""} {
		if _, err := ParseHotkey(bad); err == nil {
			t.Errorf("ParseHotkey(%q) accepted", bad)
		}
	}
}

func TestShortcutsMustBeDistinctAcrossSpellings(t *testing.T) {
	h := Default().Hotkeys
	h.Cancel = "control+option+t" // same chord as the default toggle, other spelling
	h.Toggle = "ctrl+alt+t"
	if _, err := h.Parse(); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("duplicate shortcuts accepted: %v", err)
	}
}

func TestUpdatePersistsFieldsAndPreservesEverythingElse(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	original := "# keep this note\n[custom]\nnote = \"mine\"\n\n[ui]\n# theme comment\ntheme = \"dark\"\n"
	if err := os.WriteFile(file, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Update(file, Updates{
		"ui":      {"sounds": true, "theme": "light", "live_preview": false, "preview_color": "#53cc88"},
		"hotkeys": {"push": "super+shift+p"},
		"kws":     {"wake_phrase": "hello computer", "end_phrase": "computer stop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UI.Sounds || cfg.UI.Theme != "light" || cfg.UI.LivePreview || cfg.UI.PreviewColor != "#53cc88" ||
		cfg.Hotkeys.Push != "super+shift+p" || cfg.KWS.WakePhrase != "hello computer" || cfg.KWS.EndPhrase != "computer stop" {
		t.Fatalf("returned config %+v", cfg)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, keep := range []string{"# keep this note", "[custom]", `note = "mine"`, "# theme comment"} {
		if !strings.Contains(text, keep) {
			t.Errorf("lost %q:\n%s", keep, text)
		}
	}
	if strings.Contains(text, `theme = "dark"`) {
		t.Errorf("old value survived:\n%s", text)
	}
	loaded, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.UI.Theme != "light" || loaded.Hotkeys.Push != "super+shift+p" || loaded.KWS.EndPhrase != "computer stop" {
		t.Fatalf("reload %+v", loaded.UI)
	}
	if info, _ := os.Stat(file); info.Mode().Perm()&0o077 != 0 {
		t.Errorf("config mode %v is not private", info.Mode().Perm())
	}
}

func TestUpdateRejectsInvalidValuesWithoutTouchingTheFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(file, []byte("[ui]\ntheme = \"dark\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		u    Updates
		want string
	}{
		{Updates{"hotkeys": {"push": "not a shortcut"}}, "hotkeys.push"},
		{Updates{"hotkeys": {"cancel": "control+option+t"}}, "distinct"},
		{Updates{"ui": {"preview_color": "green"}}, "ui.preview_color"},
		{Updates{"ui": {"preview_color": "#12345"}}, "ui.preview_color"},
		{Updates{"ui": {"accent": "blue"}}, "ui.accent"},
		{Updates{"ui": {"theme": "sepia"}}, "ui.theme"},
		{Updates{"ui": {"sounds": "yes"}}, "true or false"},
		{Updates{"meeting": {"mode": "streaming"}}, "meeting.mode"},
		{Updates{"meeting": {"summary_endpoint": "file:///private/transcript"}}, "summary_endpoint"},
		{Updates{"meeting": {"summary_endpoint": "https://secret@api.example/v1"}}, "summary_endpoint"},
		{Updates{"meeting": {"summary_api_key_env": "KEY; echo bad"}}, "summary_api_key_env"},
		{Updates{"llm": {"api_key_env": "$(whoami)"}}, "llm.api_key_env"},
		{Updates{"exec": {"auto_submit": "shift+enter"}}, "exec.auto_submit"},
		{Updates{"nope": {"x": 1}}, "unsupported config section"},
		{Updates{"ui": {"pill": true}}, "unsupported config key"},
	}
	for _, c := range cases {
		_, err := Update(file, c.u)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Update(%v) = %v; want error containing %q", c.u, err, c.want)
		}
	}
	data, _ := os.ReadFile(file)
	if string(data) != "[ui]\ntheme = \"dark\"\n" {
		t.Fatalf("rejected updates changed the file:\n%s", data)
	}
}

func TestUpdateNeverOverwritesAMalformedFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	broken := "[ui\ntheme = \"dark\"\n"
	if err := os.WriteFile(file, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(file, Updates{"ui": {"theme": "light"}}); err == nil {
		t.Fatal("update of malformed file succeeded")
	}
	if data, _ := os.ReadFile(file); string(data) != broken {
		t.Fatalf("malformed file rewritten:\n%s", data)
	}
}

func TestMeetingSettingsRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	cfg, err := Update(file, Updates{"meeting": {"mode": "meeting", "summary_endpoint": "http://localhost:1234/v1",
		"summary_model": "local-large", "summary_api_key_env": "VOICE_IO_TEST_KEY"}})
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Meeting
	if m.Mode != ModeMeeting || m.SummaryEndpoint != "http://localhost:1234/v1" || m.SummaryModel != "local-large" ||
		m.SummaryAPIKeyEnv != "VOICE_IO_TEST_KEY" {
		t.Fatalf("meeting %+v", m)
	}
	if cfg, err = Update(file, Updates{"meeting": {"mode": "collaborative"}}); err != nil || cfg.Meeting.Mode != ModeCollaborative {
		t.Fatalf("collaborative: %v %v", err, cfg)
	}
	if Default().Meeting.Mode != ModeDictation {
		t.Fatalf("default mode %q", Default().Meeting.Mode)
	}
}

func TestLoadRejectsInvalidColorsAndUnboundedSessions(t *testing.T) {
	for _, text := range []string{
		"[ui]\npreview_color = \"not a hex color\"\n",
		"[dictation]\nsession_max_s = 0\n",
		"[dictation]\nsession_max_s = inf\n",
		"[dictation]\nrecovery_ttl = -5\n",
		"[turn]\nmax_sec = -1\n",
		"[vad]\nthreshold = 1.5\n",
	} {
		if _, err := Parse(text); err == nil {
			t.Errorf("Parse(%q) accepted", text)
		}
	}
	cfg, err := Parse("[dictation]\nsession_max_s = 7200\n")
	if err != nil || cfg.Dictation.SessionMaxS != 7200 {
		t.Fatalf("7200: %v %v", err, cfg)
	}
}

func TestUnknownSettingsWarnInsteadOfFailing(t *testing.T) {
	cfg, err := Parse("[ui]\npill = true\nsounds = false\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UI.Sounds || len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "ui.pill") {
		t.Fatalf("sounds=%v warnings=%q", cfg.UI.Sounds, cfg.Warnings)
	}
}

func TestResolveKeepsModelPathsInsideTheModelRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	cfg := Default()
	got, err := cfg.Resolve("models/final/tokens.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(paths.ModelsDir(), "final", "tokens.txt"); got != want {
		t.Fatalf("Resolve = %q; want %q", got, want)
	}
	for _, bad := range []string{"../models/final", "models/../../secret", "models", `models\final`, ""} {
		if p, err := cfg.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) = %q; want error", bad, p)
		}
	}
	if p, err := cfg.Resolve("/opt/models/final"); err != nil || p != "/opt/models/final" {
		t.Errorf("absolute path: %q %v", p, err)
	}
}
