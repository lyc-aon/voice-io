"""Platform-neutral contract tests for macOS adapters (no native GUI needed)."""

from __future__ import annotations

import wave
from dataclasses import dataclass
from unittest.mock import patch

import pytest

from voice_io.executor import Executor
from voice_io.cues import CuePlayer
from voice_io.mac_focus import MacTextIO
from voice_io.mac_hotkeys import MacHotkeys
from voice_io.overlay_mac import OverlayState


class ClipboardItem:
    def __init__(self, values):
        self.values = dict(values)

    def types(self):
        return list(self.values)

    def dataForType_(self, typ):
        return self.values.get(typ)

    def setData_forType_(self, data, typ):
        self.values[typ] = data

    @classmethod
    def alloc(cls):
        return cls({})

    def init(self):
        return self


class Clipboard:
    def __init__(self):
        self.items = [ClipboardItem({"image/png": b"image", "text/html": b"<b>saved</b>"})]
        self.change = 0

    def pasteboardItems(self):
        return self.items

    def clearContents(self):
        self.items = []
        self.change += 1

    def setString_forType_(self, value, typ):
        self.items = [ClipboardItem({typ: value.encode()})]
        self.change += 1
        return True

    def writeObjects_(self, values):
        self.items.extend(values)
        self.change += 1
        return True

    def changeCount(self):
        return self.change


class FakeAppKit:
    NSPasteboardTypeString = "text/plain"
    NSPasteboardItem = ClipboardItem

    def __init__(self, board):
        self.NSPasteboard = type("Pasteboard", (), {"generalPasteboard": lambda: board})


class FakeQuartz:
    kCGEventFlagMaskCommand = 8
    kCGHIDEventTap = 1

    def AXIsProcessTrusted(self):
        return True

    def CGEventCreateKeyboardEvent(self, _source, code, down):
        return [code, down]

    def CGEventSetFlags(self, event, flags):
        event.append(flags)

    @staticmethod
    def CGEventKeyboardSetUnicodeString(event, length, text):
        assert length == len(text)
        event.append(text)


    def CGEventPost(self, _tap, event):
        self.events.append(event)

    events = None


def test_mac_paste_restores_all_clipboard_types_and_sends_command_v():
    board = Clipboard()
    quartz = FakeQuartz()
    quartz.events = []
    io = MacTextIO(focus=lambda: "target", native=(FakeAppKit(board), quartz), sleep=lambda _: None)
    io.paste("hello", expected_focus="target")
    assert board.items[0].values == {"image/png": b"image", "text/html": b"<b>saved</b>"}
    assert quartz.events == [[9, True, 8], [9, False, 8]]


def test_mac_focus_drift_never_sends_chord_or_erases_recovery_copy():
    board = Clipboard()
    quartz = FakeQuartz()
    quartz.events = []
    io = MacTextIO(focus=lambda: "other", native=(FakeAppKit(board), quartz), sleep=lambda _: None)
    with pytest.raises(RuntimeError, match="Focus changed"):
        io.paste("hello", expected_focus="target")
    assert quartz.events == []
    assert board.items[0].values["text/plain"] == b"hello"

def test_mac_typing_stops_at_focus_drift_and_requires_verified_target():
    board = Clipboard()
    quartz = FakeQuartz()
    quartz.events = []
    focused = iter(["target", "other"])
    io = MacTextIO(focus=lambda: next(focused), native=(FakeAppKit(board), quartz))
    with pytest.raises(RuntimeError, match="Focus changed"):
        io.type_text("ab", expected_focus="target")
    assert quartz.events == [[0, True, "a"], [0, False, "a"]]
    with pytest.raises(RuntimeError, match="verified focus"):
        io.type_text("x", expected_focus="")

def test_mac_paste_does_not_overwrite_concurrent_clipboard_change():
    board = Clipboard()
    quartz = FakeQuartz()
    quartz.events = []

    def copy_during_paste(_):
        board.setString_forType_("user", "text/plain")

    io = MacTextIO(focus=lambda: "target", native=(FakeAppKit(board), quartz),
                   sleep=copy_during_paste)
    io.paste("hello", expected_focus="target")
    assert board.items[0].values == {"text/plain": b"user"}


def test_mac_executor_never_uses_linux_fallback_after_paste_failure():
    class FailedIO:
        def paste(self, text, *, expected_focus):
            raise RuntimeError("Accessibility permission required")

    with patch("voice_io.executor.sys.platform", "darwin"):
        executor = Executor(dry_run=False, mac_io=FailedIO())
        result = executor.dictate("é", expected_focus="window")
    assert not result.ok
    assert "Accessibility" in result.message



def test_overlay_protocol_transience_and_clear():
    now = [10.]
    state = OverlayState(clock=lambda: now[0])
    assert not state.visible()
    state.handle({"op": "ready", "active": True})
    assert state.visible()
    now[0] += 4.01
    assert not state.visible()
    state.handle({"op": "session", "open": True})
    state.handle({"op": "state", "value": "listening"})
    state.handle({"op": "level", "value": 10})
    state.handle({"op": "preview", "text": "speaking"})
    assert state.level == 1 and state.preview == "speaking" and state.visible()
    assert state.preview_until > now[0]
    now[0] += 1.11
    assert state.preview_until < now[0]  # transient, not a pinned subtitle
    state.handle({"op": "preview", "text": "new words"})
    state.handle({"op": "state", "value": "processing"})
    assert state.preview_until == 0
    state.handle({"op": "session", "open": False})
    assert state.preview_until == 0
    assert not state.visible()
    state.handle({"op": "show", "text": "warning", "style": "warn"})
    assert state.visible()
    state.handle({"op": "clear"})
    assert not state.visible()
    state.handle({"op": "quit"})
    assert state.quit

def test_mac_overlay_warning_preempts_preview_and_expires_during_session():
    now = [100.]
    state = OverlayState(clock=lambda: now[0])
    state.handle({"op": "session", "open": True})
    state.handle({"op": "state", "value": "listening"})
    state.handle({"op": "preview", "text": "three short words"})
    assert state.preview_text() == "three short words"
    state.handle({"op": "show", "text": "ASR error — recovered", "style": "warn"})
    assert state.message_text() == "ASR error — recovered"
    assert state.preview_text() == ""
    state.handle({"op": "preview", "text": "hidden while warned"})
    state.handle({"op": "show", "text": "also hidden", "style": "info"})
    assert state.preview_text() == ""
    now[0] += 1.41
    assert state.message_text() == ""
    assert state.preview_text() == ""
    state.handle({"op": "preview", "text": "back to preview"})
    assert state.preview_text() == "back to preview"
    state.handle({"op": "state", "value": "processing"})
    assert state.processing_since == now[0]
    assert state.preview_text() == ""
    state.handle({"op": "session", "open": False})
    state.handle({"op": "result", "text": "focus changed", "style": "error",
                  "auto_close_s": 2.5})
    assert state.message_text() == "focus changed"
    assert state.visible()
    now[0] += 2.51
    assert not state.message_text() and not state.visible()


def test_mac_overlay_success_does_not_expose_transcript_as_message():
    now = [10.]
    state = OverlayState(clock=lambda: now[0])
    state.handle({"op": "result", "text": "private dictated text", "style": "ok",
                  "auto_close_s": 2})
    assert state.visible()
    assert state.message_text() == state.preview_text() == ""
    assert state.message == ""
    now[0] += 2.01
    assert not state.visible()

def test_mac_overlay_meeting_success_shows_only_short_expiring_status():
    now = [10.]
    state = OverlayState(clock=lambda: now[0])
    state.handle({"op": "result", "text": "Meeting notes saved", "style": "ok",
                  "show_text": True, "auto_close_s": 2.5})
    assert state.message_text() == "Meeting notes saved"
    assert state.visible()
    now[0] += 2.51
    assert not state.message_text() and not state.visible()


def test_confirmation_prompt_cannot_pin_idle_overlay_onscreen():
    now = [10.]
    state = OverlayState(clock=lambda: now[0])
    state.handle({"op": "show", "text": "approve?", "style": "prompt"})
    now[0] += 3600
    assert state.visible()
    state.handle({"op": "state", "value": "idle"})
    assert not state.visible()


def test_mac_idle_tab_hover_targets_and_active_transitions():
    now = [10.]
    state = OverlayState(clock=lambda: now[0])
    assert state.dock_mode() == "tab"
    assert state.idle_target(20) == "push"
    assert state.idle_target(60) is None
    assert state.idle_target(-1) is None
    state.enter_idle()
    assert state.dock_mode() == "hover"
    now[0] += 1.5
    state.move_idle()
    now[0] += 1.5
    assert state.dock_mode() == "hover"
    now[0] += .31
    assert state.dock_mode() == "tab"
    assert state.idle_snoozed
    state.enter_idle()  # resizing must not reopen a motionless dot
    assert state.dock_mode() == "tab"
    state.move_idle()
    assert state.dock_mode() == "hover"
    state.handle({"op": "state", "value": "off"})
    assert state.dock_mode() == "hover"
    assert state.idle_target(20) == "toggle"
    state.handle({"op": "state", "value": "idle"})
    state.handle({"op": "session", "open": True})
    state.handle({"op": "state", "value": "listening"})
    assert state.dock_mode() == "active"
    state.handle({"op": "state", "value": "processing"})
    assert state.dock_mode() == "active"
    state.handle({"op": "session", "open": False})
    state.handle({"op": "result", "text": "private words", "style": "ok",
                  "auto_close_s": 1.4})
    assert state.dock_mode() == "active"
    now[0] += 1.41
    assert state.dock_mode() == "tab"
    state.move_idle()
    assert state.dock_mode() == "hover"
    now[0] += 1.81
    assert state.dock_mode() == "tab"
    state.handle({"op": "result", "text": "microphone error", "style": "error",
                  "auto_close_s": 2})
    assert state.dock_mode() == "active"
    now[0] += 2.01
    assert state.dock_mode() == "tab"


@dataclass
class Hotkeys:
    push: str = "ctrl+alt+space"
    toggle: str = "ctrl+alt+t"
    cancel: str = "ctrl+alt+x"


def test_mac_hotkey_dispatch_filters_autorepeat_and_extra_modifiers():
    class Quartz:
        kCGKeyboardEventAutorepeat = 8
        kCGKeyboardEventKeycode = 9
        kCGEventFlagMaskControl = 1
        kCGEventFlagMaskAlternate = 2
        kCGEventFlagMaskShift = 4
        kCGEventFlagMaskCommand = 16

        @staticmethod
        def CGEventGetIntegerValueField(event, field):
            return event[field]

        @staticmethod
        def CGEventGetFlags(event):
            return event["flags"]

    seen = []
    listener = MacHotkeys(Hotkeys(), seen.append)
    listener._dispatch({9: 49, 8: False, "flags": 3}, None, Quartz)
    listener._dispatch({9: 49, 8: True, "flags": 3}, None, Quartz)
    listener._dispatch({9: 49, 8: False, "flags": 7}, None, Quartz)
    assert seen == ["push"]


def test_mac_cue_plays_with_afplay(tmp_path):
    (tmp_path / "listening.wav").write_bytes(b"cue")
    with patch("voice_io.cues.sys.platform", "darwin"), \
         patch.object(CuePlayer, "_ensure"), \
         patch("voice_io.cues.subprocess.Popen") as popen:
        CuePlayer(base_dir=tmp_path).play("listening")
    assert popen.call_args.args[0] == ["afplay", str(tmp_path / "listening.wav")]

def test_local_cues_are_valid_audible_wav_without_ffmpeg(tmp_path):
    CuePlayer(base_dir=tmp_path)
    for name in ("listening", "error"):
        with wave.open(str(tmp_path / f"{name}.wav")) as audio:
            assert audio.getnchannels() == 1
            assert audio.getframerate() == 16000
            assert any(audio.readframes(audio.getnframes()))
