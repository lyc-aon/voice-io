// Package history is the private JSONL log of successfully inserted text.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Entry is one inserted dictation.
type Entry struct {
	ID   int64   `json:"id,omitempty"`
	TS   float64 `json:"ts"`
	Text string  `json:"text"`
	Mode string  `json:"mode,omitempty"`
}

// Key identifies an entry, falling back to its timestamp for older lines.
func (e Entry) Key() int64 {
	if e.ID != 0 {
		return e.ID
	}
	return int64(e.TS * 1e9)
}

// Log appends and edits the history file. Methods are safe for concurrent use.
type Log struct {
	Path   string
	mu     sync.Mutex
	lastID int64
}

// SetPath moves future reads and writes to path (a settings change).
func (l *Log) SetPath(path string) {
	l.mu.Lock()
	l.Path = path
	l.mu.Unlock()
}

// Append records one inserted dictation (best effort for callers).
func (l *Log) Append(text, mode string) error {
	if l == nil || l.Path == "" || text == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return err
	}
	id := time.Now().UnixNano()
	if id <= l.lastID {
		id = l.lastID + 1 // keys must stay unique for Delete
	}
	l.lastID = id
	line, err := json.Marshal(Entry{ID: id, TS: float64(id) / 1e9, Text: text, Mode: mode})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_ = f.Chmod(0o600)
	_, err = f.Write(append(line, '\n'))
	return err
}

// List returns up to limit entries, newest first (limit <= 0 = all).
func (l *Log) List(limit int) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, err := l.readAll()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (l *Log) readAll() ([]Entry, error) {
	f, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(bytes.TrimSpace(sc.Bytes()), &e) == nil && e.Text != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// Delete removes the entry with key (see Entry.Key).
func (l *Log) Delete(key int64) error {
	return l.rewrite(func(e Entry) bool { return e.Key() != key })
}

// Clear removes every entry.
func (l *Log) Clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := os.Remove(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (l *Log) rewrite(keep func(Entry) bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, err := l.readAll()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range entries {
		if keep(e) {
			line, _ := json.Marshal(e)
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.Path), ".history-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, l.Path)
}
