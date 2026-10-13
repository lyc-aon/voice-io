"""Private dictation artifacts remain private and expire together."""

import json
import os
import tempfile
import time
from pathlib import Path
from unittest.mock import patch

import numpy as np

from voice_io.recovery import RecoveryStore


def test_legacy_recovery_permissions_and_failed_audio_cleanup():
    with tempfile.TemporaryDirectory(dir=Path.cwd()) as root:
        directory = Path(root) / "recovery"
        spool = directory / "spool"
        audio = directory / "audio"
        spool.mkdir(parents=True)
        audio.mkdir()
        old = spool / "older.json"
        old_audio = audio / "older.wav"
        old.write_text(json.dumps({"kind": "failed", "ts": 1,
                                   "text": "private", "audio": str(old_audio)}))
        old_audio.write_bytes(b"audio")
        for path in (directory, spool, audio, old, old_audio):
            path.chmod(0o775 if path.is_dir() else 0o664)

        store = RecoveryStore(directory)
        for path in (directory, spool, audio):
            assert path.stat().st_mode & 0o077 == 0
        for path in (old, old_audio):
            assert path.stat().st_mode & 0o077 == 0

        recording = Path(store.save_audio(np.zeros(1600, dtype=np.float32)))
        age = time.time() - 90000
        os.utime(recording, (age, age))
        ts = store.spool({"kind": "failed", "text": "recover me", "audio": str(recording)})
        entry = store.latest()
        assert entry["text"] == "recover me"
        assert recording.stat().st_mode & 0o077 == 0
        assert all(p.stat().st_mode & 0o077 == 0 for p in spool.glob("*.json"))
        store.drop(ts)
        assert not recording.exists()
        assert store.latest() is None  # the legacy entry has expired
        assert not old.exists() and not old_audio.exists()


def test_recovery_file_cannot_delete_external_audio():
    with tempfile.TemporaryDirectory(dir=Path.cwd()) as root:
        directory = Path(root) / "recovery"
        external = Path(root) / "unrelated.wav"
        external.write_bytes(b"keep")
        store = RecoveryStore(directory)
        ts = store.spool({"kind": "failed", "text": "draft", "audio": str(external)})
        store.drop(ts)
        orphan = store.audio_dir / "orphan.wav"
        orphan.write_bytes(b"abandoned private audio")
        age = time.time() - 90000
        os.utime(orphan, (age, age))
        store.prune()
        assert not orphan.exists()
        assert external.read_bytes() == b"keep"

def test_close_timestamps_drop_only_the_requested_recovery_entry():
    with tempfile.TemporaryDirectory(dir=Path.cwd()) as root:
        store = RecoveryStore(Path(root) / "recovery")
        with patch("voice_io.recovery.time.time_ns",
                   side_effect=[1_800_000_000_000_000_001,
                                1_800_000_000_000_000_002]):
            first = store.spool({"kind": "draft", "text": "first"})
            second = store.spool({"kind": "commit", "text": "second"})
        assert first != second
        store.drop(first)
        assert store.latest()["text"] == "second"
        store.drop(second)
        assert store.latest() is None
