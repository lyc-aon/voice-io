// Package config loads, validates and edits ~/.config/voice-io/config.toml.
//
// TOML sections mirror the structs below (e.g. [vad] threshold = 0.5). Missing
// keys keep their defaults; unknown keys are reported as warnings. Editing is
// comment-preserving: only the requested keys are rewritten (see Update).
package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"gitlab.com/integral-inference/voice-io/internal/paths"
)

type VAD struct {
	Model              string  `toml:"model"`
	Threshold          float64 `toml:"threshold"`
	MinSilenceDuration float64 `toml:"min_silence_duration"` // ends a speech turn, not the session
	MaxSpeechDuration  float64 `toml:"max_speech_duration"`  // per-utterance ceiling
}

type KWS struct {
	// Enabled keeps the microphone open while idle to hear the wake phrase.
	// When false the microphone opens only for a session.
	Enabled    bool    `toml:"enabled"`
	ModelDir   string  `toml:"model_dir"`
	Chunk      string  `toml:"chunk"` // chunk-16 = 320 ms window, chunk-8 = 160 ms
	Score      float64 `toml:"score"`
	WakePhrase string  `toml:"wake_phrase"`
	EndPhrase  string  `toml:"end_phrase"`
	Threshold  float64 `toml:"threshold"`
}

type ASR struct {
	Streaming string `toml:"streaming"` // live preview model
	FinalEN   string `toml:"final_en"`  // authoritative per-turn re-decode
	Threads   int    `toml:"threads"`
}

type Audio struct {
	Device string `toml:"device"` // input device name; "" = system default
}

type LLM struct {
	Endpoint  string `toml:"endpoint"` // OpenAI-compatible root, /v1 or chat URL; "" = off
	Model     string `toml:"model"`
	APIKeyEnv string `toml:"api_key_env"` // environment variable NAME, never the key
}

type Exec struct {
	AutoPaste  bool   `toml:"auto_paste"`  // paste at the cursor when finished; false copies only
	AutoSubmit string `toml:"auto_submit"` // "" | "enter" | "ctrl+enter"
}

type Polish struct {
	Deterministic bool   `toml:"deterministic"` // custom words, fillers, spacing, emoji
	Filler        bool   `toml:"filler"`
	Emoji         bool   `toml:"emoji"`
	Enabled       bool   `toml:"enabled"`      // optional LLM polish when [llm] is set
	CustomWords   string `toml:"custom_words"` // JSON {"misheard": "correct"}; "" = vocabulary.json
	Snippets      string `toml:"snippets"`     // JSON {"keyword": "exact text"}; "" = snippets.json
}

type Turn struct {
	MaxSec    float64 `toml:"max_sec"`    // per-utterance decoder ceiling
	FrontDrop float64 `toml:"front_drop"` // wake-tail overlap dropped at stream open (s)
	TailS     float64 `toml:"tail_s"`     // streaming flush tail for the fallback transcript (s)
}

type Dictation struct {
	History     string  `toml:"history"`      // JSONL path; "" = data dir history.jsonl
	KeepHistory bool    `toml:"keep_history"` // record successful insertions
	RecoveryDir string  `toml:"recovery_dir"` // "" = data dir recovery/
	RecoveryTTL float64 `toml:"recovery_ttl"` // seconds a saved transcript stays recoverable
	SessionMaxS float64 `toml:"session_max_s"`
}

type Meeting struct {
	Mode              string `toml:"mode"` // dictation | meeting | collaborative
	SummaryEndpoint   string `toml:"summary_endpoint"`
	SummaryModel      string `toml:"summary_model"`
	SummaryAPIKeyEnv  string `toml:"summary_api_key_env"`
	SegmentationModel string `toml:"segmentation_model"`
	EmbeddingModel    string `toml:"embedding_model"`
}

type UI struct {
	LivePreview  bool   `toml:"live_preview"`  // transient last-words caption beside the pill
	PreviewColor string `toml:"preview_color"` // "" = theme default
	Sounds       bool   `toml:"sounds"`
	Theme        string `toml:"theme"`       // dark | light | auto
	Accent       string `toml:"accent"`      // "gradient" (colored waveform), "" (monochrome) or #RRGGBB
	IdleHandle   bool   `toml:"idle_handle"` // show the small resting pill when idle
}

type Hotkeys struct {
	Push       string `toml:"push"` // start / finish
	Toggle     string `toml:"toggle"`
	Cancel     string `toml:"cancel"`
	HoldToTalk bool   `toml:"hold_to_talk"` // macOS: holding push records until release
	FnHold     bool   `toml:"fn_hold"`      // macOS: hold the fn (🌐) key to dictate
}

type Overlay struct {
	Enabled bool `toml:"enabled"`
}

// Config is the complete voice-io configuration.
type Config struct {
	VAD       VAD       `toml:"vad"`
	KWS       KWS       `toml:"kws"`
	ASR       ASR       `toml:"asr"`
	Audio     Audio     `toml:"audio"`
	LLM       LLM       `toml:"llm"`
	Exec      Exec      `toml:"exec"`
	Polish    Polish    `toml:"polish"`
	Turn      Turn      `toml:"turn"`
	Dictation Dictation `toml:"dictation"`
	Meeting   Meeting   `toml:"meeting"`
	UI        UI        `toml:"ui"`
	Hotkeys   Hotkeys   `toml:"hotkeys"`
	Overlay   Overlay   `toml:"overlay"`

	// Warnings lists ignored unknown keys from the loaded file.
	Warnings []string `toml:"-"`
}

const (
	ModeDictation     = "dictation"
	ModeMeeting       = "meeting"
	ModeCollaborative = "collaborative"
)

// AccentGradient is the default ui.accent: a colored waveform.
const AccentGradient = "gradient"

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		VAD: VAD{Model: "models/silero_vad_v5.onnx", Threshold: 0.5,
			MinSilenceDuration: 0.5, MaxSpeechDuration: 20},
		KWS: KWS{Enabled: true, ModelDir: "models/sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20",
			Chunk: "chunk-16", Score: 1.0, WakePhrase: "hey voice", EndPhrase: "voice send",
			Threshold: 0.25},
		ASR: ASR{
			Streaming: "models/sherpa-onnx-x-asr-480ms-streaming-zipformer-transducer-zh-en-int8-2026-06-05",
			FinalEN:   "models/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8",
			Threads:   4},
		Polish:    Polish{Deterministic: true, Filler: true, Emoji: true, Enabled: true},
		Exec:      Exec{AutoPaste: true},
		Turn:      Turn{MaxSec: 20, FrontDrop: 0.1, TailS: 1.0},
		Dictation: Dictation{KeepHistory: true, RecoveryTTL: 86400, SessionMaxS: 3600},
		Meeting: Meeting{Mode: ModeDictation,
			SegmentationModel: "models/sherpa-onnx-pyannote-segmentation-3-0/model.int8.onnx",
			EmbeddingModel:    "models/nemo_en_titanet_small.onnx"},
		UI: UI{LivePreview: true, Sounds: true, Theme: "dark", Accent: AccentGradient, IdleHandle: true},
		Hotkeys: Hotkeys{Push: "ctrl+alt+space", Toggle: "ctrl+alt+t", Cancel: "ctrl+alt+x",
			HoldToTalk: true, FnHold: true},
		Overlay: Overlay{Enabled: true},
	}
}

// Load reads path (paths.ConfigFile() when empty). A missing file yields the
// defaults. Type errors and invalid values are returned as errors.
func Load(file string) (*Config, error) {
	if file == "" {
		file = paths.ConfigFile()
	}
	cfg := Default()
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := cfg.decode(string(data)); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return cfg, nil
}

// Parse decodes TOML text over the defaults and validates it.
func Parse(text string) (*Config, error) {
	cfg := Default()
	if err := cfg.decode(text); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) decode(text string) error {
	md, err := toml.Decode(text, c)
	if err != nil {
		return err
	}
	for _, key := range md.Undecoded() {
		c.Warnings = append(c.Warnings, fmt.Sprintf("ignored unknown setting %s", key.String()))
	}
	return c.Validate()
}

var (
	envName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

func positiveSeconds(name string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return fmt.Errorf("%s must be a finite positive number of seconds", name)
	}
	return nil
}

func inRange(name string, v, lo, hi float64) error {
	if math.IsNaN(v) || v < lo || v > hi {
		return fmt.Errorf("%s must be between %g and %g", name, lo, hi)
	}
	return nil
}

// ValidateEndpoint accepts an HTTP(S) base URL without credentials, query or fragment.
func ValidateEndpoint(name, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("%s must be an HTTP(S) base URL without credentials", name)
	}
	return nil
}

// ValidatePreviewColor accepts #RRGGBB or empty for the theme default.
func ValidatePreviewColor(v string) error {
	if v != "" && !hexColor.MatchString(v) {
		return errors.New("ui.preview_color must be #RRGGBB or empty for the default")
	}
	return nil
}

// Validate checks every value that could make the daemon misbehave.
func (c *Config) Validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(inRange("vad.threshold", c.VAD.Threshold, 0.01, 0.99))
	add(positiveSeconds("vad.min_silence_duration", c.VAD.MinSilenceDuration))
	add(positiveSeconds("vad.max_speech_duration", c.VAD.MaxSpeechDuration))
	if c.KWS.Chunk != "chunk-16" && c.KWS.Chunk != "chunk-8" {
		add(errors.New("kws.chunk must be chunk-16 or chunk-8"))
	}
	add(inRange("kws.score", c.KWS.Score, 0.01, 10))
	add(inRange("kws.threshold", c.KWS.Threshold, 0.01, 0.99))
	if strings.TrimSpace(c.KWS.WakePhrase) == "" {
		add(errors.New("kws.wake_phrase must be a nonempty phrase"))
	}
	if strings.TrimSpace(c.KWS.EndPhrase) == "" {
		add(errors.New("kws.end_phrase must be a nonempty phrase"))
	}
	if c.ASR.Threads < 1 || c.ASR.Threads > 64 {
		add(errors.New("asr.threads must be between 1 and 64"))
	}
	add(ValidateEndpoint("llm.endpoint", c.LLM.Endpoint))
	if c.LLM.APIKeyEnv != "" && !envName.MatchString(c.LLM.APIKeyEnv) {
		add(errors.New("llm.api_key_env must name an environment variable"))
	}
	switch c.Exec.AutoSubmit {
	case "", "enter", "ctrl+enter":
	default:
		add(errors.New(`exec.auto_submit must be "", "enter" or "ctrl+enter"`))
	}
	add(positiveSeconds("turn.max_sec", c.Turn.MaxSec))
	if c.Turn.FrontDrop < 0 || c.Turn.FrontDrop > 1 {
		add(errors.New("turn.front_drop must be between 0 and 1 seconds"))
	}
	if c.Turn.TailS < 0 || c.Turn.TailS > 5 {
		add(errors.New("turn.tail_s must be between 0 and 5 seconds"))
	}
	add(positiveSeconds("dictation.session_max_s", c.Dictation.SessionMaxS))
	add(positiveSeconds("dictation.recovery_ttl", c.Dictation.RecoveryTTL))
	switch c.Meeting.Mode {
	case ModeDictation, ModeMeeting, ModeCollaborative:
	default:
		add(errors.New("meeting.mode must be 'dictation', 'meeting', or 'collaborative'"))
	}
	if c.Meeting.SummaryAPIKeyEnv != "" && !envName.MatchString(c.Meeting.SummaryAPIKeyEnv) {
		add(errors.New("meeting.summary_api_key_env must name an environment variable"))
	}
	add(ValidateEndpoint("meeting.summary_endpoint", c.Meeting.SummaryEndpoint))
	add(ValidatePreviewColor(c.UI.PreviewColor))
	if c.UI.Accent != "" && c.UI.Accent != AccentGradient && !hexColor.MatchString(c.UI.Accent) {
		add(errors.New(`ui.accent must be "gradient", #RRGGBB or empty for monochrome`))
	}
	switch c.UI.Theme {
	case "auto", "dark", "light":
	default:
		add(errors.New("ui.theme must be auto, dark or light"))
	}
	if _, err := c.Hotkeys.Parse(); err != nil {
		add(err)
	}
	return errors.Join(errs...)
}

// ParsedHotkeys holds the three validated shortcuts.
type ParsedHotkeys struct {
	Push, Toggle, Cancel Hotkey
}

// Parse validates the three shortcuts and requires them to be distinct.
func (h Hotkeys) Parse() (ParsedHotkeys, error) {
	var out ParsedHotkeys
	var err error
	if out.Push, err = ParseHotkey(h.Push); err != nil {
		return out, fmt.Errorf("hotkeys.push: %w", err)
	}
	if out.Toggle, err = ParseHotkey(h.Toggle); err != nil {
		return out, fmt.Errorf("hotkeys.toggle: %w", err)
	}
	if out.Cancel, err = ParseHotkey(h.Cancel); err != nil {
		return out, fmt.Errorf("hotkeys.cancel: %w", err)
	}
	if out.Push == out.Toggle || out.Push == out.Cancel || out.Toggle == out.Cancel {
		return out, errors.New("push, toggle, and cancel shortcuts must be distinct")
	}
	return out, nil
}

// Resolve maps a configured path to a filesystem path. "models/..." resolves
// inside the user model directory; other relative paths resolve beside
// config.toml; absolute paths are used as given.
func (c *Config) Resolve(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	if strings.Contains(p, `\`) {
		return "", fmt.Errorf("expected a relative path with / separators, got %q", p)
	}
	clean := path.Clean(p)
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if part == ".." {
			return "", fmt.Errorf("path %q must not leave its root", p)
		}
	}
	if parts[0] == "models" {
		if len(parts) < 2 {
			return "", fmt.Errorf("expected a models/... path, got %q", p)
		}
		return filepath.Join(append([]string{paths.ModelsDir()}, parts[1:]...)...), nil
	}
	return filepath.Join(paths.ConfigDir(), filepath.FromSlash(clean)), nil
}

// MustResolve is Resolve for paths already validated at load time.
func (c *Config) MustResolve(p string) string {
	r, err := c.Resolve(p)
	if err != nil {
		return p
	}
	return r
}

// VocabularyPath returns the custom-word map file.
func (c *Config) VocabularyPath() string {
	if c.Polish.CustomWords == "" {
		return paths.VocabularyFile()
	}
	return c.MustResolve(c.Polish.CustomWords)
}

// SnippetsPath returns the snippet map file.
func (c *Config) SnippetsPath() string {
	if c.Polish.Snippets == "" {
		return paths.SnippetsFile()
	}
	return c.MustResolve(c.Polish.Snippets)
}

// HistoryPath returns the dictation history file.
func (c *Config) HistoryPath() string {
	if c.Dictation.History == "" {
		return paths.HistoryFile()
	}
	return c.MustResolve(c.Dictation.History)
}

// RecoveryPath returns the recovery spool directory.
func (c *Config) RecoveryPath() string {
	if c.Dictation.RecoveryDir == "" {
		return paths.RecoveryDir()
	}
	return c.MustResolve(c.Dictation.RecoveryDir)
}

// Clone returns a deep copy (all fields are values).
func (c *Config) Clone() *Config {
	cp := *c
	cp.Warnings = append([]string(nil), c.Warnings...)
	return &cp
}
