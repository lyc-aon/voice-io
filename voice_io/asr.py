"""English streaming preview and authoritative offline turn recognition.

Streaming decoding uses sherpa-onnx OnlineStream; its VAD endpoint is the
turn authority. Offline Parakeet decoding reads OfflineStream.result after
one decode_stream call.
"""

from __future__ import annotations

from pathlib import Path

import numpy as np
import sherpa_onnx


class StreamingASR:
    def __init__(self, model_dir: Path, threads: int = 4):
        m = Path(model_dir)
        self._tokens = f"{m}/tokens.txt"
        self._encoder = f"{m}/encoder.int8.onnx"
        self._decoder = f"{m}/decoder.onnx"
        self._joiner = f"{m}/joiner.int8.onnx"
        self._threads = threads
        self._bpe = f"{m}/bpe.model"
        self.rec = sherpa_onnx.OnlineRecognizer.from_transducer(
            tokens=self._tokens,
            encoder=self._encoder,
            decoder=self._decoder,
            joiner=self._joiner,
            num_threads=threads, sample_rate=16000, feature_dim=80,
            decoding_method="greedy_search",
            model_type="zipformer2",
            modeling_unit="bpe",
            bpe_vocab=self._bpe,
        )
        self._stream: object | None = None

    def new_stream(self) -> None:
        self._stream = self.rec.create_stream()

    def recover(self) -> None:
        """After a decode failure: recreate the recognizer's stream state.
        The recognizer itself is a shared immutable model — only the stream
        holds session state — so a fresh stream is a complete reset."""
        try:
            self._stream = self.rec.create_stream()
        except Exception:
            # Last-resort: rebuild the recognizer (e.g. corrupted internal
            # buffer). Same model files, so this is cheap in practice.
            self.rec = sherpa_onnx.OnlineRecognizer.from_transducer(
                tokens=f"{self._tokens}",
                encoder=self._encoder,
                decoder=self._decoder,
                joiner=self._joiner,
                num_threads=self._threads, sample_rate=16000,
                feature_dim=80, decoding_method="greedy_search",
                model_type="zipformer2", modeling_unit="bpe",
                bpe_vocab=self._bpe,
            )
            self._stream = self.rec.create_stream()

    def accept(self, samples: np.ndarray) -> str:
        """Feed float32 samples; decode; return current partial text."""
        self._stream.accept_waveform(16000, samples)
        while self.rec.is_ready(self._stream):
            self.rec.decode_stream(self._stream)
        return self.rec.get_result(self._stream)

    def result(self) -> str:
        """Current cumulative hypothesis of the live stream.

        No flush, no reset: the decoder keeps its context across paste
        boundaries, so a commit mid-word never re-opens cold (greedy_search
        emits tokens monotonically, so the hypothesis only grows).
        """
        return self.rec.get_result(self._stream)

    def finalize(self, tail_s: float = 0.5) -> str:
        """Flush the stream with trailing silence -> final transcript."""
        self._stream.accept_waveform(
            16000, np.zeros(int(tail_s * 16000), np.float32))
        while self.rec.is_ready(self._stream):
            self.rec.decode_stream(self._stream)
        return self.rec.get_result(self._stream)

    def is_endpoint(self) -> bool:
        return bool(self.rec.is_endpoint(self._stream))


class OfflineASR:
    """English Parakeet TDT offline recognizer."""

    def __init__(self, model_dir: Path, threads: int = 4):
        m = Path(model_dir)
        if not (m / "tokens.txt").is_file():
            raise ValueError(f"Parakeet model tokens not found: {m}")
        self.rec = sherpa_onnx.OfflineRecognizer.from_transducer(
            encoder=str(m / "encoder.int8.onnx"),
            decoder=str(m / "decoder.int8.onnx"),
            joiner=str(m / "joiner.int8.onnx"),
            tokens=str(m / "tokens.txt"),
            num_threads=threads, provider="cpu",
            decoding_method="greedy_search",
            model_type="nemo_transducer",
        )

    def transcribe(self, samples: np.ndarray) -> str:
        stream = self.rec.create_stream()
        stream.accept_waveform(16000, samples)
        self.rec.decode_stream(stream)
        return stream.result.text
