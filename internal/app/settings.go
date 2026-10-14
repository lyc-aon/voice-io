package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/control"
	"gitlab.com/integral-inference/voice-io/internal/llm"
	"gitlab.com/integral-inference/voice-io/internal/meeting"
	"gitlab.com/integral-inference/voice-io/internal/models"
	"gitlab.com/integral-inference/voice-io/internal/overlay"
	"gitlab.com/integral-inference/voice-io/internal/paths"
	"gitlab.com/integral-inference/voice-io/internal/platform"
	"gitlab.com/integral-inference/voice-io/internal/speech"
	"gitlab.com/integral-inference/voice-io/internal/text"
	"gitlab.com/integral-inference/voice-io/internal/ui"
	"gitlab.com/integral-inference/voice-io/internal/version"
)

//go:embed web
var webFS embed.FS

// settingsServer serves the Settings UI on a private loopback port. Every
// API request needs the per-run token; the Host header must be loopback.
type settingsServer struct {
	srv   *http.Server
	url   string
	token string
	host  string
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// openSettings starts the server if needed and shows the window (macOS) or
// browser tab (Linux). page selects a section, e.g. "models".
func (a *App) openSettings(page string) {
	s, err := a.ensureSettings()
	if err != nil {
		a.log.Printf("settings unavailable: %v", err)
		return
	}
	if a.opts.NoUI {
		// Headless runs never open windows; print the address instead.
		a.log.Printf("settings: %s#page=%s&token=%s", s.url, page, s.token)
		return
	}
	if ui.Supported() {
		// A native window (macOS) or the browser (Linux); the fragment picks
		// a page and carries the token where the window cannot inject it.
		ui.OpenSettings(s.url+"#page="+page, s.token)
		return
	}
	// Fragments never reach the server or its logs.
	if err := platform.OpenURL(s.url + "#page=" + page + "&token=" + s.token); err != nil {
		a.log.Printf("cannot open browser: %v", err)
	}
}

func controlRequest(cmd string) control.Request { return control.Request{Cmd: cmd} }

func (a *App) ensureSettings() (*settingsServer, error) {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	if a.settings != nil {
		return a.settings, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &settingsServer{token: randomToken(), host: ln.Addr().String()}
	s.url = "http://" + s.host + "/"
	s.srv = &http.Server{Handler: a.routes(s), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	a.settings = s
	return s, nil
}

func (a *App) settingsClosed() {
	// Shortcuts paused for recording must come back if the window closes.
	if a.keys.isPaused() {
		a.pauseShortcuts(false)
	}
	if runtime.GOOS == "darwin" {
		go func() {
			time.Sleep(3 * time.Second)
			a.stopSettings()
		}()
	}
}

func (a *App) stopSettings() {
	a.settingsMu.Lock()
	s := a.settings
	a.settings = nil
	a.settingsMu.Unlock()
	if s != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	}
}

type apiError struct {
	Error string `json:"error"`
	Field string `json:"field,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, apiError{Error: err.Error()})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (a *App) routes(s *settingsServer) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	}))
	api := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.guard(h))
	}
	api("GET /api/bootstrap", a.apiBootstrap)
	api("GET /api/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.statusInfo()) })
	api("PATCH /api/config", a.apiUpdateConfig)
	api("POST /api/validate/phrases", a.apiValidatePhrases)
	api("GET /api/devices", a.apiDevices)
	api("GET /api/models", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.modelStatus()) })
	api("POST /api/models/install", a.apiInstallModels)
	api("POST /api/models/cancel", a.apiCancelModels)
	api("GET /api/history", a.apiHistory)
	api("DELETE /api/history/{key}", a.apiDeleteHistory)
	api("DELETE /api/history", a.apiClearHistory)
	api("POST /api/copy", a.apiCopy)
	api("GET /api/recovery", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, nonNil(a.recovery.List())) })
	api("DELETE /api/recovery/{id}", a.apiDropRecovery)
	api("GET /api/meetings", a.apiMeetings)
	api("POST /api/meetings/{name}/open", a.apiOpenMeeting)
	api("DELETE /api/meetings/{name}", a.apiDeleteMeeting)
	api("GET /api/maps/{kind}", a.apiGetMap)
	api("PUT /api/maps/{kind}", a.apiPutMap)
	api("POST /api/permissions/{id}/request", a.apiRequestPermission)
	api("GET /api/login", a.apiLogin)
	api("PUT /api/login", a.apiSetLogin)
	api("POST /api/llm/models", a.apiLLMModels)
	api("POST /api/llm/test", a.apiLLMTest)
	api("GET /api/overlay/{state}", a.apiOverlayPreview)
	api("POST /api/reveal", a.apiReveal)
	api("POST /api/shortcuts/pause", a.apiPauseShortcuts)
	api("POST /api/shortcuts/apply", a.apiApplyGNOME)
	api("POST /api/control", a.apiControl)
	api("POST /api/overlay/reset-position", a.apiResetPosition)
	api("POST /api/mic/test", a.apiMicTest)
	api("GET /api/events", a.apiEvents)
	return mux
}

// guard enforces the token and a loopback Host (no DNS rebinding).
func (s *settingsServer) guard(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.host {
			fail(w, http.StatusForbidden, errors.New("bad host"))
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+s.host {
			fail(w, http.StatusForbidden, errors.New("bad origin"))
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			fail(w, http.StatusUnauthorized, errors.New("missing or wrong token"))
			return
		}
		next(w, r)
	})
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// ---- handlers ---------------------------------------------------------------

type modelInfo struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	Group      string `json:"group"`
	Installed  bool   `json:"installed"`
	Size       int64  `json:"size"`
	License    string `json:"license"`
	LicenseURL string `json:"license_url"`
	Path       string `json:"path"`
}

func (a *App) modelStatus() map[string]any {
	var list []modelInfo
	for _, s := range models.Inspect(paths.ModelsDir()) {
		list = append(list, modelInfo{ID: s.Model.ID, Title: s.Model.Title, Detail: s.Model.Detail,
			Group: string(s.Model.Group), Installed: s.Installed, Size: s.Model.DownloadSize(),
			License: s.Model.License, LicenseURL: s.Model.LicenseURL, Path: s.Path})
	}
	a.installMu.Lock()
	busy := a.installing != nil
	a.installMu.Unlock()
	return map[string]any{"models": list, "installing": busy, "dir": paths.ModelsDir()}
}

func (a *App) apiBootstrap(w http.ResponseWriter, r *http.Request) {
	cfg := a.cfg()
	login, loginOn, approval := platform.LoginItem()
	cerr := ""
	if err := a.configError(); err != nil {
		cerr = err.Error()
	}
	writeJSON(w, 200, map[string]any{
		"version":        version.Version,
		"platform":       runtime.GOOS,
		"config":         configMap(cfg),
		"defaults":       configMap(config.Default()),
		"config_error":   cerr,
		"status":         a.statusInfo(),
		"models":         a.modelStatus(),
		"login":          map[string]bool{"supported": login, "enabled": loginOn, "approval": approval},
		"shortcut_error": a.shortcutError(),
		"fn_key_action":  platform.FnKeyAction(),
		"paths": map[string]string{
			"config": paths.ConfigFile(), "data": paths.DataDir(), "logs": paths.LogFile(),
			"models": paths.ModelsDir(), "meetings": paths.MeetingsDir(), "history": cfg.HistoryPath(),
			"vocabulary": cfg.VocabularyPath(), "snippets": cfg.SnippetsPath(),
		},
		"scenarios": scenarioNames(),
	})
}

func scenarioNames() []map[string]string {
	var out []map[string]string
	for _, s := range overlay.Scenarios() {
		out = append(out, map[string]string{"name": s.Name, "title": s.Title})
	}
	return out
}

func (a *App) apiUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var u config.Updates
	if err := decode(r, &u); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if kws, ok := u["kws"]; ok {
		cur := a.cfg()
		wake, end := cur.KWS.WakePhrase, cur.KWS.EndPhrase
		if v, ok := kws["wake_phrase"].(string); ok {
			wake = v
		}
		if v, ok := kws["end_phrase"].(string); ok {
			end = v
		}
		if err := a.validatePhrases(wake, end); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError{Error: err.Error(), Field: "kws"})
			return
		}
	}
	cfg, err := a.updateConfig(u)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, 200, map[string]any{"config": configMap(cfg), "status": a.statusInfo(), "shortcut_error": a.shortcutError()})
}

func (a *App) validatePhrases(wake, end string) error {
	dir, err := a.cfg().Resolve(a.cfg().KWS.ModelDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "en.phone")); err != nil {
		if strings.TrimSpace(wake) == "" || strings.TrimSpace(end) == "" {
			return errors.New("phrases must not be empty")
		}
		return nil // checked against the lexicon once models are installed
	}
	_, err = speech.KeywordLines(dir, wake, end)
	return err
}

func (a *App) apiValidatePhrases(w http.ResponseWriter, r *http.Request) {
	var req struct{ Wake, End string }
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := a.validatePhrases(req.Wake, req.End); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) apiDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := audio.InputDevices()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, nonNil(devs))
}

func (a *App) apiInstallModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Group string `json:"group"` // core | meeting | all
	}
	_ = decode(r, &req)
	groups := []models.Group{models.Core}
	switch req.Group {
	case "meeting":
		groups = []models.Group{models.Meeting}
	case "all":
		groups = []models.Group{models.Core, models.Meeting}
	}
	a.installMu.Lock()
	if a.installing != nil {
		a.installMu.Unlock()
		fail(w, http.StatusConflict, errors.New("a download is already running"))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.installing = cancel
	a.installMu.Unlock()
	go func() {
		in := &models.Installer{Root: paths.ModelsDir(), Progress: func(p models.Progress) {
			a.events.publish(Event{Type: "models", Data: p})
		}}
		_, err := in.Install(ctx, groups...)
		a.installMu.Lock()
		a.installing = nil
		a.installMu.Unlock()
		done := map[string]any{"phase": "done"}
		if err != nil {
			done = map[string]any{"phase": "error", "error": err.Error()}
		}
		a.events.publish(Event{Type: "models", Data: done})
		if err == nil {
			// New models load without restarting: re-apply the config.
			a.daemon.Reconfigure(a.cfg())
		}
		time.Sleep(500 * time.Millisecond)
		a.statusChanged()
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (a *App) apiCancelModels(w http.ResponseWriter, r *http.Request) {
	a.installMu.Lock()
	if a.installing != nil {
		a.installing()
	}
	a.installMu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type historyItem struct {
	Key  string  `json:"key"`
	TS   float64 `json:"ts"`
	Text string  `json:"text"`
	Mode string  `json:"mode"`
}

func (a *App) apiHistory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	entries, err := a.history.List(limit)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]historyItem, 0, len(entries))
	for _, e := range entries {
		out = append(out, historyItem{Key: strconv.FormatInt(e.Key(), 10), TS: e.TS, Text: e.Text, Mode: e.Mode})
	}
	writeJSON(w, 200, out)
}

func (a *App) apiDeleteHistory(w http.ResponseWriter, r *http.Request) {
	key, err := strconv.ParseInt(r.PathValue("key"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := a.history.Delete(key); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiClearHistory(w http.ResponseWriter, r *http.Request) {
	if err := a.history.Clear(); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// apiCopy puts text from History or Recovery on the clipboard.
func (a *App) apiCopy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := decode(r, &req); err != nil || strings.TrimSpace(req.Text) == "" {
		fail(w, http.StatusBadRequest, errors.New("nothing to copy"))
		return
	}
	res := inserter{app: a}.CopyOnly(req.Text)
	if !res.OK {
		fail(w, http.StatusInternalServerError, errors.New(res.Message))
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiDropRecovery(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	a.recovery.Drop(id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type meetingItem struct {
	Name     string  `json:"name"`
	Title    string  `json:"title"`
	Modified float64 `json:"modified"`
	Size     int64   `json:"size"`
	Summary  bool    `json:"summary"`
	Excerpt  string  `json:"excerpt"`
}

func (a *App) apiMeetings(w http.ResponseWriter, r *http.Request) {
	files, err := meeting.Notes(paths.MeetingsDir())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := []meetingItem{}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		data, _ := os.ReadFile(f)
		body := string(data)
		title := strings.TrimPrefix(strings.SplitN(body, "\n", 2)[0], "# ")
		excerpt := ""
		if i := strings.Index(body, "## Summary\n\n"); i >= 0 {
			excerpt = body[i+12:]
		} else if i := strings.Index(body, "## Transcript\n\n"); i >= 0 {
			excerpt = body[i+15:]
		}
		excerpt = transcriptStamp.ReplaceAllString(excerpt, "")
		if r := []rune(excerpt); len(r) > 280 {
			excerpt = string(r[:280]) + "…"
		}
		out = append(out, meetingItem{Name: filepath.Base(f), Title: title, Modified: float64(info.ModTime().Unix()),
			Size: info.Size(), Summary: strings.Contains(body, "## Summary"), Excerpt: strings.TrimSpace(excerpt)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified > out[j].Modified })
	writeJSON(w, 200, out)
}

// transcriptStamp matches the "[00:00:01–00:00:06] " prefix of transcript lines.
var transcriptStamp = regexp.MustCompile(`(?m)^\[\d{2}:\d{2}:\d{2}–\d{2}:\d{2}:\d{2}\] `)

func meetingPath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
		return "", errors.New("invalid meeting name")
	}
	p := filepath.Join(paths.MeetingsDir(), name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

func (a *App) apiOpenMeeting(w http.ResponseWriter, r *http.Request) {
	p, err := meetingPath(r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	if r.URL.Query().Get("reveal") == "1" {
		err = platform.Reveal(p)
	} else {
		err = platform.OpenPath(p)
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiDeleteMeeting(w http.ResponseWriter, r *http.Request) {
	p, err := meetingPath(r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err := os.Remove(p); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) mapPath(kind string) (string, error) {
	switch kind {
	case "vocabulary":
		return a.cfg().VocabularyPath(), nil
	case "snippets":
		return a.cfg().SnippetsPath(), nil
	}
	return "", errors.New("unknown list")
}

func (a *App) apiGetMap(w http.ResponseWriter, r *http.Request) {
	p, err := a.mapPath(r.PathValue("kind"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	m, err := text.LoadMap(p)
	if err != nil {
		writeJSON(w, 200, map[string]any{"entries": map[string]string{}, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"entries": m})
}

func (a *App) apiPutMap(w http.ResponseWriter, r *http.Request) {
	p, err := a.mapPath(r.PathValue("kind"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	var m map[string]string
	if err := decode(r, &m); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	clean := map[string]string{}
	for k, v := range m {
		if k = strings.TrimSpace(k); k != "" {
			clean[k] = v
		}
	}
	data, _ := json.MarshalIndent(clean, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, 200, map[string]any{"entries": clean})
}

func (a *App) apiRequestPermission(w http.ResponseWriter, r *http.Request) {
	if err := platform.RequestPermission(r.PathValue("id")); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	go func() {
		// Grants land asynchronously; refresh Settings as they do.
		for range 20 {
			time.Sleep(1500 * time.Millisecond)
			a.statusChanged()
		}
	}()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiLogin(w http.ResponseWriter, r *http.Request) {
	sup, on, approval := platform.LoginItem()
	writeJSON(w, 200, map[string]bool{"supported": sup, "enabled": on, "approval": approval})
}

func (a *App) apiSetLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := platform.SetLoginItem(req.Enabled); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	a.apiLogin(w, r)
}

type llmRequest struct {
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

func (a *App) apiLLMModels(w http.ResponseWriter, r *http.Request) {
	var req llmRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := config.ValidateEndpoint("endpoint", req.Endpoint); err != nil || req.Endpoint == "" {
		fail(w, http.StatusBadRequest, errors.New("enter an http(s) endpoint"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := (&llm.Client{Endpoint: req.Endpoint, APIKeyEnv: req.APIKeyEnv}).Models(ctx)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	ids := make([]string, 0, len(list))
	for _, m := range list {
		ids = append(ids, m.ID)
	}
	writeJSON(w, 200, ids)
}

func (a *App) apiLLMTest(w http.ResponseWriter, r *http.Request) {
	var req llmRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := config.ValidateEndpoint("endpoint", req.Endpoint); err != nil || req.Endpoint == "" {
		fail(w, http.StatusBadRequest, errors.New("enter an http(s) endpoint"))
		return
	}
	sample := "um so the the meeting is moved to uh thursday at 3 pm and Dana will send the agenda"
	system, user := text.PolishPrompt(sample)
	start := time.Now()
	out, err := (&llm.Client{Endpoint: req.Endpoint, Model: req.Model, APIKeyEnv: req.APIKeyEnv}).Complete(r.Context(),
		llm.Request{System: system, User: user, MaxTokens: 200, Temperature: 0.2, Timeout: 30 * time.Second})
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, 200, map[string]any{"input": sample, "output": text.PolishGuard(sample, out),
		"ms": time.Since(start).Milliseconds()})
}

// apiOverlayPreview renders an overlay state with the given appearance.
func (a *App) apiOverlayPreview(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("state"), ".png")
	var scenario *overlay.Scenario
	for _, s := range overlay.Scenarios() {
		if s.Name == name {
			scenario = &s
			break
		}
	}
	if scenario == nil {
		fail(w, http.StatusNotFound, errors.New("unknown state"))
		return
	}
	q := r.URL.Query()
	oc := OverlayConfig(a.cfg())
	oc.Enabled, oc.IdleHandle = true, true
	if v := q.Get("theme"); v != "" {
		oc.Theme = v
	}
	if q.Has("accent") {
		oc.Accent = q.Get("accent")
	}
	if q.Has("preview_color") {
		oc.PreviewColor = q.Get("preview_color")
	}
	dark := q.Get("dark") != "0"
	var list overlay.List
	overlay.RenderScenario(*scenario, oc, dark, ui.Measurer{}, &list)
	tmp, err := os.CreateTemp("", "voice-io-preview-*.png")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := ui.RenderPNG(&list, overlay.CanvasW, overlay.CanvasH, 2, overlay.Color{}, tmp.Name()); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (a *App) apiReveal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string `json:"target"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	cfg := a.cfg()
	target := map[string]string{
		"config": paths.ConfigFile(), "data": paths.DataDir(), "logs": paths.LogFile(),
		"models": paths.ModelsDir(), "meetings": paths.MeetingsDir(), "history": cfg.HistoryPath(),
		"keyboard": "x-apple.systempreferences:com.apple.Keyboard-Settings.extension",
	}[req.Target]
	var err error
	switch {
	case target == "":
		err = errors.New("unknown location")
	case strings.HasPrefix(target, "x-apple"):
		err = platform.OpenURL(target)
	case req.Target == "meetings" || req.Target == "models" || req.Target == "data":
		_ = os.MkdirAll(target, 0o700)
		err = platform.OpenPath(target)
	case !exists(target):
		err = fmt.Errorf("%s does not exist yet", target)
	default:
		err = platform.Reveal(target)
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiPauseShortcuts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paused bool `json:"paused"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	a.pauseShortcuts(req.Paused)
	writeJSON(w, 200, map[string]any{"ok": true, "shortcut_error": a.shortcutError()})
}

func (a *App) apiApplyGNOME(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "linux" {
		fail(w, http.StatusBadRequest, platform.ErrUnsupported)
		return
	}
	keys, err := a.cfg().Hotkeys.Parse()
	if err == nil {
		err = applyGNOME(keys, a.executable())
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiControl(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cmd string `json:"cmd"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if _, err := a.handleControl(controlRequest(req.Cmd)); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiResetPosition(w http.ResponseWriter, r *http.Request) {
	_ = os.Remove(paths.OverlayStateFile())
	oc := OverlayConfig(a.cfg())
	a.model.SetConfig(oc)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// apiMicTest streams input levels from a short private capture so the
// meter works even when the daemon's microphone is closed.
func (a *App) apiMicTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Device  string  `json:"device"`
		Seconds float64 `json:"seconds"`
	}
	_ = decode(r, &req)
	if req.Seconds <= 0 || req.Seconds > 15 {
		req.Seconds = 8
	}
	mic, err := audio.OpenMic(req.Device)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	go func() {
		defer mic.Close()
		block := make([]float32, audio.SampleRate/20)
		end := time.Now().Add(time.Duration(req.Seconds * float64(time.Second)))
		peak := 0.0
		for time.Now().Before(end) {
			if err := mic.Read(block); err != nil {
				a.events.publish(Event{Type: "mictest", Data: map[string]any{"error": err.Error()}})
				return
			}
			rms := audio.RMS(block)
			peak = max(peak, rms)
			a.events.publish(Event{Type: "mictest", Data: map[string]any{"level": audio.SpeechLevel(rms)}})
		}
		a.events.publish(Event{Type: "mictest", Data: map[string]any{"done": true, "peak": audio.SpeechLevel(peak)}})
	}()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) apiEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	ch, done := a.events.subscribe()
	defer done()
	send := func(e Event) bool {
		data, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	send(Event{Type: "status", Data: a.statusInfo()})
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !send(e) {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
