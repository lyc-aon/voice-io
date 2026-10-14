package app

import (
	"fmt"
	"runtime"
	"strings"

	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/daemon"
	"gitlab.com/integral-inference/voice-io/internal/overlay"
	"gitlab.com/integral-inference/voice-io/internal/platform"
	"gitlab.com/integral-inference/voice-io/internal/ui"
)

var modeTitles = []struct{ mode, title string }{
	{config.ModeDictation, "Dictation"},
	{config.ModeMeeting, "Meeting notes"},
	{config.ModeCollaborative, "Collaborative draft"},
}

// keyEquivalent renders a shortcut as a native menu key equivalent.
func keyEquivalent(binding string) (string, int) {
	hk, err := config.ParseHotkey(binding)
	if err != nil {
		return "", 0
	}
	key := hk.Key
	switch {
	case key == "space":
		key = " "
	case len(key) > 1:
		return "", 0 // named keys are shown in Settings instead
	}
	mods := 0
	for _, m := range []struct {
		mod config.Mod
		bit int
	}{{config.ModCtrl, 1}, {config.ModAlt, 2}, {config.ModShift, 4}, {config.ModSuper, 8}} {
		if hk.Mods&m.mod != 0 {
			mods |= m.bit
		}
	}
	return key, mods
}

func (a *App) statusMenu() []ui.MenuItem {
	cfg := a.cfg()
	st := a.daemon.Status()
	info := a.statusInfo()
	header := "Voice IO — Ready"
	switch {
	case info.Blocking:
		header = "Voice IO — Setup needed"
	case !st.Enabled:
		header = "Voice IO — Off"
	case st.State == daemon.Listening && st.Mode != config.ModeDictation:
		header = "Voice IO — Recording meeting"
	case st.State == daemon.Listening:
		header = "Voice IO — Listening"
	case st.State == daemon.Parsing:
		header = "Voice IO — Transcribing"
	}
	items := []ui.MenuItem{{Title: header, Header: true}}
	pushKey, pushMods := keyEquivalent(cfg.Hotkeys.Push)
	cancelKey, cancelMods := keyEquivalent(cfg.Hotkeys.Cancel)
	toggleKey, toggleMods := keyEquivalent(cfg.Hotkeys.Toggle)
	switch st.State {
	case daemon.Listening:
		title := "Finish dictation"
		if st.Mode != config.ModeDictation {
			title = "Stop and save notes"
		}
		items = append(items,
			ui.MenuItem{Title: title, Action: "push", Key: pushKey, Mods: pushMods},
			ui.MenuItem{Title: "Cancel", Action: "cancel", Key: cancelKey, Mods: cancelMods})
	case daemon.Parsing:
		items = append(items,
			ui.MenuItem{Title: "Transcribing…", Disabled: true},
			ui.MenuItem{Title: "Cancel", Action: "cancel", Key: cancelKey, Mods: cancelMods})
	default:
		dictate := ui.MenuItem{Title: "Dictate", Action: "dictate", Key: pushKey, Mods: pushMods,
			Disabled: info.Blocking || !st.Enabled}
		if runtime.GOOS == "darwin" && cfg.Hotkeys.FnHold {
			dictate.Subtitle = "Hold fn to talk"
		}
		items = append(items, dictate,
			ui.MenuItem{Title: "Start meeting notes", Action: "meeting", Disabled: info.Blocking || !st.Enabled})
	}
	items = append(items, ui.Separator)
	var modes []ui.MenuItem
	for _, m := range modeTitles {
		modes = append(modes, ui.MenuItem{Title: m.title, Action: "mode:" + m.mode, Checked: cfg.Meeting.Mode == m.mode})
	}
	items = append(items,
		ui.MenuItem{Title: "Shortcut records", Items: modes},
		ui.MenuItem{Title: fmt.Sprintf("Listen for “%s”", cfg.KWS.WakePhrase), Action: "toggle-wake", Checked: cfg.KWS.Enabled},
		ui.MenuItem{Title: "Voice input on", Action: "toggle", Checked: st.Enabled, Key: toggleKey, Mods: toggleMods},
		ui.Separator)
	_, hasRecovery := a.recovery.Latest()
	items = append(items, ui.MenuItem{Title: "Copy last transcript", Action: "recover", Disabled: !hasRecovery})
	if exists(a.meetingsDir()) {
		items = append(items, ui.MenuItem{Title: "Open meeting notes", Action: "open-meetings"})
	}
	items = append(items, ui.Separator,
		ui.MenuItem{Title: "Settings…", Action: "settings", Key: ",", Mods: 8},
		ui.MenuItem{Title: "Quit Voice IO", Action: "quit"})
	return items
}

func (a *App) overlayMenu() []ui.MenuItem {
	all := a.statusMenu()
	out := make([]ui.MenuItem, 0, len(all))
	for _, it := range all[1:] { // the header repeats what the pill shows
		if it.Action == "open-meetings" {
			continue
		}
		out = append(out, it)
	}
	return out
}

// menuAction runs on the UI thread; anything slow moves to a goroutine.
func (a *App) menuAction(action string) {
	switch {
	case action == "push" || action == "dictate" || action == "meeting" || action == "cancel" || action == "toggle":
		a.daemon.Send(action)
	case strings.HasPrefix(action, "mode:"):
		mode := strings.TrimPrefix(action, "mode:")
		go a.saveSetting(config.Updates{"meeting": {"mode": mode}})
	case action == "toggle-wake":
		go a.saveSetting(config.Updates{"kws": {"enabled": !a.cfg().KWS.Enabled}})
	case action == "recover":
		go func() {
			if err := a.copyLatest(); err != nil {
				a.model.Result(overlay.Result{Text: err.Error(), Style: "warn", AutoClose: 3, ShowText: true})
				return
			}
			a.model.Result(overlay.Result{Text: "Transcript copied", Style: "ok", AutoClose: 2, ShowText: true})
		}()
	case action == "open-meetings":
		go func() { _ = platform.OpenPath(a.meetingsDir()) }()
	case action == "settings":
		a.openSettings("")
	case action == "quit":
		go a.Quit()
	}
}

func (a *App) saveSetting(u config.Updates) {
	if _, err := a.updateConfig(u); err != nil {
		a.log.Printf("setting not saved: %v", err)
		a.model.Result(overlay.Result{Text: "Setting not saved — see Settings", Style: "error", AutoClose: 3, ShowText: true})
	}
}

// ---- shortcuts --------------------------------------------------------------

// applyShortcuts registers the configured global shortcuts (macOS) and the
// fn hold monitor. On GNOME, shortcuts are applied explicitly from Settings.
func (a *App) applyShortcuts() {
	if a.opts.NoUI || !platform.HotkeysNeedRegistration {
		return
	}
	cfg := a.cfg()
	keys, err := cfg.Hotkeys.Parse()
	if err == nil {
		err = platform.RegisterHotkeys(keys, a.keys.hotkey)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
		a.log.Printf("shortcuts: %v", err)
	}
	a.shortcutErr.Store(&msg)
	if platform.FnSupported && cfg.Hotkeys.FnHold {
		platform.StartFnMonitor(a.keys.fn)
	} else {
		platform.StopFnMonitor()
	}
}

// shortcutError is the last registration failure ("" when every shortcut works).
func (a *App) shortcutError() string {
	if p := a.shortcutErr.Load(); p != nil {
		return *p
	}
	return ""
}

// pauseShortcuts stops global shortcuts while Settings records a new one.
func (a *App) pauseShortcuts(paused bool) {
	a.keys.setPaused(paused)
	if paused {
		platform.UnregisterHotkeys()
	} else {
		a.applyShortcuts()
	}
}
