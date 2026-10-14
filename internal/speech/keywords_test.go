package speech

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lexiconDir(t *testing.T, lexicon string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "en.phone"), []byte(lexicon), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDefaultPhrasesCompileWithStableLabels(t *testing.T) {
	dir := lexiconDir(t, "HEY HH EY1\nVOICE V OY1 S\nSEND S EH1 N D\n")
	got, err := KeywordLines(dir, "hey voice", "Voice  send")
	if err != nil {
		t.Fatal(err)
	}
	if want := "HH EY1 V OY1 S @WAKE\nV OY1 S S EH1 N D @SEND\n"; got != want {
		t.Fatalf("KeywordLines = %q; want %q", got, want)
	}
}

func TestUnsupportedPhrasesAreActionable(t *testing.T) {
	dir := lexiconDir(t, "HEY HH EY1\nSEND S EH1 N D\n")
	_, err := KeywordLines(dir, "hey zorp", "send")
	if err == nil || !strings.Contains(err.Error(), "zorp") || !strings.Contains(err.Error(), "en.phone") {
		t.Fatalf("unknown word: %v", err)
	}
	if _, err := KeywordLines(dir, "hey!", "send"); err == nil {
		t.Fatal("punctuation accepted")
	}
	if _, err := KeywordLines(dir, "hey", "hey"); err == nil || !strings.Contains(err.Error(), "same pronunciation") {
		t.Fatalf("identical phrases: %v", err)
	}
	if _, err := KeywordLines(t.TempDir(), "hey", "send"); err == nil {
		t.Fatal("missing lexicon accepted")
	}
}
