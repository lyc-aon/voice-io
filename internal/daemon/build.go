package daemon

import (
	"fmt"

	"gitlab.com/integral-inference/voice-io/internal/config"
	"gitlab.com/integral-inference/voice-io/internal/llm"
	"gitlab.com/integral-inference/voice-io/internal/meeting"
	"gitlab.com/integral-inference/voice-io/internal/speech"
)

// NewEngines bundles engines with a release function.
func NewEngines(vad VAD, kws Spotter, streaming Streaming, refiner Refiner, proc *meeting.Processor,
	release func()) *Engines {
	return &Engines{VAD: vad, KWS: kws, Streaming: streaming, Refiner: refiner, Meeting: proc, close: release}
}

// BuildEngines loads every dictation model named by cfg. Speaker models load
// lazily on the first meeting.
func BuildEngines(cfg *config.Config) (*Engines, error) {
	resolve := func(p string) (string, error) {
		r, err := cfg.Resolve(p)
		if err != nil {
			return "", fmt.Errorf("invalid model path: %w", err)
		}
		return r, nil
	}
	vadPath, err := resolve(cfg.VAD.Model)
	if err != nil {
		return nil, err
	}
	kwsDir, err := resolve(cfg.KWS.ModelDir)
	if err != nil {
		return nil, err
	}
	streamDir, err := resolve(cfg.ASR.Streaming)
	if err != nil {
		return nil, err
	}
	finalDir, err := resolve(cfg.ASR.FinalEN)
	if err != nil {
		return nil, err
	}
	seg, err := resolve(cfg.Meeting.SegmentationModel)
	if err != nil {
		return nil, err
	}
	emb, err := resolve(cfg.Meeting.EmbeddingModel)
	if err != nil {
		return nil, err
	}
	vad, err := speech.NewVAD(speech.VADConfig{Model: vadPath, Threshold: cfg.VAD.Threshold,
		MinSilenceDuration: cfg.VAD.MinSilenceDuration, MaxSpeechDuration: cfg.VAD.MaxSpeechDuration})
	if err != nil {
		return nil, err
	}
	// The spotter runs on every audio chunk while idle. One thread keeps up
	// easily; more leave ONNX Runtime's worker threads spinning between
	// chunks, which cost a whole core while idle on Windows.
	kws, err := speech.NewKWS(speech.KWSConfig{ModelDir: kwsDir, Chunk: cfg.KWS.Chunk, Score: cfg.KWS.Score,
		Threshold: cfg.KWS.Threshold, WakePhrase: cfg.KWS.WakePhrase, EndPhrase: cfg.KWS.EndPhrase,
		Threads: 1})
	if err != nil {
		vad.Close()
		return nil, err
	}
	stream, err := speech.NewStreaming(streamDir, cfg.ASR.Threads)
	if err != nil {
		vad.Close()
		kws.Close()
		return nil, err
	}
	refiner := speech.NewRefiner(finalDir, cfg.ASR.Threads)
	proc := &meeting.Processor{Segmentation: seg, Embedding: emb, Threads: cfg.ASR.Threads, Refiner: refiner,
		Summarizer: &meeting.Summarizer{Client: &llm.Client{Endpoint: cfg.Meeting.SummaryEndpoint,
			Model: cfg.Meeting.SummaryModel, APIKeyEnv: cfg.Meeting.SummaryAPIKeyEnv}}}
	return NewEngines(vad, kws, stream, refiner, proc, func() {
		vad.Close()
		kws.Close()
		stream.Close()
		refiner.Close()
		proc.Close()
	}), nil
}
