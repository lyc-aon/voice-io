"""Silero VAD v5 via sherpa-onnx VoiceActivityDetector.

Verified API (sherpa-onnx 1.13.8): feed 512-sample chunks;
front().samples is a LIST, front().start is the absolute sample index;
drain with pop(); flush() at stream end.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import numpy as np
import sherpa_onnx

CHUNK = 512  # 32 ms @ 16 kHz


@dataclass
class Segment:
    start: int  # sample index within the VAD stream
    samples: np.ndarray  # float32 speech samples

    @property
    def end(self) -> int:
        return self.start + len(self.samples)

    @property
    def duration(self) -> float:
        return len(self.samples) / 16000


class VAD:
    def __init__(self, model_path: Path, threshold: float = 0.5,
                 min_silence_duration: float = 0.25,
                 max_speech_duration: float = 20.0):
        cfg = sherpa_onnx.VadModelConfig()
        cfg.silero_vad.model = str(model_path)
        cfg.silero_vad.threshold = threshold
        cfg.silero_vad.min_silence_duration = min_silence_duration
        cfg.silero_vad.max_speech_duration = max_speech_duration
        cfg.sample_rate = 16000
        self.det = sherpa_onnx.VoiceActivityDetector(cfg)

    def accept(self, samples: np.ndarray) -> None:
        """Feed arbitrary-length float32 audio; chunked internally to 512.

        Note: sherpa-onnx 1.13.8 VoiceActivityDetector.accept_waveform takes
        only samples (the sample rate lives in VadModelConfig).
        """
        for i in range(0, len(samples), CHUNK):
            self.det.accept_waveform(samples[i:i + CHUNK])

    def flush(self) -> None:
        self.det.flush()

    def clear(self) -> None:
        self.det.reset()

    def pop_segments(self) -> list[Segment]:
        segs = []
        while not self.det.empty():
            front = self.det.front
            segs.append(Segment(start=front.start,
                                samples=np.asarray(front.samples,
                                                   dtype=np.float32)))
            self.det.pop()
        return segs

    def is_speech(self) -> bool:
        return bool(self.det.is_speech_detected())
