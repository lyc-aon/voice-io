// Package speech wraps the sherpa-onnx engines voice-io uses: Silero VAD,
// the wake/end keyword spotter, the streaming preview recognizer, the
// authoritative offline Parakeet recognizer and offline speaker diarization.
package speech

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	so "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const sampleRate = 16000

func requireFiles(dir string, names ...string) error {
	var missing []string
	for _, n := range names {
		if info, err := os.Stat(filepath.Join(dir, n)); err != nil || info.Size() == 0 {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("model files missing in %s: %s; run voice-io models install",
			dir, strings.Join(missing, ", "))
	}
	return nil
}

// VADConfig mirrors [vad] in config.toml.
type VADConfig struct {
	Model              string
	Threshold          float64
	MinSilenceDuration float64
	MaxSpeechDuration  float64
}

// VAD is Silero VAD v5 fed with arbitrary-length audio.
type VAD struct {
	det *so.VoiceActivityDetector
}

// vadWindow is Silero's 32 ms input window at 16 kHz.
const vadWindow = 512

// NewVAD loads the detector.
func NewVAD(c VADConfig) (*VAD, error) {
	if info, err := os.Stat(c.Model); err != nil || info.Size() == 0 {
		return nil, fmt.Errorf("VAD model not found: %s; run voice-io models install", c.Model)
	}
	cfg := so.VadModelConfig{
		SileroVad: so.SileroVadModelConfig{
			Model:              c.Model,
			Threshold:          float32(c.Threshold),
			MinSilenceDuration: float32(c.MinSilenceDuration),
			MinSpeechDuration:  0.25,
			WindowSize:         vadWindow,
			MaxSpeechDuration:  float32(c.MaxSpeechDuration),
		},
		SampleRate: sampleRate,
		NumThreads: 1,
		Provider:   "cpu",
	}
	det := so.NewVoiceActivityDetector(&cfg, 30)
	if det == nil {
		return nil, fmt.Errorf("cannot load VAD model %s", c.Model)
	}
	return &VAD{det: det}, nil
}

// Accept feeds samples in Silero-sized windows.
func (v *VAD) Accept(samples []float32) {
	for i := 0; i < len(samples); i += vadWindow {
		end := min(i+vadWindow, len(samples))
		v.det.AcceptWaveform(samples[i:end])
	}
}

// IsSpeech reports whether speech is currently detected.
func (v *VAD) IsSpeech() bool { return v.det.IsSpeech() }

// PopSegments drains completed speech segments, returning their durations
// in seconds.
func (v *VAD) PopSegments() []float64 {
	var out []float64
	for !v.det.IsEmpty() {
		seg := v.det.Front()
		out = append(out, float64(len(seg.Samples))/sampleRate)
		v.det.Pop()
	}
	return out
}

// Clear resets detector state.
func (v *VAD) Clear() { v.det.Reset() }

// Close frees the detector.
func (v *VAD) Close() {
	if v.det != nil {
		so.DeleteVoiceActivityDetector(v.det)
		v.det = nil
	}
}

// KWSConfig mirrors [kws] in config.toml.
type KWSConfig struct {
	ModelDir   string
	Chunk      string
	Score      float64
	Threshold  float64
	WakePhrase string
	EndPhrase  string
	Threads    int
}

// KWS spots the configured wake (WAKE) and end (SEND) phrases.
type KWS struct {
	spotter *so.KeywordSpotter
	stream  *so.OnlineStream
}

// NewKWS compiles the phrases and loads the spotter.
func NewKWS(c KWSConfig) (*KWS, error) {
	keywords, err := KeywordLines(c.ModelDir, c.WakePhrase, c.EndPhrase)
	if err != nil {
		return nil, err
	}
	suffix := "epoch-13-avg-2-" + c.Chunk + "-left-64"
	enc, dec, join := "encoder-"+suffix+".int8.onnx", "decoder-"+suffix+".onnx", "joiner-"+suffix+".int8.onnx"
	if err := requireFiles(c.ModelDir, "tokens.txt", enc, dec, join); err != nil {
		return nil, err
	}
	cfg := so.KeywordSpotterConfig{
		FeatConfig: so.FeatureConfig{SampleRate: sampleRate, FeatureDim: 80},
		ModelConfig: so.OnlineModelConfig{
			Transducer: so.OnlineTransducerModelConfig{
				Encoder: filepath.Join(c.ModelDir, enc),
				Decoder: filepath.Join(c.ModelDir, dec),
				Joiner:  filepath.Join(c.ModelDir, join),
			},
			Tokens:     filepath.Join(c.ModelDir, "tokens.txt"),
			NumThreads: c.Threads,
			Provider:   "cpu",
		},
		MaxActivePaths:    4,
		KeywordsScore:     float32(c.Score),
		KeywordsThreshold: float32(c.Threshold),
		KeywordsBuf:       keywords,
		KeywordsBufSize:   len(keywords),
	}
	spotter := so.NewKeywordSpotter(&cfg)
	if spotter == nil {
		return nil, fmt.Errorf("cannot load keyword spotter from %s", c.ModelDir)
	}
	return &KWS{spotter: spotter, stream: so.NewKeywordStream(spotter)}, nil
}

// Accept feeds samples and returns detected labels (WAKE or SEND).
func (k *KWS) Accept(samples []float32) ([]string, error) {
	k.stream.AcceptWaveform(sampleRate, samples)
	var out []string
	for k.spotter.IsReady(k.stream) {
		k.spotter.Decode(k.stream)
		kw := k.spotter.GetResult(k.stream).Keyword
		if kw == "" {
			continue
		}
		label := strings.TrimPrefix(kw, "@")
		if label != Wake && label != Send {
			return out, fmt.Errorf("unexpected keyword label %q; expected WAKE or SEND", kw)
		}
		out = append(out, label)
		k.spotter.Reset(k.stream)
	}
	return out, nil
}

// Close frees the spotter.
func (k *KWS) Close() {
	if k.stream != nil {
		so.DeleteOnlineStream(k.stream)
		k.stream = nil
	}
	if k.spotter != nil {
		so.DeleteKeywordSpotter(k.spotter)
		k.spotter = nil
	}
}

// Streaming is the zipformer2 streaming recognizer used only for previews
// and as a fallback when the offline re-decode fails.
type Streaming struct {
	rec    *so.OnlineRecognizer
	stream *so.OnlineStream
}

// NewStreaming loads the streaming model directory.
func NewStreaming(dir string, threads int) (*Streaming, error) {
	if err := requireFiles(dir, "tokens.txt", "bpe.model", "encoder.int8.onnx", "decoder.onnx", "joiner.int8.onnx"); err != nil {
		return nil, err
	}
	cfg := so.OnlineRecognizerConfig{
		FeatConfig: so.FeatureConfig{SampleRate: sampleRate, FeatureDim: 80},
		ModelConfig: so.OnlineModelConfig{
			Transducer: so.OnlineTransducerModelConfig{
				Encoder: filepath.Join(dir, "encoder.int8.onnx"),
				Decoder: filepath.Join(dir, "decoder.onnx"),
				Joiner:  filepath.Join(dir, "joiner.int8.onnx"),
			},
			Tokens:       filepath.Join(dir, "tokens.txt"),
			NumThreads:   threads,
			Provider:     "cpu",
			ModelType:    "zipformer2",
			ModelingUnit: "bpe",
			BpeVocab:     filepath.Join(dir, "bpe.model"),
		},
		DecodingMethod: "greedy_search",
		MaxActivePaths: 4,
	}
	rec := so.NewOnlineRecognizer(&cfg)
	if rec == nil {
		return nil, fmt.Errorf("cannot load streaming recognizer from %s", dir)
	}
	return &Streaming{rec: rec}, nil
}

// NewStream starts a fresh utterance.
func (s *Streaming) NewStream() {
	if s.stream != nil {
		so.DeleteOnlineStream(s.stream)
	}
	s.stream = so.NewOnlineStream(s.rec)
}

// Accept feeds samples and returns the current partial hypothesis.
func (s *Streaming) Accept(samples []float32) string {
	if s.stream == nil {
		s.NewStream()
	}
	s.stream.AcceptWaveform(sampleRate, samples)
	for s.rec.IsReady(s.stream) {
		s.rec.Decode(s.stream)
	}
	return s.rec.GetResult(s.stream).Text
}

// Finalize flushes the stream with trailing silence and returns its text.
func (s *Streaming) Finalize(tailSeconds float64) string {
	if s.stream == nil {
		return ""
	}
	s.stream.AcceptWaveform(sampleRate, make([]float32, int(tailSeconds*sampleRate)))
	for s.rec.IsReady(s.stream) {
		s.rec.Decode(s.stream)
	}
	return s.rec.GetResult(s.stream).Text
}

// Close frees the recognizer.
func (s *Streaming) Close() {
	if s.stream != nil {
		so.DeleteOnlineStream(s.stream)
		s.stream = nil
	}
	if s.rec != nil {
		so.DeleteOnlineRecognizer(s.rec)
		s.rec = nil
	}
}

// Offline is the English Parakeet TDT recognizer. It is safe for concurrent
// use; decodes are serialized.
type Offline struct {
	mu  sync.Mutex
	rec *so.OfflineRecognizer
}

// NewOffline loads the Parakeet model directory.
func NewOffline(dir string, threads int) (*Offline, error) {
	if err := requireFiles(dir, "tokens.txt", "encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx"); err != nil {
		return nil, err
	}
	cfg := so.OfflineRecognizerConfig{
		FeatConfig: so.FeatureConfig{SampleRate: sampleRate, FeatureDim: 80},
		ModelConfig: so.OfflineModelConfig{
			Transducer: so.OfflineTransducerModelConfig{
				Encoder: filepath.Join(dir, "encoder.int8.onnx"),
				Decoder: filepath.Join(dir, "decoder.int8.onnx"),
				Joiner:  filepath.Join(dir, "joiner.int8.onnx"),
			},
			Tokens:     filepath.Join(dir, "tokens.txt"),
			NumThreads: threads,
			Provider:   "cpu",
			ModelType:  "nemo_transducer",
		},
		DecodingMethod: "greedy_search",
		MaxActivePaths: 4,
	}
	rec := so.NewOfflineRecognizer(&cfg)
	if rec == nil {
		return nil, fmt.Errorf("cannot load offline recognizer from %s", dir)
	}
	return &Offline{rec: rec}, nil
}

// Transcribe decodes one utterance.
func (o *Offline) Transcribe(samples []float32) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rec == nil {
		return "", errors.New("offline recognizer closed")
	}
	stream := so.NewOfflineStream(o.rec)
	defer so.DeleteOfflineStream(stream)
	stream.AcceptWaveform(sampleRate, samples)
	o.rec.Decode(stream)
	return stream.GetResult().Text, nil
}

// Close frees the recognizer after any in-flight decode.
func (o *Offline) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rec != nil {
		so.DeleteOfflineRecognizer(o.rec)
		o.rec = nil
	}
}

// Refiner lazily loads the offline recognizer on first use (or Warm).
// A missing model is an install error, never a silent model switch.
type Refiner struct {
	dir     string
	threads int
	mu      sync.Mutex
	engine  *Offline
}

// NewRefiner prepares a lazily loaded offline recognizer.
func NewRefiner(dir string, threads int) *Refiner {
	return &Refiner{dir: dir, threads: threads}
}

func (r *Refiner) load() (*Offline, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.engine == nil {
		e, err := NewOffline(r.dir, r.threads)
		if err != nil {
			return nil, err
		}
		r.engine = e
	}
	return r.engine, nil
}

// Warm loads the model off the capture-critical path.
func (r *Refiner) Warm() error {
	_, err := r.load()
	return err
}

// Transcribe re-decodes one turn.
func (r *Refiner) Transcribe(samples []float32) (string, error) {
	e, err := r.load()
	if err != nil {
		return "", err
	}
	return e.Transcribe(samples)
}

// Close frees the loaded model.
func (r *Refiner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.engine != nil {
		r.engine.Close()
		r.engine = nil
	}
}
