package speech

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/config"
)

// synth renders text with macOS speech synthesis at 16 kHz mono.
func synth(t *testing.T, voice, text string) []float32 {
	t.Helper()
	if _, err := exec.LookPath("say"); err != nil {
		t.Skip("speech synthesis (say) unavailable")
	}
	path := filepath.Join(t.TempDir(), "speech.wav")
	if out, err := exec.Command("say", "-v", voice, "-o", path, "--data-format=LEI16@16000", text).CombinedOutput(); err != nil {
		t.Skipf("say failed: %v %s", err, out)
	}
	samples, rate, err := audio.ReadWAV(path)
	if err != nil || rate != audio.SampleRate {
		t.Fatalf("read synthesized speech: %v (rate %d)", err, rate)
	}
	return samples
}

func installedModels(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	for _, p := range []string{cfg.VAD.Model, cfg.KWS.ModelDir, cfg.ASR.Streaming, cfg.ASR.FinalEN} {
		if _, err := os.Stat(cfg.MustResolve(p)); err != nil {
			t.Skip("models not installed; run voice-io models install")
		}
	}
	return cfg
}

func TestEnginesFollowSynthesizedDictation(t *testing.T) {
	cfg := installedModels(t)
	samples := synth(t, "Samantha",
		"Hey voice. [[slnc 700]] Please draft the release notes for Friday. [[slnc 900]] Voice send.")

	vad, err := NewVAD(VADConfig{Model: cfg.MustResolve(cfg.VAD.Model), Threshold: cfg.VAD.Threshold,
		MinSilenceDuration: cfg.VAD.MinSilenceDuration, MaxSpeechDuration: cfg.VAD.MaxSpeechDuration})
	if err != nil {
		t.Fatal(err)
	}
	defer vad.Close()
	kws, err := NewKWS(KWSConfig{ModelDir: cfg.MustResolve(cfg.KWS.ModelDir), Chunk: cfg.KWS.Chunk,
		Score: cfg.KWS.Score, Threshold: cfg.KWS.Threshold, WakePhrase: cfg.KWS.WakePhrase,
		EndPhrase: cfg.KWS.EndPhrase, Threads: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer kws.Close()

	var labels []string
	var segments []float64
	chunk := audio.SampleRate / 10
	padded := append(samples, make([]float32, audio.SampleRate)...)
	for i := 0; i+chunk <= len(padded); i += chunk {
		block := padded[i : i+chunk]
		vad.Accept(block)
		got, err := kws.Accept(block)
		if err != nil {
			t.Fatal(err)
		}
		labels = append(labels, got...)
		segments = append(segments, vad.PopSegments()...)
	}
	if strings.Join(labels, ",") != "WAKE,SEND" {
		t.Errorf("keyword labels = %v, want [WAKE SEND]", labels)
	}
	if len(segments) < 3 {
		t.Errorf("VAD found %d speech segments, want >= 3", len(segments))
	}

	stream, err := NewStreaming(cfg.MustResolve(cfg.ASR.Streaming), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	stream.NewStream()
	partial := stream.Accept(samples)
	final := strings.ToLower(stream.Finalize(1.0))
	if partial == "" || !strings.Contains(final, "release") {
		t.Errorf("streaming partial %q final %q; want release notes", partial, final)
	}

	refiner := NewRefiner(cfg.MustResolve(cfg.ASR.FinalEN), 4)
	defer refiner.Close()
	start := time.Now()
	text, err := refiner.Transcribe(samples)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("offline transcript in %v: %q", time.Since(start), text)
	if lower := strings.ToLower(text); !strings.Contains(lower, "release notes") || !strings.Contains(lower, "friday") {
		t.Errorf("offline transcript %q lacks the dictated words", text)
	}
}

func TestDiarizerSeparatesTwoVoicesAndCancels(t *testing.T) {
	cfg := installedModels(t)
	seg, emb := cfg.MustResolve(cfg.Meeting.SegmentationModel), cfg.MustResolve(cfg.Meeting.EmbeddingModel)
	if _, err := os.Stat(emb); err != nil {
		t.Skip("meeting models not installed")
	}
	a := synth(t, "Samantha", "Let's review the launch plan and confirm the owners for each task this week.")
	b := synth(t, "Daniel", "I can take the security review, and I will send the report on Thursday morning.")
	gap := make([]float32, audio.SampleRate)
	recording := append(append(append(append([]float32{}, a...), gap...), b...), gap...)
	recording = append(recording, a...)

	d, err := NewDiarizer(seg, emb, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	regions, err := d.Process(recording, nil)
	if err != nil {
		t.Fatal(err)
	}
	speakers := map[int]bool{}
	for _, r := range regions {
		speakers[r.Speaker] = true
	}
	t.Logf("regions: %+v", regions)
	if len(speakers) != 2 {
		t.Errorf("found %d speakers, want 2", len(speakers))
	}
	if len(regions) < 3 || regions[0].Speaker != regions[len(regions)-1].Speaker {
		t.Errorf("first and last turns should share a speaker: %+v", regions)
	}

	calls := 0
	_, err = d.Process(recording, func() bool { calls++; return true })
	if err != ErrCancelled || calls == 0 {
		t.Errorf("cancelled diarization returned %v after %d polls", err, calls)
	}
}
