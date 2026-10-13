"""Turn authority: one utterance per turn, single endpoint authority.

A turn owns one utterance's audio. VAD silence or explicit Stop closes it;
engine duration caps protect resource use, not dictate paste timing. The
daemon opens a new turn for the next utterance without closing the session.
"""

from __future__ import annotations

import time

import numpy as np

from voice_io.audio import SR


class Turn:
    """Audio accumulation for one utterance.

    The turn opens at the trigger (wake / PTT press) with an empty buffer.
    The daemon retains only a short onset ring until speech starts; after
    VAD confirms speech, frames accumulate until the endpoint closes it.
    """

    def __init__(self) -> None:
        self._buf: list[np.ndarray] = []
        self._samples = 0
        self._started: float | None = None

    def open(self, seed: np.ndarray | None = None,
             front_drop_s: float = 0.0) -> None:
        """Open a turn. `seed` pre-seeds the buffer; the first
        `front_drop_s` seconds of the seed (wake tail overlap) are dropped
        before transcription."""
        self._buf = []
        self._samples = 0
        if seed is not None and len(seed):
            drop = int(front_drop_s * SR)
            if drop < len(seed):
                self._buf = [seed[drop:]]
                self._samples = len(seed) - drop
        self._started = time.monotonic()

    def add(self, samples: np.ndarray) -> None:
        if self._started is not None:
            self._buf.append(samples)
            self._samples += len(samples)

    def retain_last(self, seconds: float) -> None:
        """Bound the pre-speech onset buffer without copying sample data."""
        excess = self._samples - int(seconds * SR)
        while excess > 0 and self._buf:
            first = self._buf[0]
            if excess >= len(first):
                excess -= len(first)
                self._samples -= len(first)
                self._buf.pop(0)
            else:
                self._buf[0] = first[excess:]
                self._samples -= excess
                break

    @property
    def is_open(self) -> bool:
        return self._started is not None

    @property
    def seconds(self) -> float:
        if self._started is None:
            return 0.0
        return self._samples / SR

    @property
    def audio(self) -> np.ndarray | None:
        if not self._buf:
            return None
        return self._buf[0] if len(self._buf) == 1 else np.concatenate(self._buf)

    def close(self) -> None:
        self._buf = []
        self._samples = 0
        self._started = None
