"""Dictation targets the current cursor, even when GNOME hides its identity."""

import queue
import threading
from types import SimpleNamespace
from unittest.mock import patch

import numpy as np

from voice_io.config import Config
from voice_io.daemon import Daemon
from voice_io.executor import Executor, Result


def test_wayland_identity_requires_compositor_window_and_survives_title_changes():
    from voice_io.daemon import _probe_focus

    replies = iter((
        '{\"app\":\"tern\",\"window\":\"gnome:instance:17\",\"title\":\"first\"}',
        '{\"app\":\"tern\",\"window\":\"gnome:instance:17\",\"title\":\"second\"}',
        '{\"app\":\"tern\",\"window\":\"gnome:instance:18\",\"title\":\"second\"}',
        '{\"app\":\"tern\",\"window\":\"unverified-title\",\"title\":\"same\"}',
        ''))
    with (patch("voice_io.daemon.sys.platform", "linux"),
          patch("voice_io.daemon._system_python", return_value="/usr/bin/python3"),
          patch("voice_io.daemon._x11_auth_env", return_value={}),
          patch("voice_io.daemon.subprocess.run",
                side_effect=lambda *_a, **_k: SimpleNamespace(stdout=next(replies)))):
        first = _probe_focus()
        assert first == _probe_focus()
        assert first != _probe_focus()
        assert _probe_focus() is None  # AT-SPI title alone cannot verify Wayland focus
        assert _probe_focus() is None  # no extension or focused client




def test_wayland_unknown_focus_sends_paste_without_submitting_or_losing_clipboard():
    daemon = Daemon.__new__(Daemon)
    daemon.executor = Executor(dry_run=False, auto_submit="enter")
    keys = []
    clipboard = []
    daemon.executor._snapshot_clipboard = lambda: ("text", b"original")
    daemon.executor._spawn_copy = lambda data, mime: clipboard.append(data)
    daemon.executor._verify_serve = lambda text: True
    daemon.executor._restore_clipboard = lambda snap: clipboard.append(snap[1])
    daemon.executor._send_key = lambda chord: keys.append(chord) or True
    daemon.overlay = SimpleNamespace(result=lambda *args, **kwargs: None)
    daemon.cues = SimpleNamespace(play=lambda _: None)
    ev = {}

    with (patch("voice_io.daemon.sys.platform", "linux"),
          patch("voice_io.daemon._probe_focus", return_value=None),
          patch("voice_io.executor.time.sleep", lambda _: None)):
        res = daemon._paste_at_focus("meeting notes", ev, submit=True)

    assert res.ok and ev["inserted"]
    assert keys == ["shift+Insert"]  # no spoken-send or auto-submit Enter
    assert clipboard == [b"meeting notes", b"original"]


def test_paste_targets_newly_focused_tern_instead_of_old_editor():
    daemon = Daemon.__new__(Daemon)
    daemon.executor = Executor(dry_run=False)
    keys = []
    daemon.executor._snapshot_clipboard = lambda: None
    daemon.executor._spawn_copy = lambda *_: None
    daemon.executor._verify_serve = lambda *_: True
    daemon.executor._restore_clipboard = lambda *_: None
    daemon.executor._send_key = lambda chord: keys.append(chord) or True
    daemon.overlay = SimpleNamespace(result=lambda *args, **kwargs: None)
    daemon.cues = SimpleNamespace(play=lambda _: None)
    ev = {}

    with (patch("voice_io.daemon.sys.platform", "linux"),
          patch("voice_io.daemon._probe_focus", return_value="tern|gnome:window:2"),
          patch("voice_io.executor.time.sleep", lambda _: None)):
        res = daemon._paste_at_focus("notes for this pane", ev)

    assert res.ok and ev["inserted"]
    assert keys == ["ctrl+shift+v"]


def test_failed_recovery_spool_preserves_preexisting_committed_text():
    daemon = Daemon.__new__(Daemon)
    daemon._session_lock = threading.Lock()
    daemon._session_token = 7
    daemon._draft_ts = None
    daemon._paste_q = queue.Queue()
    daemon._paste_q.put(("private notes", {}, False, 12.0,
                         np.empty(0, dtype=np.float32), "continuous", 7))
    daemon._paste_q.put(None)
    drops = []
    daemon.recovery = SimpleNamespace(
        drop=drops.append, spool=lambda _: (_ for _ in ()).throw(OSError("full")))
    daemon._paste_at_focus = lambda *args, **kwargs: Result(ok=False)
    daemon.executor = SimpleNamespace(clipboard_only=lambda _: Result(ok=True))
    daemon.cues = SimpleNamespace(play=lambda _: None)
    daemon.overlay = SimpleNamespace(result=lambda *args, **kwargs: None)
    daemon._finish_ui = lambda _: None
    daemon._print_outcome = lambda _: None

    daemon._paste_loop()

    assert drops == []  # original durable commit remains available for recovery


def test_commit_storage_failure_still_queues_fully_cleaned_transcript():
    daemon = Daemon.__new__(Daemon)
    daemon.cfg = Config()
    daemon.cfg.polish.deterministic = False
    daemon._session_lock = threading.Lock()
    daemon._session_token = 3
    daemon._session_parts = ["First paragraph.", "Second paragraph."]
    daemon._snippets = {}
    daemon._paste_q = queue.Queue()
    daemon._polish_text = lambda text: text
    daemon.recovery = SimpleNamespace(
        spool=lambda _: (_ for _ in ()).throw(OSError("full")))
    warnings = []
    daemon.overlay = SimpleNamespace(show=lambda *args: warnings.append(args))

    daemon._finish_dictation_worker(
        3, {}, np.empty(0, dtype=np.float32), "continuous", False)

    text, _, _, stamp, *_ = daemon._paste_q.get_nowait()
    assert text == "First paragraph. Second paragraph."
    assert stamp is None
    assert warnings == [("Recovery unavailable — finishing paste", "warn")]


def test_long_dictation_skips_short_output_polisher_without_losing_text():
    daemon = Daemon.__new__(Daemon)
    daemon.cfg = Config()
    daemon.cfg.llm.endpoint = "http://configured.example"
    daemon.llm = SimpleNamespace(
        complete=lambda *args: (_ for _ in ()).throw(
            AssertionError("600-token polish cannot cover a long session")))
    transcript = "A complete sentence. " * 350

    assert daemon._polish_text(transcript) == transcript
