// Package recovery is the private, crash-safe spool of dictation drafts.
//
// Store layout: <dir>/spool/<epoch-ns>-<kind>.json (atomic per entry) and
// <dir>/audio/<epoch-ns>.wav for failed pastes.
//
//   - draft:  completed turns awaiting final insertion; replaced as turns
//     finish and dropped on cancel or successful insertion.
//   - commit: finalized text awaiting insertion; dropped after a paste.
//   - failed: finalized text that could not be pasted (optionally with audio).
//
// Recovery is explicit (`voice-io recover`, Settings, menu). Entries expire
// after the TTL.
package recovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/paths"
)

// Entry is one saved transcript.
type Entry struct {
	ID    int64   `json:"id"`
	TS    float64 `json:"ts"`
	Kind  string  `json:"kind"`
	Text  string  `json:"text"`
	Mode  string  `json:"mode,omitempty"`
	Audio string  `json:"audio,omitempty"`
}

// Time is the entry's creation time.
func (e Entry) Time() time.Time { return time.Unix(0, int64(e.TS*1e9)) }

// Store is safe for concurrent use.
type Store struct {
	Dir      string
	TTL      time.Duration
	spoolDir string
	audioDir string
	mu       sync.Mutex
	now      func() time.Time
	lastID   int64
}

// Open creates (or secures) the store directories.
func Open(dir string, ttl time.Duration) (*Store, error) {
	s := &Store{Dir: dir, TTL: ttl, spoolDir: filepath.Join(dir, "spool"),
		audioDir: filepath.Join(dir, "audio"), now: time.Now}
	for _, d := range []string{s.Dir, s.spoolDir, s.audioDir} {
		if err := paths.PrivateDir(d); err != nil {
			return nil, fmt.Errorf("recovery directory unavailable: %w", err)
		}
	}
	for _, pat := range []string{filepath.Join(s.spoolDir, "*.json"), filepath.Join(s.spoolDir, "*.tmp"),
		filepath.Join(s.audioDir, "*.wav")} {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if info, err := os.Lstat(m); err == nil && info.Mode().IsRegular() {
				_ = os.Chmod(m, 0o600)
			}
		}
	}
	return s, nil
}

func (s *Store) nextID() int64 {
	id := s.now().UnixNano()
	if id <= s.lastID {
		id = s.lastID + 1
	}
	s.lastID = id
	return id
}

// Spool persists an entry atomically and returns its unique ID.
func (s *Store) Spool(kind, text, mode, audioPath string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID()
	e := Entry{ID: id, TS: float64(id) / 1e9, Kind: kind, Text: text, Mode: mode, Audio: audioPath}
	data, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(s.spoolDir, ".recovery-*.tmp")
	if err != nil {
		return 0, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return 0, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(name, filepath.Join(s.spoolDir, fmt.Sprintf("%d-%s.json", id, kind))); err != nil {
		return 0, err
	}
	return id, nil
}

// SaveAudio stores failed-paste audio privately and returns its path.
func (s *Store) SaveAudio(samples []float32) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}
	s.mu.Lock()
	path := filepath.Join(s.audioDir, fmt.Sprintf("%d.wav", s.nextID()))
	s.mu.Unlock()
	if err := audio.WriteWAV(path, samples, audio.SampleRate); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Store) read(path string) (Entry, error) {
	var e Entry
	info, err := os.Lstat(path)
	if err != nil {
		return e, err
	}
	if !info.Mode().IsRegular() {
		return e, errors.New("not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	err = json.Unmarshal(data, &e)
	return e, err
}

func (s *Store) files() []string {
	files, _ := filepath.Glob(filepath.Join(s.spoolDir, "*.json"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files
}

// List returns unexpired entries, newest first.
func (s *Store) List() []Entry {
	s.Prune()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, f := range s.files() {
		if e, err := s.read(f); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// Latest is the newest unexpired entry of any kind.
func (s *Store) Latest() (Entry, bool) {
	entries := s.List()
	if len(entries) == 0 {
		return Entry{}, false
	}
	return entries[0], true
}

// Get returns the entry with id.
func (s *Store) Get(id int64) (Entry, bool) {
	for _, e := range s.List() {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

func (s *Store) ownsAudio(path string) bool {
	return path != "" && filepath.Dir(path) == s.audioDir && strings.HasSuffix(path, ".wav")
}

// Drop removes the entry with id and its attached failed-paste audio.
func (s *Store) Drop(id int64) {
	if id == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	matches, _ := filepath.Glob(filepath.Join(s.spoolDir, fmt.Sprintf("%d-*.json", id)))
	for _, f := range matches {
		e, err := s.read(f)
		if err != nil || e.ID != id {
			continue
		}
		_ = os.Remove(f)
		if s.ownsAudio(e.Audio) {
			_ = os.Remove(e.Audio)
		}
	}
}

// Prune removes expired entries, their audio, and orphaned expired audio.
func (s *Store) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-s.TTL)
	retained := map[string]bool{}
	for _, f := range s.files() {
		e, err := s.read(f)
		if err != nil {
			if info, lerr := os.Lstat(f); lerr == nil && info.Mode().IsRegular() {
				_ = os.Remove(f)
			}
			continue
		}
		if e.Time().Before(cutoff) {
			_ = os.Remove(f)
			if s.ownsAudio(e.Audio) {
				_ = os.Remove(e.Audio)
			}
			continue
		}
		if s.ownsAudio(e.Audio) {
			retained[e.Audio] = true
		}
	}
	wavs, _ := filepath.Glob(filepath.Join(s.audioDir, "*.wav"))
	for _, w := range wavs {
		info, err := os.Lstat(w)
		if err != nil || !info.Mode().IsRegular() || retained[w] {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(w)
		}
	}
}

// SpillDir returns a fresh private directory for queued turn audio.
func (s *Store) SpillDir() (string, error) {
	return os.MkdirTemp(s.Dir, "voice-io-pcm-")
}
