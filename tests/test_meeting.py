"""Meeting capture, transcript publication, and optional API failure behavior."""

import json
import os
import tempfile
import threading
import wave
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import patch

import numpy as np
import pytest

from voice_io.config import Config
from voice_io.meeting import (MeetingProcessor, MeetingRecorder, SpeechLine,
                              _chunks, collaborative_text, transcript_text)


def test_collaborative_draft_preserves_speaker_turns_without_timestamps():
    lines = [
        SpeechLine(0, 1, 0, "Let's review the plan."),
        SpeechLine(1.01, 2, 0, "Start with design."),
        SpeechLine(2, 3, 1, "I agree."),
        SpeechLine(3, 4, 0, "Then we can ship."),
    ]
    assert collaborative_text(lines) == (
        "Speaker 1: Let's review the plan. Start with design.\n"
        "Speaker 2: I agree.\n"
        "Speaker 1: Then we can ship."
    )
    assert transcript_text(lines).startswith("[00:00:00–00:00:01]")


def test_collaborative_labels_sparse_diarizer_ids_in_first_appearance_order():
    lines = [
        SpeechLine(0, 1, 3, "First voice."),
        SpeechLine(1, 2, 0, "Second voice."),
        SpeechLine(2, 3, 3, "First voice again."),
    ]
    assert collaborative_text(lines) == (
        "Speaker 1: First voice.\n"
        "Speaker 2: Second voice.\n"
        "Speaker 1: First voice again."
    )
    assert "Speaker 4: First voice." in transcript_text(lines)


def test_collaborative_draft_merges_only_contiguous_adjacent_chunks():
    lines = [
        SpeechLine(0, 1, 0, "One"),
        SpeechLine(1.04, 2, 0, "two"),
        SpeechLine(2.051, 3, 0, "new turn"),
        SpeechLine(2.9, 3.5, 0, "overlap"),
        SpeechLine(3.5, 4, 1, "another speaker"),
        SpeechLine(4, 5, 0, "back again"),
    ]
    assert collaborative_text(lines) == (
        "Speaker 1: One two\n"
        "Speaker 1: new turn\n"
        "Speaker 1: overlap\n"
        "Speaker 2: another speaker\n"
        "Speaker 1: back again"
    )


def test_collaborative_draft_cleans_each_line_and_omits_empty_speech():
    lines = [
        SpeechLine(0, 1, -1, "um"),
        SpeechLine(1, 2, -1, " um  kubernetees "),
        SpeechLine(2, 3, -1, "smiling emoji"),
        SpeechLine(3, 4, 2, "   "),
        SpeechLine(4, 5, -1, "hello"),
    ]
    assert collaborative_text(lines, words={"kubernetees": "Kubernetes"}) == (
        "Unknown speaker: Kubernetes 😊\nUnknown speaker: hello"
    )
    assert collaborative_text([SpeechLine(0, 1, -1, "um")]) == ""
    assert collaborative_text([]) == ""
    assert collaborative_text([SpeechLine(0, 1, -1, "um kubernetees smiling emoji")],
                              filler=False, emoji=False) == (
        "Unknown speaker: um kubernetees smiling emoji"
    )


def test_long_transcript_chunks_preserve_every_speaker_line():
    lines = [f"[00:{i // 60:02}:{i % 60:02}] Speaker {i % 3 + 1}: " + "topic " * 32
             for i in range(280)]
    chunks = _chunks(lines)
    assert len(chunks) > 1
    assert all(len(part) <= 12000 for part in chunks)
    assert "\n".join(chunks) == "\n".join(lines)


def test_meeting_settings_save_and_reject_unsafe_values():
    with tempfile.TemporaryDirectory(dir=Path.cwd()) as root:
        config = Path(root) / "config.toml"
        config.write_text('[ui]\nsounds = false\n# keep this\n')
        Config.save_user({"meeting": {
            "mode": "meeting", "summary_endpoint": "http://localhost:1234/v1",
            "summary_model": "local-large", "summary_api_key_env": "VOICE_IO_TEST_KEY",
        }}, config)
        current = Config.load(config)
        assert current.meeting.mode == "meeting"
        assert current.meeting.summary_model == "local-large"
        assert current.ui.sounds is False
        before = config.read_text()
        assert "# keep this" in before
        Config.save_user({"meeting": {"mode": "collaborative"}}, config)
        assert Config.load(config).meeting.mode == "collaborative"
        Config.save_user({"meeting": {"mode": "dictation"}}, config)
        assert Config.load(config).meeting.mode == "dictation"
        assert Config.load(Path(root) / "missing.toml").meeting.mode == "dictation"
        before = config.read_text()
        for field, bad in (("mode", "streaming"),
                           ("summary_endpoint", "file:///private/transcript"),
                           ("summary_endpoint", "https://secret@api.example/v1"),
                           ("summary_api_key_env", "KEY; echo bad")):
            with pytest.raises(ValueError):
                Config.save_user({"meeting": {field: bad}}, config)
            assert config.read_text() == before


def test_capture_and_cancel_remove_private_audio():
    with tempfile.TemporaryDirectory(dir=Path.cwd()) as root:
        recorder = MeetingRecorder(Path(root) / "meetings")
        recorder.append(np.full(16000, .25, dtype=np.float32))
        assert recorder.path.stat().st_mode & 0o077 == 0
        assert recorder.directory.stat().st_mode & 0o077 == 0
        filename = recorder.close()
        assert filename.is_file()
        again = MeetingRecorder(recorder.directory)
        again.append(np.ones(1600, dtype=np.float32))
        path = again.path
        again.discard()
        assert not path.exists()


def test_open_recording_is_readable_after_periodic_disk_sync():
    with tempfile.TemporaryDirectory(dir=Path.cwd()) as root:
        recorder = MeetingRecorder(Path(root) / "meetings")
        recorder.append(np.full(6 * 16000, .25, dtype=np.float32))
        try:
            with wave.open(str(recorder.path)) as recording:
                assert recording.getnframes() == 6 * 16000
                assert len(recording.readframes(recording.getnframes())) == 12 * 16000
        finally:
            recorder.discard()


def test_transcript_keeps_speaker_timestamps_and_drops_raw_audio():
    with tempfile.TemporaryDirectory(dir=Path.cwd()) as root:
        cfg = Config()
        processor = MeetingProcessor(cfg, refiner=None)
        processor.diarize = lambda samples, cancelled: [
            SpeechLine(1.2, 4.5, 0, "First speaker proposed a review."),
            SpeechLine(4.6, 6.5, 1, "Second speaker agreed.")]
        recorder = MeetingRecorder(Path(root) / "meetings")
        recorder.append(np.full(16000, .25, dtype=np.float32))
        recording = recorder.close()
        document, error = processor.process(recording)
        text = document.read_text()
        assert error is None
        assert "## Summary" not in text
        assert "[00:00:01–00:00:04] Speaker 1: First speaker" in text
        assert "[00:00:05–00:00:06] Speaker 2: Second speaker" in text
        assert document.stat().st_mode & 0o077 == 0
        assert not recording.exists()


def test_openai_compatible_summary_and_credential_failure_keep_transcript():
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers["Content-Length"])
            payload = json.loads(self.rfile.read(length))
            requests.append((self.path, self.headers.get("Authorization"), payload))
            content = b'{"choices":[{"message":{"content":"Decision: approve the review."}}]}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(content)))
            self.end_headers()
            self.wfile.write(content)

        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", 0), Handler)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        with tempfile.TemporaryDirectory(dir=Path.cwd()) as root:
            cfg = Config()
            cfg.meeting.summary_endpoint = f"http://127.0.0.1:{server.server_port}/v1"
            cfg.meeting.summary_model = "local-summarizer"
            cfg.meeting.summary_api_key_env = "VOICE_IO_MEETING_TEST_KEY"
            processor = MeetingProcessor(cfg, refiner=None)
            processor.diarize = lambda samples, cancelled: [
                SpeechLine(0, 2, 0, "We agreed to review the work.")]
            with patch.dict(os.environ, {cfg.meeting.summary_api_key_env: "secret"}):
                recorder = MeetingRecorder(Path(root) / "meetings")
                recorder.append(np.ones(16000, dtype=np.float32))
                document, error = processor.process(recorder.close())
            assert error is None
            assert "## Summary\n\nDecision: approve the review." in document.read_text()
            assert requests[0][0] == "/v1/chat/completions"
            assert requests[0][1] == "Bearer secret"
            assert requests[0][2]["model"] == "local-summarizer"
            assert "Speaker 1" in requests[0][2]["messages"][1]["content"]

            with patch.dict(os.environ, {}, clear=True):
                recorder = MeetingRecorder(Path(root) / "meetings")
                recorder.append(np.ones(16000, dtype=np.float32))
                document, error = processor.process(recorder.close())
            assert "VOICE_IO_MEETING_TEST_KEY" in error
            assert "## Summary" not in document.read_text()
            assert "## Transcript" in document.read_text()
            assert len(requests) == 1
    finally:
        server.shutdown()
        worker.join(timeout=3)
        server.server_close()
