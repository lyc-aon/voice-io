"""Authoritative English turn re-decode after streaming preview.

Missing Parakeet weights are an install error, not a silent model switch.
"""

from __future__ import annotations

import threading

import numpy as np

from voice_io.asr import OfflineASR
from voice_io.config import Config


class TurnRefiner:
    def __init__(self, cfg: Config) -> None:
        self._cfg = cfg
        self._engine: OfflineASR | None = None
        self._lock = threading.Lock()

    def _engine_for(self) -> OfflineASR:
        if self._engine is None:
            path = self._cfg.resolve(self._cfg.asr.final_en)
            if not path.exists():
                raise FileNotFoundError(
                    f"Final ASR model not found: {path}; run voice-io models install")
            self._engine = OfflineASR(path, self._cfg.asr.threads)
        return self._engine

    def warm(self) -> None:
        """Load the English engine off the critical path."""
        with self._lock:
            self._engine_for()

    def transcribe(self, samples: np.ndarray) -> str:
        """Re-decode a turn; the shared recognizer serializes on this lock."""
        with self._lock:
            return self._engine_for().transcribe(samples)
