// Package models installs and verifies pinned offline model weights.
//
// Weights are never packaged. Install downloads each artifact from a pinned
// Hugging Face revision or GitHub release, checks its SHA-256 while
// streaming, extracts only the required files from archives, and publishes a
// model directory atomically. Starting the daemon never downloads anything.
package models

import (
	"strings"
)

// Group selects which features a model serves.
type Group string

const (
	Core    Group = "core"    // dictation: VAD, wake words, live preview, final ASR
	Meeting Group = "meeting" // meeting and collaborative speaker labels
)

// Artifact is one downloaded object.
type Artifact struct {
	URL    string
	SHA256 string
	Size   int64
	// Archive artifacts are .tar.bz2 files whose required members live in a
	// top-level directory named after the model. Others are saved as File.
	Archive bool
	File    string
}

// Model is one installable unit under the models directory.
type Model struct {
	ID         string
	Title      string
	Detail     string
	Group      Group
	Relative   string   // directory or single file under the models root
	Required   []string // files that must be present and non-empty
	Artifacts  []Artifact
	License    string
	LicenseURL string
}

// Directory reports whether the model installs as a directory.
func (m Model) Directory() bool { return !strings.HasSuffix(m.Relative, ".onnx") }

// DownloadSize is the total bytes fetched for the model.
func (m Model) DownloadSize() int64 {
	var n int64
	for _, a := range m.Artifacts {
		n += a.Size
	}
	return n
}

const (
	kwsName      = "sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20"
	streamName   = "sherpa-onnx-x-asr-480ms-streaming-zipformer-transducer-zh-en-int8-2026-06-05"
	finalName    = "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8"
	segName      = "sherpa-onnx-pyannote-segmentation-3-0"
	embedName    = "nemo_en_titanet_small.onnx"
	vadRepo      = "csukuangfj/vad"
	vadRevision  = "fba88cd2e921609e7675c3aaf51e0b9b295da4bc"
	finalRepo    = "csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8"
	finalRev     = "1ab9323565ddb038682214b292f588070a538ce2"
	sherpaGitHub = "https://github.com/k2-fsa/sherpa-onnx/releases/download/"
)

func hf(repo, rev, file, sha string, size int64) Artifact {
	return Artifact{URL: "https://huggingface.co/" + repo + "/resolve/" + rev + "/" + file,
		SHA256: sha, Size: size, File: file}
}

// Catalog lists every model in install order.
var Catalog = []Model{
	{
		ID: "vad", Title: "Voice activity", Detail: "Silero VAD v5", Group: Core,
		Relative: "silero_vad_v5.onnx", Required: []string{"silero_vad_v5.onnx"},
		Artifacts: []Artifact{hf(vadRepo, vadRevision, "silero_vad_v5.onnx",
			"6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396", 2313101)},
		License: "MIT (Silero); mirror artifact license undeclared", LicenseURL: "https://github.com/snakers4/silero-vad/blob/master/LICENSE",
	},
	{
		ID: "kws", Title: "Wake and end phrases", Detail: "Zipformer keyword spotter (3M)", Group: Core,
		Relative: kwsName,
		Required: []string{"tokens.txt", "en.phone",
			"encoder-epoch-13-avg-2-chunk-16-left-64.int8.onnx",
			"decoder-epoch-13-avg-2-chunk-16-left-64.onnx",
			"joiner-epoch-13-avg-2-chunk-16-left-64.int8.onnx",
			"encoder-epoch-13-avg-2-chunk-8-left-64.int8.onnx",
			"decoder-epoch-13-avg-2-chunk-8-left-64.onnx",
			"joiner-epoch-13-avg-2-chunk-8-left-64.int8.onnx"},
		Artifacts: []Artifact{{URL: sherpaGitHub + "kws-models/" + kwsName + ".tar.bz2",
			SHA256: "68447f4fbc67e70eee3a93961f36e81e98f47aef73ce7e7ca00885c6cd3616a6",
			Size:   32885699, Archive: true}},
		License: "Apache-2.0 (per author)", LicenseURL: "https://github.com/k2-fsa/sherpa-onnx/issues/3802",
	},
	{
		ID: "streaming", Title: "Live preview", Detail: "X-ASR 480 ms streaming transducer", Group: Core,
		Relative: streamName,
		Required: []string{"tokens.txt", "bpe.model", "encoder.int8.onnx", "decoder.onnx", "joiner.int8.onnx"},
		Artifacts: []Artifact{{URL: sherpaGitHub + "asr-models/" + streamName + ".tar.bz2",
			SHA256: "14919f03b74d9001fc8f2689c2ab1ea42c436db269e8c809144492f41612bfff",
			Size:   134369642, Archive: true}},
		License: "Apache-2.0", LicenseURL: "https://huggingface.co/GilgameshWind/X-ASR-zh-en",
	},
	{
		ID: "final_en", Title: "Transcription", Detail: "NVIDIA Parakeet TDT 0.6B v2 (int8)", Group: Core,
		Relative: finalName,
		Required: []string{"tokens.txt", "encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx"},
		Artifacts: []Artifact{
			hf(finalRepo, finalRev, "tokens.txt", "ec182b70dd42113aff6c5372c75cac58c952443eb22322f57bbd7f53977d497d", 9384),
			hf(finalRepo, finalRev, "encoder.int8.onnx", "a32b12d17bbbc309d0686fbbcc2987b5e9b8333a7da83fa6b089f0a2acd651ab", 652184296),
			hf(finalRepo, finalRev, "decoder.int8.onnx", "b6bb64963457237b900e496ee9994b59294526439fbcc1fecf705b31a15c6b4e", 7257753),
			hf(finalRepo, finalRev, "joiner.int8.onnx", "7946164367946e7f9f29a122407c3252b680dbae9a51343eb2488d057c3c43d2", 1739080),
		},
		License: "CC-BY-4.0", LicenseURL: "https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2",
	},
	{
		ID: "segmentation", Title: "Speaker turns", Detail: "pyannote segmentation 3.0", Group: Meeting,
		Relative: segName, Required: []string{"model.int8.onnx", "LICENSE"},
		Artifacts: []Artifact{{URL: sherpaGitHub + "speaker-segmentation-models/" + segName + ".tar.bz2",
			SHA256: "24615ee884c897d9d2ba09bb4d30da6bb1b15e685065962db5b02e76e4996488",
			Size:   6958444, Archive: true}},
		License: "MIT", LicenseURL: "https://huggingface.co/pyannote/segmentation-3.0",
	},
	{
		ID: "speaker_embedding", Title: "Speaker voices", Detail: "NVIDIA NeMo TitaNet-S", Group: Meeting,
		Relative: embedName, Required: []string{embedName},
		Artifacts: []Artifact{{URL: sherpaGitHub + "speaker-recongition-models/" + embedName,
			SHA256: "ad4a1802485d8b34c722d2a9d04249662f2ece5d28a7a039063ca22f515a789e",
			Size:   40257283, File: embedName}},
		License: "Apache-2.0 (NeMo)", LicenseURL: "https://catalog.ngc.nvidia.com/orgs/nvidia/teams/nemo/models/titanet_small",
	},
}

// Select returns catalog models in the given groups (all when none given).
func Select(groups ...Group) []Model {
	if len(groups) == 0 {
		return Catalog
	}
	var out []Model
	for _, m := range Catalog {
		for _, g := range groups {
			if m.Group == g {
				out = append(out, m)
				break
			}
		}
	}
	return out
}
