"""macOS Accessibility focus identity and safe native clipboard/keyboard I/O.

Requires pyobjc-framework-Cocoa and pyobjc-framework-Quartz. Accessibility
permission must be granted to the Python executable running voice-io in System
Settings > Privacy & Security > Accessibility. No synthetic event is sent when
focus cannot be established or changed since capture.
"""

from __future__ import annotations

import time
from typing import Callable


class MacPermissionError(RuntimeError):
    """A macOS privacy grant needed for input/focus is missing."""


def _native():
    try:
        import AppKit
        import Quartz
    except ImportError as exc:
        raise RuntimeError("macOS support requires pyobjc-framework-Cocoa and pyobjc-framework-Quartz") from exc
    return AppKit, Quartz


def _accessibility(quartz) -> None:
    if not quartz.AXIsProcessTrusted():
        raise MacPermissionError(
            "Accessibility permission required: grant the voice-io Python executable "
            "in System Settings > Privacy & Security > Accessibility, then restart voice-io")


def _attribute(quartz, element, name):
    code, value = quartz.AXUIElementCopyAttributeValue(element, name, None)
    if code != quartz.kAXErrorSuccess:
        raise RuntimeError(f"Cannot read focused macOS window ({name}, AX error {code})")
    return value


def _focused_window_impl() -> str:
    """Return a stable frontmost-app/window identity or raise (never silently guess).

    Window number, when exposed by Accessibility, distinguishes windows with
    identical titles. Geometry and title also participate for apps that do not
    expose a window number. If focus moves, a subsequent probe differs.
    """
    appkit, quartz = _native()
    _accessibility(quartz)
    app = appkit.NSWorkspace.sharedWorkspace().frontmostApplication()
    if app is None:
        raise RuntimeError("No frontmost macOS application; refusing to paste")
    pid = int(app.processIdentifier())
    element = quartz.AXUIElementCreateApplication(pid)
    window = _attribute(quartz, element, quartz.kAXFocusedWindowAttribute)
    if window is None:
        raise RuntimeError("Frontmost application has no focused window; refusing to paste")

    def optional(name):
        code, value = quartz.AXUIElementCopyAttributeValue(window, name, None)
        return value if code == quartz.kAXErrorSuccess else None

    number = optional("AXWindowNumber")
    title = optional(quartz.kAXTitleAttribute)
    position = optional(quartz.kAXPositionAttribute)
    size = optional(quartz.kAXSizeAttribute)
    # AXValue objects lack useful repr; convert the pair of CGPoints through
    # AXValueGetValue when available. A title alone is not a unique window.
    def point(value, kind):
        if value is None:
            return None
        try:
            return tuple(quartz.AXValueGetValue(value, kind, None)[1])
        except (TypeError, ValueError, AttributeError):
            return str(value)

    geometry = (point(position, quartz.kAXValueCGPointType),
                point(size, quartz.kAXValueCGSizeType))
    if number is None and title is None and geometry == (None, None):
        raise RuntimeError("Cannot identify focused macOS window; refusing to paste")
    return repr((app.bundleIdentifier() or pid, pid, number, title, geometry))


def focused_window() -> str:
    """Return the frontmost window identity, or an actionable RuntimeError."""
    try:
        return _focused_window_impl()
    except RuntimeError:
        raise
    except Exception as exc:
        raise RuntimeError(
            "macOS focus probe failed; grant Accessibility to the voice-io "
            "Python executable in System Settings > Privacy & Security and restart") from exc


class MacTextIO:
    """Paste via NSPasteboard + Quartz, restoring only our own clipboard write.

    `focus` and `native` can be injected in unit tests. The optional
    `expected_focus` must be the identity captured when the session began.
    """

    def __init__(self, *, focus: Callable[[], str] = focused_window,
                 native=None, sleep: Callable[[float], None] = time.sleep):
        self.focus = focus
        self._native = native
        self.sleep = sleep

    def _modules(self):
        return self._native if self._native is not None else _native()

    def _pasteboard(self):
        appkit, _ = self._modules()
        return appkit.NSPasteboard.generalPasteboard()

    def copy(self, text: str) -> None:
        appkit, _ = self._modules()
        board = self._pasteboard()
        board.clearContents()
        if not board.setString_forType_(text, appkit.NSPasteboardTypeString):
            raise RuntimeError("macOS clipboard rejected transcript")

    def key(self, chord: str) -> None:
        _, quartz = self._modules()
        _accessibility(quartz)
        if chord in ("cmd+v", "super+v"):
            keycode = 9  # virtual keycode kVK_ANSI_V
            flags = quartz.kCGEventFlagMaskCommand  # macOS paste always Command-V
        elif chord == "enter":
            keycode, flags = 36, 0
        elif chord in ("ctrl+enter", "cmd+enter"):
            keycode = 36
            flags = (quartz.kCGEventFlagMaskControl if chord == "ctrl+enter"
                     else quartz.kCGEventFlagMaskCommand)
        else:
            raise ValueError(f"Unsupported macOS key chord: {chord}")
        for down in (True, False):
            event = quartz.CGEventCreateKeyboardEvent(None, keycode, down)
            quartz.CGEventSetFlags(event, flags)
            quartz.CGEventPost(quartz.kCGHIDEventTap, event)

    def type_text(self, text: str, *, expected_focus: str) -> None:
        """Type Unicode directly through Quartz without touching the clipboard.

        Explicit opt-in alternative when an application refuses paste; never a
        silent fallback, since partial typing cannot be rolled back.
        """
        _, quartz = self._modules()
        _accessibility(quartz)
        if not expected_focus:
            raise RuntimeError("Cannot type without a verified focus identity")
        for character in text:
            if self.focus() != expected_focus:
                raise RuntimeError("Focus changed during typing; stopped before next character")
            for down in (True, False):
                event = quartz.CGEventCreateKeyboardEvent(None, 0, down)
                quartz.CGEventKeyboardSetUnicodeString(
                    event, len(character.encode("utf-16-le")) // 2, character)
                quartz.CGEventPost(quartz.kCGHIDEventTap, event)

    def paste(self, text: str, *, expected_focus: str | None = None) -> None:
        if not expected_focus:
            raise RuntimeError("No verified macOS focus identity; transcript not pasted")
        appkit, _ = self._modules()
        _accessibility(self._modules()[1])
        board = self._pasteboard()
        # Preserve arbitrary clipboard representations, not only text. The
        # board may have multiple items/types (e.g. image and HTML).
        saved = []
        for item in board.pasteboardItems() or []:
            types = {}
            for typ in item.types():
                data = item.dataForType_(typ)
                if data is None:
                    raise RuntimeError(f"Cannot snapshot macOS clipboard type {typ}; paste cancelled")
                types[typ] = data
            saved.append(types)
        board.clearContents()
        if not board.setString_forType_(text, appkit.NSPasteboardTypeString):
            raise RuntimeError("macOS clipboard rejected transcript")
        ours = board.changeCount()
        try:
            # Probe immediately before the chord to avoid pasting into a new
            # target while transcript/clipboard preparation was in flight.
            if expected_focus is not None and self.focus() != expected_focus:
                raise RuntimeError("Focus changed before paste; transcript left on clipboard")
            self.sleep(0.06)
            if expected_focus is not None and self.focus() != expected_focus:
                raise RuntimeError("Focus changed before paste; transcript left on clipboard")
            self.key("cmd+v")
            self.sleep(0.12)
        except Exception:
            # Keep the transcript for manual recovery if the paste failed.
            raise
        else:
            # Never overwrite content another process/user copied meanwhile.
            if board.changeCount() == ours:
                board.clearContents()
                items = []
                for types in saved:
                    item = appkit.NSPasteboardItem.alloc().init()
                    for typ, data in types.items():
                        item.setData_forType_(data, typ)
                    items.append(item)
                if items:
                    board.writeObjects_(items)
