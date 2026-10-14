package recovery

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestOpenTightensLegacyPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recovery")
	for _, d := range []string{dir, filepath.Join(dir, "spool"), filepath.Join(dir, "audio")} {
		if err := os.MkdirAll(d, 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	legacy := []string{filepath.Join(dir, "spool", "1-commit.json"), filepath.Join(dir, "audio", "1.wav")}
	for _, f := range legacy {
		if err := os.WriteFile(f, []byte("{}"), 0o664); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f, 0o664); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(dir, time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, filepath.Join(dir, "spool"), filepath.Join(dir, "audio")} {
		if m := mode(t, d); m&0o077 != 0 {
			t.Errorf("%s mode %v", d, m)
		}
	}
	for _, f := range legacy {
		if m := mode(t, f); m&0o077 != 0 {
			t.Errorf("%s mode %v", f, m)
		}
	}
}

func TestFailedPasteKeepsTextAndAudioUntilDropped(t *testing.T) {
	s, err := Open(t.TempDir(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wav, err := s.SaveAudio(make([]float32, 1600))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Spool("failed", "recover me", "dictation", wav)
	if err != nil {
		t.Fatal(err)
	}
	if m := mode(t, wav); m&0o077 != 0 {
		t.Errorf("audio mode %v", m)
	}
	spooled, _ := filepath.Glob(filepath.Join(s.Dir, "spool", "*.json"))
	if len(spooled) != 1 || mode(t, spooled[0])&0o077 != 0 {
		t.Fatalf("spool files %v", spooled)
	}
	latest, ok := s.Latest()
	if !ok || latest.Text != "recover me" || latest.Kind != "failed" || latest.ID != id || latest.Audio != wav {
		t.Fatalf("latest %+v %v", latest, ok)
	}
	s.Drop(id)
	if _, ok := s.Latest(); ok {
		t.Fatal("entry survived Drop")
	}
	if _, err := os.Stat(wav); !os.IsNotExist(err) {
		t.Fatalf("audio survived Drop: %v", err)
	}
}

func TestRecoveryNeverDeletesExternalAudio(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "recovery"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(root, "unrelated.wav")
	if err := os.WriteFile(external, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := s.Spool("failed", "text", "dictation", external)
	if err != nil {
		t.Fatal(err)
	}
	s.Drop(id)
	if _, err := s.Spool("failed", "old", "dictation", external); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(s.Dir, "audio", "5.wav")
	if err := os.WriteFile(orphan, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(orphan, aged, aged); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	s.Prune()
	if data, err := os.ReadFile(external); err != nil || string(data) != "keep" {
		t.Fatalf("external audio touched: %q %v", data, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("aged orphan audio kept: %v", err)
	}
	if _, ok := s.Latest(); ok {
		t.Fatal("expired entry kept")
	}
}

func TestPruneKeepsFreshEntriesAndTheirAudio(t *testing.T) {
	s, err := Open(t.TempDir(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wav, _ := s.SaveAudio(make([]float32, 160))
	if _, err := s.Spool("failed", "fresh", "dictation", wav); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(wav, aged, aged) // old file time, but its entry is fresh
	s.Prune()
	if e, ok := s.Latest(); !ok || e.Text != "fresh" {
		t.Fatalf("fresh entry pruned: %+v", e)
	}
	if _, err := os.Stat(wav); err != nil {
		t.Fatalf("referenced audio pruned: %v", err)
	}
}

func TestSimultaneousEntriesDropIndependently(t *testing.T) {
	s, err := Open(t.TempDir(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Unix(1_800_000_000, 1)
	s.now = func() time.Time { return fixed }
	first, _ := s.Spool("draft", "first", "dictation", "")
	second, _ := s.Spool("commit", "second", "dictation", "")
	if first == second {
		t.Fatalf("same id %d", first)
	}
	s.Drop(first)
	if e, ok := s.Latest(); !ok || e.Text != "second" {
		t.Fatalf("after dropping first: %+v %v", e, ok)
	}
	s.Drop(second)
	if _, ok := s.Latest(); ok {
		t.Fatal("entries remain")
	}
}

func TestSpillDirIsPrivate(t *testing.T) {
	s, err := Open(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.SpillDir()
	if err != nil {
		t.Fatal(err)
	}
	if m := mode(t, d); m&0o077 != 0 {
		t.Fatalf("spill dir mode %v", m)
	}
}
