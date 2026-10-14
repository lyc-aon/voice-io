# Third-party rights and distribution notes

The source checkout contains **no model weights**. `voice-io models install` downloads pinned model artifacts to each machine and checks their SHA-256 hashes (sources, revisions and hashes: [`internal/models/catalog.go`](internal/models/catalog.go)). The MIT [LICENSE](LICENSE) covers Voice IO's own code only; it does not change model or dependency terms.

## Bundled with the app

`scripts/build-macos-app` and `scripts/install` copy the license file of every linked Go module into the installed bundle (`Voice IO.app/Contents/Resources/Licenses/`, or `~/.local/lib/voice-io/licenses/` on Linux) together with this file.

| Component | License | How it ships |
| --- | --- | --- |
| [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx) runtime (`libsherpa-onnx-c-api`) and Go bindings ([sherpa-onnx-go](https://github.com/k2-fsa/sherpa-onnx-go)) | Apache-2.0 | Shared library in `Contents/Frameworks` (macOS) or `~/.local/lib/voice-io/lib` (Linux); Go bindings linked into the binary. |
| [ONNX Runtime](https://github.com/microsoft/onnxruntime) (`libonnxruntime`) | MIT, © Microsoft Corporation (text below) | Shared library next to sherpa-onnx. |
| [malgo](https://github.com/gen2brain/malgo) with [miniaudio](https://miniaud.io) | Unlicense / public domain (miniaudio: public domain or MIT No Attribution) | Linked into the binary. |
| [BurntSushi/toml](https://github.com/BurntSushi/toml) | MIT | Linked into the binary. |
| [godbus/dbus](https://github.com/godbus/dbus) (Linux) | BSD-2-Clause | Linked into the Linux binary. |
| GTK 3, cairo, Pango (Linux) | LGPL-2.1+ / LGPL-2.1 or MPL-1.1 | Dynamically linked from the system; not shipped. |
| App icon | Generated for this project with OpenAI `gpt-image-2` (Codex image generation); source SHA-256 `017d5029…842bbd` | `packaging/macos/VoiceIO.icns`, `packaging/linux/voice-io.png`. |

### ONNX Runtime license

```
MIT License

Copyright (c) Microsoft Corporation

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## Downloaded models (never bundled)

| Model | Upstream rights | Notes |
| --- | --- | --- |
| Silero VAD v5 | [Silero project](https://github.com/snakers4/silero-vad/blob/master/LICENSE): MIT; the downloaded [ONNX mirror](https://huggingface.co/csukuangfj/vad) does not declare a separate artifact license | Confirm the mirror's artifact provenance and license before repackaging it. |
| English Parakeet TDT 0.6B v2 | [NVIDIA original](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2) and [ONNX export](https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8): CC-BY-4.0 | Commercial use allowed with attribution and license notice. |
| X-ASR low-latency preview | [Original model card](https://huggingface.co/GilgameshWind/X-ASR-zh-en): Apache-2.0; ONNX from the [sherpa-onnx release](https://github.com/k2-fsa/sherpa-onnx/releases/tag/asr-models) | Retain notices if repackaging. |
| Sherpa zh-en keyword spotter | [Author's clarification](https://github.com/k2-fsa/sherpa-onnx/issues/3802): ModelScope listing updated to Apache-2.0; the release archive itself lacks a LICENSE/README | Rights and provenance for its bundled `en.phone` lexicon and training data have not been established for redistribution. |
| Pyannote segmentation 3.0 ONNX | [Original model card](https://huggingface.co/pyannote/segmentation-3.0): MIT (gated at origin); the [ONNX release archive](https://github.com/k2-fsa/sherpa-onnx/releases/tag/speaker-segmentation-models) includes an MIT `LICENSE`, © 2022 CNRS | Optional, for meeting notes and collaborative drafts. |
| NeMo TitaNet-S speaker embedding | [NVIDIA model card](https://catalog.ngc.nvidia.com/orgs/nvidia/teams/nemo/models/titanet_small) points to the [NeMo Apache-2.0 license](https://github.com/NVIDIA/NeMo/blob/main/LICENSE) | Optional, for meeting notes and collaborative drafts. |

No copied product UI assets, personal recordings or model weights belong in the repository. Optional dictation-polish and meeting-summary endpoints have their **own** model and API terms; configuring one sends text to it. Each user downloads the models directly from their upstream sources. If you redistribute model files (for example in an installer that bundles them), first clear the two provenance gaps above and include the upstream notices.
