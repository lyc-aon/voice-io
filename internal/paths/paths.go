// Package paths resolves voice-io's per-user files.
//
// Config lives at $VOICE_IO_CONFIG, else $XDG_CONFIG_HOME/voice-io/config.toml
// (default ~/.config/voice-io/config.toml) on macOS and Linux and
// %APPDATA%\voice-io\config.toml on Windows. Data (models, recovery,
// meetings, history) lives in ~/Library/Application Support/voice-io on
// macOS, $XDG_DATA_HOME/voice-io on Linux and %LOCALAPPDATA%\voice-io on
// Windows. Nothing here creates files.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

const appDir = "voice-io"

func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(home(), fallback)
}

// localAppData is %LOCALAPPDATA% (Windows only).
func localAppData() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" && filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(home(), "AppData", "Local")
}

// ConfigFile is the user config.toml path.
func ConfigFile() string {
	if v := os.Getenv("VOICE_IO_CONFIG"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	if runtime.GOOS == "windows" {
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, appDir, "config.toml")
		}
	}
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), appDir, "config.toml")
}

// ConfigDir holds config.toml and its sibling user files (vocabulary,
// snippets, overlay position).
func ConfigDir() string { return filepath.Dir(ConfigFile()) }

// DataDir is the private per-user data root.
func DataDir() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home(), "Library", "Application Support", appDir)
	case "windows":
		return filepath.Join(localAppData(), appDir)
	}
	return filepath.Join(xdg("XDG_DATA_HOME", filepath.Join(".local", "share")), appDir)
}

// ModelsDir holds downloaded model weights.
func ModelsDir() string { return filepath.Join(DataDir(), "models") }

// RecoveryDir holds the crash-safe transcript spool.
func RecoveryDir() string { return filepath.Join(DataDir(), "recovery") }

// MeetingsDir holds meeting recordings and transcripts.
func MeetingsDir() string { return filepath.Join(DataDir(), "meetings") }

// HistoryFile is the JSONL log of successfully inserted dictations.
func HistoryFile() string { return filepath.Join(DataDir(), "history.jsonl") }

// CacheDir holds regenerable files (cue WAVs on Linux).
func CacheDir() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home(), "Library", "Caches", appDir)
	case "windows":
		return filepath.Join(DataDir(), "cache")
	}
	return filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), appDir)
}

// LogDir holds voice-io.log.
func LogDir() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home(), "Library", "Logs", appDir)
	case "windows":
		return filepath.Join(DataDir(), "logs")
	}
	return filepath.Join(xdg("XDG_STATE_HOME", filepath.Join(".local", "state")), appDir)
}

// LogFile is the daemon log written by `voice-io start`.
func LogFile() string { return filepath.Join(LogDir(), "voice-io.log") }

// RuntimeDir holds the control socket and instance lock.
func RuntimeDir() string {
	if runtime.GOOS == "linux" {
		if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" && filepath.IsAbs(v) {
			return filepath.Join(v, appDir)
		}
	}
	return filepath.Join(DataDir(), "run")
}

// SocketPath is the daemon's control socket. Unix socket paths are limited
// to ~104 bytes on macOS; very long home paths fall back to the temp dir.
func SocketPath() string {
	p := filepath.Join(RuntimeDir(), "control.sock")
	if len(p) < 100 {
		return p
	}
	return filepath.Join(os.TempDir(), "voice-io-"+uidString(), "control.sock")
}

// LockPath guards against a second daemon instance.
func LockPath() string { return filepath.Join(RuntimeDir(), "daemon.lock") }

// VocabularyFile is the default custom-word correction map.
func VocabularyFile() string { return filepath.Join(ConfigDir(), "vocabulary.json") }

// SnippetsFile is the default spoken-snippet map.
func SnippetsFile() string { return filepath.Join(ConfigDir(), "snippets.json") }

// OverlayStateFile remembers the overlay's dragged position.
func OverlayStateFile() string { return filepath.Join(ConfigDir(), "overlay.json") }

// PrivateDir creates dir (and parents) and forces mode 0700 on dir itself,
// refusing a symlinked leaf.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &os.PathError{Op: "private dir", Path: dir, Err: os.ErrPermission}
	}
	return os.Chmod(dir, 0o700)
}
