"""Global macOS shortcuts via a Quartz HID event tap (Input Monitoring grant).

MacHotkeys(cfg.hotkeys, voice_io.control.write).start() installs the listener;
stop() removes it. Events run on a dedicated CFRunLoop thread; callbacks emit
one of push/toggle/cancel on key-down, never on autorepeat or key-up. The
listener never consumes other applications' keystrokes.
"""

from __future__ import annotations

import logging
import sys
import threading
from typing import Callable

from voice_io.config import parse_hotkey

log = logging.getLogger(__name__)


def _native():
    try:
        import AppKit
        import Quartz
    except ImportError as exc:
        raise RuntimeError("Mac hotkeys require pyobjc-framework-Cocoa and pyobjc-framework-Quartz") from exc
    return AppKit, Quartz


def _key_name(event, appkit, quartz) -> str:
    keycode = int(quartz.CGEventGetIntegerValueField(event, quartz.kCGKeyboardEventKeycode))
    special = {49: "space", 36: "enter", 48: "tab", 53: "escape",
               51: "backspace", 117: "delete", 123: "left", 124: "right",
               125: "down", 126: "up", 115: "home", 119: "end",
               116: "pageup", 121: "pagedown", 114: "insert"}
    if keycode in special:
        return special[keycode]
    # The HID keycode alone assumes a US layout. NSEvent translates according
    # to the active keyboard layout, ignoring held Command/Option/Control.
    translated = appkit.NSEvent.eventWithCGEvent_(event)
    chars = translated.charactersIgnoringModifiers() if translated else None
    if chars and len(chars) == 1 and 0xF704 <= ord(chars) <= 0xF71B:
        return f"f{ord(chars) - 0xF703}"
    if chars and len(chars) == 1 and chars.isprintable():
        return chars.lower()
    return ""


def _modifiers(flags, quartz) -> frozenset[str]:
    masks = ((quartz.kCGEventFlagMaskControl, "ctrl"),
             (quartz.kCGEventFlagMaskAlternate, "alt"),
             (quartz.kCGEventFlagMaskShift, "shift"),
             (quartz.kCGEventFlagMaskCommand, "super"))
    return frozenset(name for mask, name in masks if flags & mask)


class MacHotkeys:
    def __init__(self, hotkeys, callback: Callable[[str], object]):
        self._bindings = {}
        for command in ("push", "toggle", "cancel"):
            binding = parse_hotkey(getattr(hotkeys, command))
            if binding in self._bindings:
                raise ValueError(f"Duplicate macOS hotkey: {command} and {self._bindings[binding]}")
            self._bindings[binding] = command
        self._callback = callback
        self._thread: threading.Thread | None = None
        self._loop = None
        self._tap = None
        self._ready = threading.Event()
        self._error: Exception | None = None

    def _dispatch(self, event, appkit, quartz) -> None:
        if quartz.CGEventGetIntegerValueField(event, quartz.kCGKeyboardEventAutorepeat):
            return
        flags = quartz.CGEventGetFlags(event)
        command = self._bindings.get((_modifiers(flags, quartz),
                                       _key_name(event, appkit, quartz)))
        if command:
            try:
                if self._callback(command) is False:
                    log.warning("Mac hotkey %s could not reach the control FIFO", command)
            except Exception:
                log.exception("Mac hotkey callback failed: %s", command)

    def start(self) -> None:
        if self._thread is not None:
            return
        if sys.platform != "darwin":
            raise RuntimeError("MacHotkeys requires macOS")
        self._ready.clear()
        self._error = None
        self._thread = threading.Thread(target=self._run, daemon=True,
                                        name="voice-io-mac-hotkeys")
        self._thread.start()
        self._ready.wait()
        if self._error is not None:
            self._thread.join()
            self._thread = None
            raise self._error

    def _run(self) -> None:
        try:
            appkit, quartz = _native()
            if hasattr(quartz, "CGPreflightListenEventAccess") and not quartz.CGPreflightListenEventAccess():
                raise RuntimeError(
                    "Input Monitoring permission required: grant the voice-io Python executable "
                    "in System Settings > Privacy & Security > Input Monitoring, then restart voice-io")

            def on_event(_proxy, kind, event, _info):
                if kind == quartz.kCGEventTapDisabledByTimeout:
                    quartz.CGEventTapEnable(self._tap, True)
                elif kind == quartz.kCGEventKeyDown:
                    self._dispatch(event, appkit, quartz)
                return event  # listen-only: never consume or modify input

            mask = 1 << quartz.kCGEventKeyDown
            self._tap = quartz.CGEventTapCreate(
                quartz.kCGHIDEventTap, quartz.kCGHeadInsertEventTap,
                quartz.kCGEventTapOptionListenOnly, mask, on_event, None)
            if self._tap is None:
                raise RuntimeError(
                    "Cannot create global keyboard event tap; enable Input Monitoring "
                    "for the voice-io Python executable and restart voice-io")
            source = quartz.CFMachPortCreateRunLoopSource(None, self._tap, 0)
            self._loop = quartz.CFRunLoopGetCurrent()
            quartz.CFRunLoopAddSource(self._loop, source, quartz.kCFRunLoopCommonModes)
            quartz.CGEventTapEnable(self._tap, True)
            self._ready.set()
            quartz.CFRunLoopRun()
            quartz.CGEventTapEnable(self._tap, False)
            quartz.CFRunLoopRemoveSource(self._loop, source, quartz.kCFRunLoopCommonModes)
        except Exception as exc:
            self._error = exc
            self._ready.set()
        finally:
            self._loop = None
            self._tap = None
            self._ready.set()

    def stop(self) -> None:
        thread = self._thread
        if thread is None:
            return
        loop = self._loop
        if loop is not None:
            _, quartz = _native()
            quartz.CFRunLoopStop(loop)
            quartz.CFRunLoopWakeUp(loop)
        thread.join(timeout=3)
        if thread.is_alive():
            raise RuntimeError("Mac hotkey event tap did not stop")
        self._thread = None
