"""Non-verbal WAV cues generated locally and played with afplay/aplay.

No TTS, ffmpeg, or network service required.
"""

from __future__ import annotations

import math
import os
import subprocess
import sys
import wave
from array import array
from pathlib import Path

# name -> (frequency Hz, duration s)
CUE_SPECS: dict[str, tuple[float, float]] = {
    "listening": (880, 0.10),
    "executed": (660, 0.12),
    "approval": (520, 0.20),
    "input": (440, 0.12),
    "error": (220, 0.30),
    "busy": (330, 0.20),
}

BASE_DIR = ((Path.home() / "Library" / "Application Support")
            if sys.platform == "darwin" else
            Path(os.environ.get("XDG_DATA_HOME", Path.home() / ".local" / "share"))) / "voice-io" / "cues"


class CuePlayer:
    def __init__(self, dry_run: bool = False, base_dir: Path = BASE_DIR,
                 enabled: bool = True):
        self.dry_run = dry_run
        self.enabled = enabled
        self._dir = base_dir
        self._warned = False
        self._ensure()

    def set_enabled(self, enabled: bool) -> None:
        self.enabled = enabled
        if enabled:
            self._ensure()

    def _ensure(self) -> None:
        if self.dry_run or not self.enabled:
            return
        self._dir.mkdir(parents=True, exist_ok=True)
        sample_rate = 16000
        for name, (freq, duration) in CUE_SPECS.items():
            out = self._dir / f"{name}.wav"
            if out.exists():
                continue
            count = int(duration * sample_rate)
            fade = sample_rate // 200
            samples = array("h", (
                int(6500 * min(1.0, i / fade, (count - 1 - i) / fade)
                    * math.sin(2 * math.pi * freq * i / sample_rate))
                for i in range(count)))
            with wave.open(str(out), "wb") as audio:
                audio.setnchannels(1)
                audio.setsampwidth(2)
                audio.setframerate(sample_rate)
                audio.writeframes(samples.tobytes())

    def play(self, name: str) -> None:
        if name not in CUE_SPECS:
            return
        if not self.enabled:
            return
        if self.dry_run:
            print(f"  [cue] {name}", flush=True)
            return
        path = self._dir / f"{name}.wav"
        if not path.exists():
            raise RuntimeError(f"Voice cue missing: {path}")
        try:
            player = ["afplay", str(path)] if sys.platform == "darwin" else ["aplay", "-q", str(path)]
            subprocess.Popen(player, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL)
        except OSError as exc:
            if sys.platform == "darwin":
                raise RuntimeError("afplay unavailable for macOS cues") from exc
            if not self._warned:
                self._warned = True
                print("cue playback failed (afplay/aplay missing?)", file=sys.stderr)
