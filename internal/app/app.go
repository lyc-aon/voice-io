// Package app wires the daemon, native UI, control socket, shortcuts and
// the Settings server into the running voice-io application.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/control"
	"gitlab.com/integral-inference/voice-io/internal/daemon"
	"gitlab.com/integral-inference/voice-io/internal/history"
	"gitlab.com/integral-inference/voice-io/internal/insert"
	"gitlab.com/integral-inference/voice-io/internal/overlay"
	"gitlab.com/integral-inference/voice-io/internal/paths"
	"gitlab.com/integral-inference/voice-io/internal/platform"
	"gitlab.com/integral-inference/voice-io/internal/recovery"
	"gitlab.com/integral-inference/voice-io/internal/ui"
	"gitlab.com/integral-inference/voice-io/internal/version"
)

// Options configures one run of the app.
type Options struct {
	DryRun       bool          // log instead of pasting and playing sounds
	Input        string        // replay a 16 kHz mono WAV instead of the microphone
	Realtime     bool          // pace Input at wall-clock speed
	NoUI         bool          // no overlay, menu bar item or Settings window
	Duration     time.Duration // stop after this long (0 = until quit)
	RecordPath   string        // record the next session's audio (diagnostics)
	OpenSettings bool          // open Settings once running
}

// App is the running application.
type App struct {
	opts      Options
	conf      atomic.Pointer[config.Config]
	configErr atomic.Pointer[error] // config.toml parse error; nil when valid
	log       *log.Logger
	model     *overlay.Model
	daemon    *daemon.Daemon
	control   *control.Server
	recovery  *recovery.Store
	history   *history.Log
	events    *hub
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	quitOnce  sync.Once

	settingsMu sync.Mutex
	settings   *settingsServer

	keys        keyGestures
	shortcutErr atomic.Pointer[string]

	installMu  sync.Mutex
	installing context.CancelFunc
}

func (a *App) cfg() *config.Config { return a.conf.Load() }

// OverlayConfig maps user settings onto the overlay.
func OverlayConfig(cfg *config.Config) overlay.Config {
	shortcut := ""
	if runtime.GOOS == "darwin" && cfg.Hotkeys.FnHold {
		shortcut = "fn"
	} else if hk, err := cfg.Hotkeys.Parse(); err == nil {
		shortcut = hk.Push.Display(runtime.GOOS)
	}
	return overlay.Config{
		Enabled:      cfg.Overlay.Enabled,
		Theme:        cfg.UI.Theme,
		Accent:       cfg.UI.Accent,
		PreviewColor: cfg.UI.PreviewColor,
		IdleHandle:   cfg.UI.IdleHandle,
		LivePreview:  cfg.UI.LivePreview,
		Shortcut:     shortcut,
		OffsetY:      loadOffset(),
	}
}

type overlayState struct {
	OffsetY float64 `json:"offset_y"`
}

func loadOffset() float64 {
	var s overlayState
	if data, err := os.ReadFile(paths.OverlayStateFile()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s.OffsetY
}

func saveOffset(y float64) error {
	data, _ := json.Marshal(overlayState{OffsetY: y})
	if err := os.MkdirAll(paths.ConfigDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(paths.OverlayStateFile(), data, 0o600)
}

// dryFocus reports a fixed target so dry runs exercise the insert path.
type dryFocus struct{}

func (dryFocus) Probe() string                  { return "dry-run|0|0" }
func (dryFocus) Chord(string) string            { return "ctrl+v" }
func (dryFocus) PasteBlocked() (string, string) { return "", "" }
func (dryFocus) PasteHint() string              { return "Ctrl+V" }

// inserter applies the current auto-submit setting to each paste.
type inserter struct {
	app *App
}

func (i inserter) executor() *insert.Executor {
	return &insert.Executor{Backend: platform.NewBackend(), DryRun: i.app.opts.DryRun,
		AutoSubmit: i.app.cfg().Exec.AutoSubmit}
}

func (i inserter) Dictate(text string, r insert.Request) insert.Result {
	return i.executor().Dictate(text, r)
}

func (i inserter) CopyOnly(text string) insert.Result { return i.executor().CopyOnly(text) }

// Run starts the app and blocks until it quits. With a native UI it must be
// called on the main thread.
func Run(opts Options) error {
	a := &App{opts: opts, log: log.New(os.Stderr, "", log.LstdFlags), events: newHub(), done: make(chan struct{})}
	cfg, err := config.Load("")
	if err != nil {
		a.log.Printf("settings file error (using defaults): %v", err)
		a.configErr.Store(&err)
		cfg = config.Default()
	}
	for _, w := range cfg.Warnings {
		a.log.Printf("config: %s", w)
	}
	a.conf.Store(cfg)
	if a.recovery, err = recovery.Open(cfg.RecoveryPath(), time.Duration(cfg.Dictation.RecoveryTTL*float64(time.Second))); err != nil {
		return err
	}
	a.history = &history.Log{Path: cfg.HistoryPath()}
	withUI := !opts.NoUI && ui.Supported()
	var measure overlay.Measurer = overlay.ApproxMeasurer{}
	if withUI {
		measure = ui.Measurer{}
	}
	a.model = overlay.New(measure, nil)
	a.model.SetConfig(OverlayConfig(cfg))
	a.model.SetHooks(ui.RequestFrame, a.onOverlayAction)
	var sink daemon.Overlay = overlaySink{app: a, model: a.model}
	if !withUI {
		sink = logSink{log: a.log}
	}
	var focus daemon.Focus = platform.Focus{}
	if opts.DryRun {
		focus = dryFocus{}
	}
	open := func(c *config.Config) (audio.Source, error) { return audio.OpenMic(c.Audio.Device) }
	if opts.Input != "" {
		open = func(*config.Config) (audio.Source, error) {
			return audio.OpenFile(opts.Input, opts.Realtime, 1500*time.Millisecond)
		}
	}
	a.daemon = daemon.New(daemon.Options{
		Config: cfg, Overlay: sink, Cues: &cues{app: a, dryRun: opts.DryRun}, Focus: focus,
		Inserter: inserter{app: a}, Recovery: a.recovery, History: a.history, Log: a.log,
		BuildEngines: daemon.BuildEngines, OpenSource: open, PersistentSource: opts.Input != "",
		RecordPath: opts.RecordPath, OnStatus: a.statusChanged,
	})
	a.keys = keyGestures{send: a.daemon.Send,
		idle:       func() bool { return a.daemon.Status().State == daemon.Idle },
		holdToTalk: func() bool { return a.cfg().Hotkeys.HoldToTalk }}
	if opts.Input == "" {
		if a.control, err = control.Listen(a.handleControl); err != nil {
			a.daemon.Close()
			return err
		}
		defer a.control.Close()
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			a.Quit()
		case <-a.done:
		}
	}()
	if opts.Duration > 0 {
		time.AfterFunc(opts.Duration, a.Quit)
	}
	if !withUI {
		if opts.OpenSettings {
			a.openSettings("")
		}
		a.runDaemon()
		return nil
	}
	ui.Run(ui.Hooks{
		Model:          a.model,
		Ready:          a.uiReady,
		StatusMenu:     a.statusMenu,
		MenuAction:     a.menuAction,
		SettingsClosed: a.settingsClosed,
		Terminate: func() {
			a.Quit()
			<-a.done
			ui.ReplyTerminate()
		},
	}, ui.Options{Overlay: cfg.Overlay.Enabled, StatusItem: true})
	<-a.done
	return nil
}

func (a *App) runDaemon() {
	defer close(a.done)
	if err := a.daemon.Run(a.ctx); err != nil {
		a.log.Printf("daemon stopped: %v", err)
	}
	a.daemon.Close()
	a.stopSettings()
}

// uiReady runs on the main thread once AppKit/GTK is running.
func (a *App) uiReady() {
	platform.SetMainLoopRunning(true)
	a.applyShortcuts()
	go func() {
		a.runDaemon()
		ui.Quit()
	}()
	go func() {
		time.Sleep(1500 * time.Millisecond)
		a.statusChanged()
		if a.opts.OpenSettings || a.needsSetup() {
			a.openSettings("")
		}
		if a.cfg().Exec.AutoPaste && missingGrant(platform.Permissions(), "accessibility") {
			// Pasting at the cursor is the default; ask for the grant up front.
			_ = platform.RequestPermission("accessibility")
		}
		a.watchPermissions()
	}()
}

func missingGrant(perms []platform.Permission, id string) bool {
	for _, p := range perms {
		if p.ID == id {
			return p.State != "granted"
		}
	}
	return false
}

// watchPermissions notices grants made in System Settings while running:
// Settings and the menus update, and the fn monitor is re-armed because
// macOS only delivers key events to monitors installed after the grant.
func (a *App) watchPermissions() {
	missing := func() string {
		var ids []string
		for _, p := range platform.Permissions() {
			if p.Required && p.State != "granted" {
				ids = append(ids, p.ID)
			}
		}
		return strings.Join(ids, ",")
	}
	last := missing()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for last != "" {
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
		}
		now := missing()
		if now == last {
			continue
		}
		last = now
		platform.StopFnMonitor()
		a.applyShortcuts()
		a.statusChanged()
	}
}

// Quit stops the daemon (finishing accepted work) and the UI.
func (a *App) Quit() {
	a.quitOnce.Do(func() {
		a.daemon.Send("quit")
		a.cancel()
	})
}

// handleControl serves the control socket.
func (a *App) handleControl(req control.Request) (any, error) {
	switch req.Cmd {
	case "ping":
		return nil, nil
	case "status":
		return a.statusInfo(), nil
	case "push", "toggle", "cancel", "ptt", "ptt_stop", "ptt-stop", "dictate", "meeting", "collaborative", "identify":
		cmd := req.Cmd
		if cmd == "ptt-stop" {
			cmd = "ptt_stop"
		}
		a.daemon.Send(cmd)
		return nil, nil
	case "settings":
		a.openSettings(req.Arg)
		return nil, nil
	case "recover":
		return nil, a.copyLatest()
	case "reload":
		return nil, a.reloadConfig()
	case "quit":
		go a.Quit()
		return nil, nil
	}
	return nil, fmt.Errorf("unknown command %q", req.Cmd)
}

// statusChanged refreshes the menu bar icon, setup hint and Settings.
func (a *App) statusChanged() {
	if a.daemon == nil {
		return
	}
	st := a.statusInfo()
	if !a.opts.NoUI && ui.Supported() {
		icon := "idle"
		switch {
		case !st.Daemon.Enabled:
			icon = "off"
		case st.Daemon.State == daemon.Listening:
			icon = "recording"
		case st.Daemon.State == daemon.Parsing:
			icon = "processing"
		case st.Blocking:
			icon = "setup"
		}
		ui.SetStatus(icon)
	}
	hint := ""
	if st.Blocking {
		hint = "Finish setup to start dictating"
	}
	a.model.SetSetup(hint)
	a.events.publish(Event{Type: "status", Data: st})
}

// StatusInfo is the full status shown in Settings and `voice-io status`.
type StatusInfo struct {
	Daemon   daemon.Status         `json:"daemon"`
	Issues   []daemon.Issue        `json:"issues"`
	Blocking bool                  `json:"blocking"`
	Version  string                `json:"version"`
	Platform string                `json:"platform"`
	Shortcut string                `json:"shortcut"`
	FnHold   bool                  `json:"fn_hold"`
	Mic      string                `json:"microphone_error,omitempty"`
	Perms    []platform.Permission `json:"permissions"`
}

func (a *App) statusInfo() StatusInfo {
	st := StatusInfo{Daemon: a.daemon.Status(), Version: version.Version, Platform: runtime.GOOS,
		FnHold: runtime.GOOS == "darwin" && a.cfg().Hotkeys.FnHold}
	if hk, err := a.cfg().Hotkeys.Parse(); err == nil {
		st.Shortcut = hk.Push.Display(runtime.GOOS)
	}
	st.Perms = platform.Permissions()
	st.Issues, st.Blocking = a.issues(st.Perms)
	if err := a.daemon.SourceError(); err != nil {
		st.Mic = err.Error()
	}
	return st
}

// issues lists what the user must fix; blocking issues stop dictation.
func (a *App) issues(perms []platform.Permission) ([]daemon.Issue, bool) {
	var out []daemon.Issue
	blocking := false
	if err := a.configError(); err != nil {
		out = append(out, daemon.Issue{ID: "config", Message: "config.toml has an error; defaults are in use: " + err.Error()})
	}
	if err := a.daemon.EngineError(); err != nil {
		out = append(out, daemon.Issue{ID: "models", Message: "Download the speech models to start dictating."})
		blocking = true
	}
	for _, p := range perms {
		if p.Required && p.State != "granted" {
			out = append(out, daemon.Issue{ID: p.ID, Message: p.Title + ": " + p.Detail})
			if p.ID == "microphone" {
				blocking = true
			}
		}
	}
	return out, blocking
}

func (a *App) needsSetup() bool {
	_, blocking := a.issues(platform.Permissions())
	return blocking
}

// reloadConfig re-reads config.toml and applies it everywhere.
func (a *App) reloadConfig() error {
	cfg, err := config.Load("")
	if err != nil {
		a.configErr.Store(&err)
		a.statusChanged()
		return err
	}
	a.configErr.Store(nil)
	a.applyConfig(cfg)
	return nil
}

func (a *App) applyConfig(cfg *config.Config) {
	prev := a.cfg()
	a.conf.Store(cfg)
	a.history.SetPath(cfg.HistoryPath())
	oc := OverlayConfig(cfg)
	oc.OffsetY = a.model.OffsetY()
	a.model.SetConfig(oc)
	if prev.Overlay.Enabled != cfg.Overlay.Enabled && !a.opts.NoUI && ui.Supported() {
		ui.SetOverlayEnabled(cfg.Overlay.Enabled)
	}
	if prev.Hotkeys != cfg.Hotkeys {
		a.applyShortcuts()
	}
	a.daemon.Reconfigure(cfg)
	a.events.publish(Event{Type: "config", Data: configMap(cfg)})
	a.statusChanged()
}

// updateConfig persists edits and applies them.
func (a *App) updateConfig(u config.Updates) (*config.Config, error) {
	cfg, err := config.Update("", u)
	if err != nil {
		return nil, err
	}
	a.configErr.Store(nil)
	a.applyConfig(cfg)
	return cfg, nil
}

// configError reports the last config.toml parse error, if any.
func (a *App) configError() error {
	if p := a.configErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (a *App) onOverlayAction(act overlay.Action) {
	switch act.Kind {
	case "push", "dictate", "meeting", "cancel", "toggle":
		a.daemon.Send(act.Kind)
	case "settings":
		a.openSettings("")
	case "reveal":
		go func() { _ = platform.OpenPath(act.Arg) }()
	case "permission":
		go func() { _ = platform.RequestPermission(act.Arg) }()
	case "menu":
		ui.ShowMenu(a.overlayMenu(), act.X, act.Y)
	case "position":
		if err := saveOffset(act.N); err != nil {
			a.log.Printf("overlay position not saved: %v", err)
		}
	}
}

// copyLatest copies the newest saved transcript for a manual paste.
func (a *App) copyLatest() error {
	e, ok := a.recovery.Latest()
	if !ok || e.Text == "" {
		return errors.New("no saved transcript to recover")
	}
	res := inserter{app: a}.CopyOnly(e.Text)
	if !res.OK {
		return errors.New(res.Message)
	}
	return nil
}

func (a *App) meetingsDir() string { return paths.MeetingsDir() }

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (a *App) executable() string {
	exe, err := os.Executable()
	if err != nil {
		return "voice-io"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}
