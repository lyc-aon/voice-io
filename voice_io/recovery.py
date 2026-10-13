"""Private crash-safe recovery spool for dictation sessions.

Store: <dir>/spool/<epoch-ns>-<kind>.json (atomic per-entry replace) +
<dir>/audio/<epoch-ns>.wav for failed pastes.

What gets spooled:
- draft  — completed turns awaiting final insertion; replaced as turns finish,
           dropped on cancel or successful finalization.
- commit — finalized text awaiting insertion; dropped after a successful paste.
- failed — finalized text the executor could not paste, with optional audio.

Recovery is explicit: `voice-io.sh recover` copies the newest unexpired entry.
Starting another dictation never prompts, inserts, or discards saved text.
Entries expire after `ttl` (default 24h).
"""

from __future__ import annotations

import json
import os
import time
import tempfile
from pathlib import Path

import numpy as np

from voice_io.audio import write_wav

_DEFAULT_DIR = Path.home() / ".local" / "share" / "voice-io" / "recovery"


class RecoveryStore:
    def __init__(self, directory: Path | None = None,
                 ttl: float = 86400.0) -> None:
        self.dir = directory or _DEFAULT_DIR
        self.ttl = ttl
        self.spool_dir = self.dir / "spool"
        self.audio_dir = self.dir / "audio"
        for path in (self.dir, self.spool_dir, self.audio_dir):
            path.mkdir(parents=True, exist_ok=True, mode=0o700)
            if path.is_symlink():
                raise OSError(f"Recovery directory must not be a symlink: {path}")
            path.chmod(0o700)  # also secure directories from older installs
        for pattern, directory in (("*.json", self.spool_dir),
                                   ("*.tmp", self.spool_dir),
                                   ("*.wav", self.audio_dir)):
            for path in directory.glob(pattern):
                if not path.is_symlink() and path.is_file():
                    path.chmod(0o600)

    def _spool_path(self, stamp_ns: int, kind: str) -> Path:
        return self.spool_dir / f"{stamp_ns}-{kind}.json"

    def spool(self, entry: dict) -> int:
        """Persist one entry atomically; return its unique nanosecond drop ID."""
        stamp_ns = time.time_ns()
        entry = dict(entry, id=stamp_ns, ts=stamp_ns / 1_000_000_000)
        target = self._spool_path(stamp_ns, entry.get("kind", "commit"))
        tmp: Path | None = None
        try:
            with tempfile.NamedTemporaryFile(
                    mode="w", encoding="utf-8", dir=self.spool_dir,
                    prefix=".recovery-", suffix=".tmp", delete=False) as f:
                tmp = Path(f.name)
                f.write(json.dumps(entry, ensure_ascii=False) + "\n")
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp, target)
        finally:
            if tmp is not None:
                tmp.unlink(missing_ok=True)
        return stamp_ns

    def save_audio(self, samples: np.ndarray, sr: int = 16000) -> str:
        """Persist a segment wav; returns its path ("" if audio is empty)."""
        if samples is None or len(samples) == 0:
            return ""
        path = self.audio_dir / f"{time.time_ns()}.wav"
        try:
            write_wav(path, samples, sr)
            path.chmod(0o600)
        except BaseException:
            path.unlink(missing_ok=True)
            raise
        return str(path)

    def latest(self) -> dict | None:
        """Newest unexpired entry (any kind), or None."""
        self.prune()
        for f in sorted(self.spool_dir.glob("*.json"), reverse=True):
            if f.is_symlink():
                continue
            try:
                entry = json.loads(f.read_text(encoding="utf-8"))
                if isinstance(entry, dict):
                    return entry
            except (OSError, ValueError):
                continue
        return None

    def _delete_audio(self, entry: dict) -> None:
        audio = entry.get("audio", "")
        if isinstance(audio, str) and audio:
            path = Path(audio)
            if path.parent == self.audio_dir and path.suffix == ".wav":
                path.unlink(missing_ok=True)

    def drop(self, stamp: int | float) -> None:
        """Remove an exact recovery entry and its attached failed-paste audio."""
        for f in self.spool_dir.glob("*.json"):
            if f.is_symlink():
                continue
            try:
                entry = json.loads(f.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if (isinstance(entry, dict)
                    and ((isinstance(stamp, int) and entry.get("id") == stamp)
                         or (isinstance(stamp, float) and "id" not in entry
                             and entry.get("ts") == stamp))):
                f.unlink(missing_ok=True)
                self._delete_audio(entry)
                return

    def prune(self) -> None:
        cutoff = time.time() - self.ttl
        retained_audio: set[Path] = set()
        for f in self.spool_dir.glob("*.json"):
            if f.is_symlink():
                continue
            try:
                entry = json.loads(f.read_text(encoding="utf-8"))
                ts = entry.get("ts", 0.0) if isinstance(entry, dict) else 0.0
            except (OSError, ValueError):
                entry, ts = {}, 0.0
            if not isinstance(ts, (int, float)) or ts < cutoff:
                f.unlink(missing_ok=True)
                if isinstance(entry, dict):
                    self._delete_audio(entry)
            elif isinstance(entry, dict):
                audio = entry.get("audio")
                if isinstance(audio, str) and audio:
                    path = Path(audio)
                    if path.parent == self.audio_dir and path.suffix == ".wav":
                        retained_audio.add(path)
        # Older builds kept failed-paste WAVs after their JSON was removed.
        for path in self.audio_dir.glob("*.wav"):
            if path.is_symlink() or path in retained_audio:
                continue
            try:
                if path.stat().st_mtime < cutoff:
                    path.unlink()
            except OSError:
                continue
