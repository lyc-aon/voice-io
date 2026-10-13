"""Audio I/O: 16 kHz mono float32 sources (mic via sounddevice, wav files)."""

from __future__ import annotations

import wave
from pathlib import Path

import numpy as np

SR = 16000


def read_wav(path: Path) -> tuple[np.ndarray, int]:
    """Read a 16-bit mono wav -> (float32 samples in [-1,1], sample_rate)."""
    with wave.open(str(path)) as f:
        assert f.getnchannels() == 1, f"expected mono: {path}"
        assert f.getsampwidth() == 2, f"expected 16-bit: {path}"
        samples = np.frombuffer(f.readframes(f.getnframes()),
                                dtype=np.int16).astype(np.float32) / 32768
        return samples, f.getframerate()


def write_wav(path: Path, samples: np.ndarray, sr: int = SR) -> None:
    with wave.open(str(path), "wb") as f:
        f.setnchannels(1)
        f.setsampwidth(2)
        f.setframerate(sr)
        f.writeframes((samples * 32767).astype(np.int16).tobytes())


class MicSource:
    """Blocking 16 kHz mono mic reader (PipeWire via sounddevice)."""

    def __init__(self, device: str | None = None):
        import sounddevice as sd
        self._sd = sd
        self._stream = sd.InputStream(
            channels=1, dtype="float32", samplerate=SR, device=device)
        self._stream.start()

    def read(self, seconds: float = 0.1) -> np.ndarray:
        data, _ = self._stream.read(int(seconds * SR))
        return data.reshape(-1)

    def close(self) -> None:
        self._stream.stop()
        self._stream.close()


class WavSource:
    """Chunked reader over a wav file; read() returns None at EOF."""

    def __init__(self, path: Path):
        self._f = wave.open(str(path))
        assert self._f.getnchannels() == 1
        assert self._f.getsampwidth() == 2
        self._sr = self._f.getframerate()

    @property
    def sample_rate(self) -> int:
        return self._sr

    def read(self, n: int) -> np.ndarray | None:
        raw = self._f.readframes(n)
        if not raw:
            return None
        return np.frombuffer(raw, dtype=np.int16).astype(np.float32) / 32768

    def close(self) -> None:
        self._f.close()
