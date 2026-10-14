package meeting

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/llm"
	"gitlab.com/integral-inference/voice-io/internal/text"
)

var cleanOpts = text.Options{Words: map[string]string{"kubernetees": "Kubernetes"}, Filler: true, Emoji: true}

func TestCollaborativeDraftKeepsSpeakerTurnsWithoutTimestamps(t *testing.T) {
	lines := []SpeechLine{{0, 1, 0, "Let's review the plan."}, {1.01, 2, 0, "Start with design."},
		{2, 3, 1, "I agree."}, {3, 4, 0, "Then we can ship."}}
	want := "Speaker 1: Let's review the plan. Start with design.\nSpeaker 2: I agree.\nSpeaker 1: Then we can ship."
	if got := CollaborativeText(lines, cleanOpts); got != want {
		t.Fatalf("collaborative:\n%s", got)
	}
	if got := TranscriptText(lines); !strings.HasPrefix(got, "[00:00:00–00:00:01] Speaker 1: Let's review the plan.") {
		t.Fatalf("transcript:\n%s", got)
	}
}

func TestSpeakersAreNumberedByFirstAppearance(t *testing.T) {
	lines := []SpeechLine{{0, 1, 3, "First voice."}, {2, 3, 0, "Second voice."}, {4, 5, 3, "First voice again."},
		{6, 7, -1, "Someone."}}
	collab := CollaborativeText(lines, cleanOpts)
	if want := "Speaker 1: First voice.\nSpeaker 2: Second voice.\nSpeaker 1: First voice again.\nUnknown speaker: Someone."; collab != want {
		t.Fatalf("collaborative:\n%s", collab)
	}
	transcript := TranscriptText(lines)
	want := "[00:00:00–00:00:01] Speaker 1: First voice.\n[00:00:02–00:00:03] Speaker 2: Second voice.\n" +
		"[00:00:04–00:00:05] Speaker 1: First voice again.\n[00:00:06–00:00:07] Unknown speaker: Someone."
	if transcript != want {
		t.Fatalf("transcript:\n%s", transcript)
	}
}

func TestCollaborativeDraftMergesOnlyContiguousChunks(t *testing.T) {
	lines := []SpeechLine{{0, 1, 0, "One"}, {1.04, 2, 0, "two"}, {2.051, 3, 0, "new turn"}, {2.9, 3.5, 0, "overlap"},
		{3.5, 4, 1, "another speaker"}, {4, 5, 0, "back again"}}
	want := "Speaker 1: One two\nSpeaker 1: new turn\nSpeaker 1: overlap\nSpeaker 2: another speaker\nSpeaker 1: back again"
	if got := CollaborativeText(lines, cleanOpts); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestCollaborativeDraftCleansLinesAndOmitsEmptySpeech(t *testing.T) {
	lines := []SpeechLine{{0, 1, -1, "um"}, {1, 2, -1, " um  kubernetees "}, {2, 3, -1, "smiling emoji"},
		{3, 4, 2, "   "}, {4, 5, -1, "hello"}}
	if got := CollaborativeText(lines, cleanOpts); got != "Unknown speaker: Kubernetes 😊\nUnknown speaker: hello" {
		t.Fatalf("got %q", got)
	}
	if got := CollaborativeText([]SpeechLine{{0, 1, -1, "um"}}, cleanOpts); got != "" {
		t.Fatalf("filler only: %q", got)
	}
	if got := CollaborativeText(nil, cleanOpts); got != "" {
		t.Fatalf("empty: %q", got)
	}
	raw := text.Options{Words: cleanOpts.Words}
	if got := CollaborativeText(lines[1:3], raw); got != "Unknown speaker: um Kubernetes smiling emoji" {
		t.Fatalf("unfiltered: %q", got)
	}
}

func TestChunksPreserveEveryLine(t *testing.T) {
	var lines []string
	for i := range 280 {
		lines = append(lines, "[00:00:00–00:00:01] Speaker 1: "+strings.Repeat("word ", 20)+string(rune('a'+i%26)))
	}
	chunks, err := Chunks(lines, SummaryChars)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("%d chunks", len(chunks))
	}
	for _, c := range chunks {
		if len([]rune(c)) > SummaryChars {
			t.Fatalf("chunk of %d chars", len([]rune(c)))
		}
	}
	if strings.Join(chunks, "\n") != strings.Join(lines, "\n") {
		t.Fatal("chunks lost or reordered lines")
	}
	if _, err := Chunks([]string{strings.Repeat("x", SummaryChars+1)}, SummaryChars); err == nil {
		t.Fatal("oversized line accepted")
	}
}

func TestRecorderIsPrivateReadableWhileOpenAndDiscardable(t *testing.T) {
	r, err := NewRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	block := make([]float32, audio.SampleRate)
	for range 6 {
		if err := r.Append(block); err != nil {
			t.Fatal(err)
		}
	}
	samples, rate, err := audio.ReadWAV(r.Path) // still open: the header is current
	if err != nil || rate != audio.SampleRate || len(samples) != 6*audio.SampleRate {
		t.Fatalf("open recording: %d samples at %d Hz, %v", len(samples), rate, err)
	}
	if info, _ := os.Stat(r.Path); info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("recording mode %v", info.Mode().Perm())
	}
	path, err := r.Close()
	if err != nil || path != r.Path {
		t.Fatal(path, err)
	}
	if err := r.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("discarded recording still exists")
	}
}

func recording(t *testing.T, dir string, seconds int) string {
	t.Helper()
	r, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Append(make([]float32, seconds*audio.SampleRate)); err != nil {
		t.Fatal(err)
	}
	path, err := r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeDiarizer(lines ...SpeechLine) func([]float32, func() bool) ([]SpeechLine, error) {
	return func([]float32, func() bool) ([]SpeechLine, error) { return lines, nil }
}

func TestTranscriptKeepsTimestampsAndDropsRawAudio(t *testing.T) {
	dir := t.TempDir()
	wav := recording(t, dir, 6)
	p := &Processor{DiarizeFunc: fakeDiarizer(SpeechLine{1, 4, 0, "First speaker"}, SpeechLine{5, 6, 1, "Second speaker"})}
	res, err := p.Process(wav, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(res.Document)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	for _, want := range []string{"[00:00:01–00:00:04] Speaker 1: First speaker", "[00:00:05–00:00:06] Speaker 2: Second speaker",
		"0:06 recorded · 2 speakers", "## Transcript"} {
		if !strings.Contains(doc, want) {
			t.Errorf("document lacks %q:\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "## Summary") {
		t.Error("summary without an endpoint")
	}
	if info, _ := os.Stat(res.Document); info.Mode().Perm()&0o077 != 0 {
		t.Errorf("notes mode %v", info.Mode().Perm())
	}
	if _, err := os.Stat(wav); !os.IsNotExist(err) {
		t.Error("raw audio kept after publishing")
	}
	if filepath.Dir(res.Document) != dir {
		t.Errorf("notes written to %s", res.Document)
	}
}

func TestDiarizationFailureKeepsRecording(t *testing.T) {
	wav := recording(t, t.TempDir(), 1)
	p := &Processor{DiarizeFunc: fakeDiarizer()}
	if _, err := p.Process(wav, func() bool { return false }); err == nil {
		t.Fatal("empty diarization published notes")
	}
	if _, err := os.Stat(wav); err != nil {
		t.Fatal("recording removed after failure")
	}
}

func TestSummaryUsesTheEndpointAndCredentialFailureKeepsTranscript(t *testing.T) {
	var gotAuth, gotPath, gotModel, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role, Content string
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		for _, m := range body.Messages {
			if m.Role == "user" {
				gotUser = m.Content
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Decision: approve the review."}}]}`))
	}))
	defer srv.Close()
	t.Setenv("VOICE_IO_MEETING_TEST_KEY", "secret")
	dir := t.TempDir()
	newProcessor := func() *Processor {
		return &Processor{DiarizeFunc: fakeDiarizer(SpeechLine{0, 1, 0, "We approve the review."}),
			Summarizer: &Summarizer{Client: &llm.Client{Endpoint: srv.URL + "/v1", Model: "local-summarizer",
				APIKeyEnv: "VOICE_IO_MEETING_TEST_KEY"}}}
	}
	res, err := newProcessor().Process(recording(t, dir, 1), func() bool { return false })
	if err != nil || res.SummaryErr != nil {
		t.Fatal(err, res.SummaryErr)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer secret" || gotModel != "local-summarizer" ||
		!strings.Contains(gotUser, "Speaker 1") {
		t.Fatalf("request path=%q auth=%q model=%q user=%q", gotPath, gotAuth, gotModel, gotUser)
	}
	doc, _ := os.ReadFile(res.Document)
	if !strings.Contains(string(doc), "## Summary\n\nDecision: approve the review.") {
		t.Fatalf("summary missing:\n%s", doc)
	}

	t.Setenv("VOICE_IO_MEETING_TEST_KEY", "")
	res, err = newProcessor().Process(recording(t, dir, 1), func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if res.SummaryErr == nil || !strings.Contains(res.SummaryErr.Error(), "VOICE_IO_MEETING_TEST_KEY") {
		t.Fatalf("summary error %v", res.SummaryErr)
	}
	doc, _ = os.ReadFile(res.Document)
	if strings.Contains(string(doc), "## Summary") || !strings.Contains(string(doc), "## Transcript") {
		t.Fatalf("document after credential failure:\n%s", doc)
	}
}
