package speech

/*
#cgo linux LDFLAGS: -ldl
#include <stdint.h>
#include "diarize_shim.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"runtime/cgo"
	"sort"
	"unsafe"

	so "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// ErrCancelled reports that a caller cancelled long-running processing.
var ErrCancelled = errors.New("cancelled")

// Region is one diarized speech span.
type Region struct {
	Start, End float64 // seconds
	Speaker    int
}

// Diarizer labels speakers in a recording (pyannote segmentation + TitaNet
// embeddings + fast clustering).
type Diarizer struct {
	sd *so.OfflineSpeakerDiarization
}

// NewDiarizer loads both speaker models.
func NewDiarizer(segmentation, embedding string, threads int) (*Diarizer, error) {
	for _, p := range []string{segmentation, embedding} {
		if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("speaker models missing; run voice-io models install --meeting")
		}
	}
	cfg := so.OfflineSpeakerDiarizationConfig{
		Segmentation: so.OfflineSpeakerSegmentationModelConfig{
			Pyannote:   so.OfflineSpeakerSegmentationPyannoteModelConfig{Model: segmentation, WindowShiftRatio: 0.1},
			NumThreads: threads,
			Provider:   "cpu",
		},
		Embedding:      so.SpeakerEmbeddingExtractorConfig{Model: embedding, NumThreads: threads, Provider: "cpu"},
		Clustering:     so.FastClusteringConfig{NumClusters: -1, Threshold: 0.5},
		MinDurationOn:  0.3,
		MinDurationOff: 0.5,
	}
	sd := so.NewOfflineSpeakerDiarization(&cfg)
	if sd == nil {
		return nil, errors.New("meeting speaker models could not be loaded")
	}
	return &Diarizer{sd: sd}, nil
}

// impl reads the C handle held by the binding's single-pointer struct.
func (d *Diarizer) impl() unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(d.sd))
}

//export vioDiarizeProgress
func vioDiarizeProgress(done, total C.int32_t, arg unsafe.Pointer) C.int32_t {
	_, _ = done, total
	cancelled := cgo.Handle(uintptr(arg)).Value().(func() bool)
	if cancelled() {
		return 1
	}
	return 0
}

// Process diarizes samples. cancelled is polled between model chunks; when
// it returns true processing stops with ErrCancelled.
func (d *Diarizer) Process(samples []float32, cancelled func() bool) ([]Region, error) {
	if len(samples) == 0 {
		return nil, nil
	}
	if cancelled == nil {
		cancelled = func() bool { return false }
	}
	h := cgo.NewHandle(cancelled)
	defer h.Delete()
	var segs *C.vio_segment
	var count C.int32_t
	rc := C.vio_diarize(d.impl(), (*C.float)(unsafe.Pointer(&samples[0])), C.int32_t(len(samples)),
		C.uintptr_t(h), &segs, &count)
	if cancelled() {
		if segs != nil {
			C.vio_free_segments(segs)
		}
		return nil, ErrCancelled
	}
	if rc != 0 {
		return nil, fmt.Errorf("speaker diarization unavailable (code %d)", int(rc))
	}
	if segs == nil || count == 0 {
		return nil, nil
	}
	defer C.vio_free_segments(segs)
	raw := unsafe.Slice(segs, int(count))
	out := make([]Region, len(raw))
	for i, s := range raw {
		out[i] = Region{Start: float64(s.start), End: float64(s.end), Speaker: int(s.speaker)}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, nil
}

// Close frees the models.
func (d *Diarizer) Close() {
	if d.sd != nil {
		so.DeleteOfflineSpeakerDiarization(d.sd)
		d.sd = nil
	}
}
