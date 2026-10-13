"""Verify cursor-directed paste and safe focus changes on Linux."""

from unittest.mock import patch

from voice_io.executor import Executor


def test_focus_changes_during_clipboard_handoff_keep_text_for_manual_paste():
    executor = Executor(dry_run=False)
    clipboard = []
    executor._snapshot_clipboard = lambda: ("text", b"old clipboard")
    executor._spawn_copy = lambda data, mime: clipboard.append(data)
    executor._verify_serve = lambda text: True
    executor._send_key = lambda chord: (_ for _ in ()).throw(
        AssertionError("wrong editor received paste chord"))
    executor._restore_clipboard = lambda snap: (_ for _ in ()).throw(
        AssertionError("recovery transcript was cleared"))

    with patch("voice_io.executor.time.sleep", lambda _: None):
        result = executor.dictate(
            "Final cleaned transcript", expected_focus="original editor",
            focus_probe=lambda: "another editor", submit=True)

    assert not result.ok
    assert not result.executed
    assert "copied to clipboard" in result.message
    assert clipboard == [b"Final cleaned transcript"]


def test_clipboard_failure_does_not_type_into_an_editor_and_can_retry():
    executor = Executor(dry_run=False)
    executor._snapshot_clipboard = lambda: ("text", b"old clipboard")
    executor._spawn_copy = lambda *_: None
    keys = []
    executor._send_key = lambda chord: keys.append(chord) or True
    executor._restore_clipboard = lambda *_: None
    attempts = iter((False, True, True))
    executor._verify_serve = lambda *_: next(attempts)

    with patch("voice_io.executor.time.sleep", lambda _: None):
        first = executor.dictate("First paragraph", expected_focus="editor",
                                 focus_probe=lambda: "editor")
        second = executor.dictate("Second paragraph", expected_focus="editor",
                                  focus_probe=lambda: "editor")

    assert not first.ok and not first.executed
    assert second.ok and second.executed
    assert keys == ["ctrl+v"]


def test_clipboard_restore_does_not_overwrite_a_new_user_copy():
    executor = Executor(dry_run=False)
    executor._snapshot_clipboard = lambda: ("text", b"old clipboard")
    executor._spawn_copy = lambda *_: None
    served = iter((True, False))  # user copied something after the paste chord
    executor._verify_serve = lambda *_: next(served)
    executor._send_key = lambda *_: True
    executor._restore_clipboard = lambda *_: (_ for _ in ()).throw(
        AssertionError("concurrent clipboard content was overwritten"))
    with patch("voice_io.executor.time.sleep", lambda _: None):
        result = executor.dictate("Transcript", expected_focus="editor",
                                  focus_probe=lambda: "editor")
    assert result.ok


def test_focus_change_after_insertion_suppresses_submit_to_other_window():
    executor = Executor(dry_run=False, auto_submit="enter")
    executor._snapshot_clipboard = lambda: None
    executor._spawn_copy = lambda *_: None
    executor._verify_serve = lambda *_: True
    executor._restore_clipboard = lambda *_: None
    keys = []
    executor._send_key = lambda chord: keys.append(chord) or True
    focus = iter(("tern|window:1", "notes|window:2"))

    with patch("voice_io.executor.time.sleep", lambda _: None):
        result = executor.dictate(
            "Final text", "ctrl+shift+v", submit=True,
            expected_focus="tern|window:1", focus_probe=lambda: next(focus))

    assert result.ok and result.executed
    assert keys == ["ctrl+shift+v"]
    assert "submit suppressed" in result.message


def test_wayland_never_sends_keys_into_stale_xwayland_window():
    calls = []

    def fail(argv, **_kwargs):
        calls.append(argv[0])
        raise OSError("sender unavailable")

    with (patch("voice_io.executor.os.environ",
                {"XDG_SESSION_TYPE": "wayland"}),
          patch("voice_io.executor.subprocess.run", side_effect=fail)):
        assert not Executor._send_key("shift+Insert")

    assert calls == ["ydotool", "wtype"]
