package history

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHistoryIsPrivateNewestFirstAndEditable(t *testing.T) {
	l := &Log{Path: filepath.Join(t.TempDir(), "data", "history.jsonl")}
	for _, text := range []string{"first", "second", "third"} {
		if err := l.Append(text, "dictation"); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Append("", "dictation"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(l.Path); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("history mode %v (%v)", info.Mode().Perm(), err)
	}
	all, err := l.List(0)
	if err != nil || len(all) != 3 || all[0].Text != "third" || all[2].Text != "first" {
		t.Fatalf("list %+v (%v)", all, err)
	}
	if two, _ := l.List(2); len(two) != 2 || two[1].Text != "second" {
		t.Fatalf("limited list %+v", two)
	}
	if err := l.Delete(all[1].Key()); err != nil {
		t.Fatal(err)
	}
	left, _ := l.List(0)
	if len(left) != 2 || left[0].Text != "third" || left[1].Text != "first" {
		t.Fatalf("after delete %+v", left)
	}
	if info, _ := os.Stat(l.Path); info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("rewritten history mode %v", info.Mode().Perm())
	}
	if err := l.Clear(); err != nil {
		t.Fatal(err)
	}
	if none, _ := l.List(0); len(none) != 0 {
		t.Fatalf("after clear %+v", none)
	}
}
