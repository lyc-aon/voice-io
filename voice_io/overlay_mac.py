#!/usr/bin/env python3
"""AppKit overlay subprocess; newline-delimited daemon JSON on stdin.

The non-activating floating panel keeps a tiny right-edge tab visible at
rest. Hover exposes one status dot: left-click to transcribe, right-click
for Settings. Recording retains the waveform; click it to finish or ×
to cancel the session.
Run `python -m voice_io.overlay_mac --settings` for editable settings.
Requires pyobjc-framework-Cocoa.
"""

from __future__ import annotations

import json
import os
import queue
import subprocess
import sys
import threading
import time
from dataclasses import dataclass, field

from voice_io.control import fifo_path
from voice_io.levels import speech_level


def write_control(command: str) -> bool:
    """Nonblocking FIFO write: a closed daemon must not freeze AppKit."""
    try:
        fd = os.open(fifo_path(), os.O_WRONLY | os.O_NONBLOCK)
        try:
            os.write(fd, (command + "\n").encode())
        finally:
            os.close(fd)
        return True
    except OSError:
        return False


@dataclass
class OverlayState:
    state: str = "idle"
    processing_since: float = 0.0
    session: bool = False
    level: float = 0.0
    history: list[float] = field(default_factory=lambda: [0.0] * 13)
    preview: str = ""
    preview_until: float = 0.0
    message: str = ""
    style: str = "info"
    message_until: float = 0.0
    ready_until: float = 0.0
    quit: bool = False
    clock: object = field(default=time.monotonic, repr=False)
    hovered: bool = False
    idle_exposed_until: float = 0.0
    idle_snoozed: bool = False

    def handle(self, msg: dict) -> None:
        now = self.clock()
        op = msg.get("op")
        if op == "session":
            self.session = bool(msg.get("open"))
            if self.session:
                self.hovered = False
                self.idle_snoozed = False
                self.history[:] = [0.0] * len(self.history)
                self.level = 0.0
                self.preview = ""
                self.preview_until = 0.0
            if not self.session and self.state in ("listening", "processing"):
                self.state = "idle"
            if not self.session:
                self.preview = ""
                self.preview_until = 0.0
        elif op == "state":
            value = str(msg.get("value", "idle"))
            if value == "processing" and self.state != "processing":
                self.processing_since = now
            self.state = value
            if self.state != "listening":
                self.preview_until = 0.0
            if self.state == "listening" and self.style == "ok":
                self.message_until = 0.0
            if self.state == "idle" and not self.session and self.style == "prompt":
                self.message = ""
                self.message_until = 0.0
        elif op == "ready":
            self.ready_until = now + 4.0 if msg.get("active", True) else 0.0
        elif op == "level":
            self.level = speech_level(float(msg.get("value", 0)))
            self.history.pop(0)
            self.history.append(self.level)
        elif op == "preview":
            if self.session and self.state == "listening" and not self.message_text():
                self.preview = str(msg.get("text", ""))
                self.preview_until = now + 1.1 if self.preview else 0.0
        elif op == "show":
            text = str(msg.get("text", ""))
            style = str(msg.get("style", "info"))
            if style == "info":
                if not self.message_text():
                    self.preview = text if self.session and self.state == "listening" else ""
                    self.preview_until = now + 1.1 if self.preview else 0.0
            else:
                self.preview_until = 0.0
                self.message = "" if style == "ok" else text
                self.style = style
                self.message_until = float("inf") if style == "prompt" else now + 1.4
        elif op == "result":
            self.preview_until = 0.0
            self.style = str(msg.get("style", "ok"))
            self.message = (str(msg.get("text", "")) if self.style != "ok"
                            or msg.get("show_text", False) else "")
            self.message_until = now + max(0.0, float(msg.get("auto_close_s", 2)))
        elif op == "clear":
            self.preview_until = 0.0
            self.message = ""
            self.message_until = 0.0
        elif op == "quit":
            self.quit = True

    def message_text(self) -> str:
        return (self.message if self.style in ("warn", "error", "prompt", "ok")
                and self.clock() < self.message_until else "")

    def preview_text(self) -> str:
        return (self.preview if self.session and self.state == "listening"
                and self.clock() < self.preview_until and not self.message_text()
                else "")

    def visible(self) -> bool:
        now = self.clock()
        return (self.session or self.state in ("listening", "processing")
                or now < self.ready_until or now < self.message_until)

    def enter_idle(self) -> None:
        if not self.visible() and not self.idle_snoozed:
            self.hovered = True
            self.idle_exposed_until = self.clock() + 1.8

    def move_idle(self) -> None:
        if not self.visible():
            self.idle_snoozed = False
            self.hovered = True
            self.idle_exposed_until = self.clock() + 1.8

    def dock_mode(self) -> str:
        if self.hovered and self.clock() >= self.idle_exposed_until:
            self.hovered = False
            self.idle_snoozed = True
        return "active" if self.visible() else "hover" if self.hovered else "tab"

    def idle_target(self, y: float) -> str | None:
        """The entire 28px hover dot is one record/unmute target."""
        if 0 <= y < 28:
            return "toggle" if self.state == "off" else "push"
        return None


def _native():
    try:
        import AppKit
        import Foundation
    except ImportError as exc:
        raise RuntimeError("macOS overlay requires pyobjc-framework-Cocoa") from exc
    return AppKit, Foundation


def _preview_rgb(color: str, default: tuple[float, float, float]
                 ) -> tuple[float, float, float]:
    if not color:
        return default
    return tuple(int(color[i:i + 2], 16) / 255.0 for i in (1, 3, 5))


def _run_settings() -> int:
    AppKit, Foundation = _native()
    from voice_io.config import Config

    app = AppKit.NSApplication.sharedApplication()
    app.setActivationPolicy_(AppKit.NSApplicationActivationPolicyRegular)
    cfg = Config.load()
    visible_height = AppKit.NSScreen.mainScreen().visibleFrame().size.height
    window_height = min(710, max(220, visible_height - 32))
    rect = Foundation.NSMakeRect(0, 0, 450, window_height)
    win = AppKit.NSWindow.alloc().initWithContentRect_styleMask_backing_defer_(
        rect, AppKit.NSWindowStyleMaskTitled | AppKit.NSWindowStyleMaskClosable,
        AppKit.NSBackingStoreBuffered, False)
    win.setTitle_("voice-io settings")
    win.center()
    scroll = AppKit.NSScrollView.alloc().initWithFrame_(
        Foundation.NSMakeRect(0, 56, 450, window_height - 56))
    scroll.setHasVerticalScroller_(True)
    document = AppKit.NSView.alloc().initWithFrame_(
        Foundation.NSMakeRect(0, 0, 450, 665))
    scroll.setDocumentView_(document)
    win.contentView().addSubview_(scroll)
    fields = {}
    for index, (section, key, label) in enumerate((
            ("hotkeys", "push", "Push / finish"),
            ("hotkeys", "toggle", "Toggle"),
            ("hotkeys", "cancel", "Cancel"),
            ("kws", "wake_phrase", "Wake phrase"),
            ("kws", "end_phrase", "End phrase"))):
        y = 617 - index * 44
        name = AppKit.NSTextField.labelWithString_(label)
        name.setFrame_(Foundation.NSMakeRect(20, y, 126, 26))
        document.addSubview_(name)
        field = AppKit.NSTextField.alloc().initWithFrame_(
            Foundation.NSMakeRect(151, y, 275, 27))
        field.setStringValue_(str(getattr(getattr(cfg, section), key)))
        document.addSubview_(field)
        fields[section, key] = field
    live_preview = AppKit.NSButton.alloc().initWithFrame_(
        Foundation.NSMakeRect(20, 400, 250, 26))
    live_preview.setButtonType_(AppKit.NSButtonTypeSwitch)
    live_preview.setTitle_("Live three-word preview")
    live_preview.setState_(1 if cfg.ui.live_preview else 0)
    document.addSubview_(live_preview)
    color_label = AppKit.NSTextField.labelWithString_("Color")
    color_label.setFrame_(Foundation.NSMakeRect(272, 400, 43, 26))
    document.addSubview_(color_label)
    preview_color = AppKit.NSColorWell.alloc().initWithFrame_(
        Foundation.NSMakeRect(321, 400, 105, 26))
    rgb = _preview_rgb(cfg.ui.preview_color, (0.92, 0.92, 0.92))
    preview_color.setColor_(
        AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(*rgb, 1.0))
    document.addSubview_(preview_color)
    heading = AppKit.NSTextField.labelWithString_("MODE & MEETING")
    heading.setFrame_(Foundation.NSMakeRect(20, 366, 400, 25))
    document.addSubview_(heading)
    mode_label = AppKit.NSTextField.labelWithString_("Recording mode")
    mode_label.setFrame_(Foundation.NSMakeRect(20, 331, 126, 26))
    document.addSubview_(mode_label)
    mode = AppKit.NSPopUpButton.alloc().initWithFrame_pullsDown_(
        Foundation.NSMakeRect(151, 331, 275, 27), False)
    modes = ("dictation", "meeting", "collaborative")
    mode.addItemsWithTitles_(["Dictation", "Meeting", "Collaborative"])
    mode.selectItemAtIndex_(modes.index(cfg.meeting.mode))
    document.addSubview_(mode)
    explanation = AppKit.NSTextField.labelWithString_(
        "Meeting saves transcript + optional summary, not pasted text. "
        "Collaborative pastes an editable speaker-labelled draft on Stop; "
        "it never submits.")
    explanation.setFrame_(Foundation.NSMakeRect(20, 280, 410, 48))
    explanation.setLineBreakMode_(AppKit.NSLineBreakByWordWrapping)
    document.addSubview_(explanation)
    install_note = AppKit.NSTextField.labelWithString_(
        "Meeting and Collaborative require voice-io models install --meeting")
    install_note.setFrame_(Foundation.NSMakeRect(20, 253, 410, 27))
    install_note.setLineBreakMode_(AppKit.NSLineBreakByWordWrapping)
    document.addSubview_(install_note)
    meeting_fields = {}
    for index, (key, label) in enumerate((
            ("summary_endpoint", "Summary endpoint"),
            ("summary_model", "Summary model"),
            ("summary_api_key_env", "API key env name"))):
        y = 231 - index * 38
        name = AppKit.NSTextField.labelWithString_(label)
        name.setFrame_(Foundation.NSMakeRect(20, y, 126, 26))
        document.addSubview_(name)
        field = AppKit.NSTextField.alloc().initWithFrame_(
            Foundation.NSMakeRect(151, y, 275, 27))
        field.setStringValue_(str(getattr(cfg.meeting, key)))
        document.addSubview_(field)
        meeting_fields[key] = field
    summary_hint = AppKit.NSTextField.labelWithString_(
        "Leave the endpoint or model empty to disable summaries. "
        "Enter an environment variable name, never the API key.")
    summary_hint.setFrame_(Foundation.NSMakeRect(20, 97, 410, 48))
    summary_hint.setLineBreakMode_(AppKit.NSLineBreakByWordWrapping)
    document.addSubview_(summary_hint)
    scroll.contentView().scrollToPoint_(
        Foundation.NSMakePoint(0, max(0, 665 - (window_height - 56))))
    scroll.reflectScrolledClipView_(scroll.contentView())

    class SettingsController(AppKit.NSObject):
        def save_(self, _sender):
            updates = {section: {key: fields[section, key].stringValue().strip()
                                 for s, key in fields if s == section}
                       for section in ("hotkeys", "kws")}
            selected = preview_color.color().colorUsingColorSpaceName_(
                AppKit.NSCalibratedRGBColorSpace)
            rgb = (selected.redComponent(), selected.greenComponent(),
                   selected.blueComponent())
            preview_hex = "#%02x%02x%02x" % tuple(
                round(min(1.0, max(0.0, c)) * 255) for c in rgb)
            updates["ui"] = {"live_preview": bool(live_preview.state()),
                             "preview_color": preview_hex}
            updates["meeting"] = {
                "mode": modes[mode.indexOfSelectedItem()],
                **{key: field.stringValue().strip()
                   for key, field in meeting_fields.items()},
            }
            try:
                from voice_io.kws import keyword_lines
                keyword_lines(cfg.resolve(cfg.kws.model_dir),
                              updates["kws"]["wake_phrase"], updates["kws"]["end_phrase"])
                Config.save_user(updates)
            except (ValueError, OSError) as exc:
                alert = AppKit.NSAlert.alloc().init()
                alert.setMessageText_("Could not save voice-io settings")
                alert.setInformativeText_(str(exc))
                alert.runModal()
                return
            alert = AppKit.NSAlert.alloc().init()
            alert.setMessageText_("Settings saved")
            alert.setInformativeText_("Restart voice-io to apply these changes.")
            alert.runModal()
            win.close()
            app.stop_(None)

        def windowWillClose_(self, _notification):
            app.stop_(None)

    controller = SettingsController.alloc().init()
    button = AppKit.NSButton.alloc().initWithFrame_(Foundation.NSMakeRect(327, 20, 100, 32))
    button.setTitle_("Save")
    button.setBezelStyle_(AppKit.NSBezelStyleRounded)
    button.setTarget_(controller)
    button.setAction_("save:")
    win.contentView().addSubview_(button)
    win.setDelegate_(controller)
    win.makeKeyAndOrderFront_(None)
    app.activateIgnoringOtherApps_(True)  # settings is explicitly requested
    app.run()
    return 0


def _run_overlay() -> int:
    AppKit, Foundation = _native()
    from voice_io.config import Config
    app = AppKit.NSApplication.sharedApplication()
    app.setActivationPolicy_(AppKit.NSApplicationActivationPolicyAccessory)
    state = OverlayState()
    messages: queue.Queue[dict] = queue.Queue()

    def open_settings():
        subprocess.Popen([sys.executable, "-m", "voice_io.overlay_mac", "--settings"])


    class OverlayView(AppKit.NSView):
        def isOpaque(self):
            return False

        def acceptsFirstResponder(self):
            return False

        def drawRect_(self, _rect):
            now = state.clock()
            mode = state.dock_mode()
            active = state.session or state.state in ("listening", "processing")
            if mode == "tab":
                bg = AppKit.NSBezierPath.bezierPathWithRoundedRect_xRadius_yRadius_(
                    Foundation.NSMakeRect(0, 0, 8, 32), 4, 4)
                AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                    .08, .10, .16, .94).set()
                bg.fill()
                rail = AppKit.NSBezierPath.bezierPathWithRoundedRect_xRadius_yRadius_(
                    Foundation.NSMakeRect(1, 4, 2, 24), 1, 1)
                tint = ((.47, .52, .62, .55) if state.state == "off" else
                        (.33, .58, 1., .75))
                AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(*tint).set()
                rail.fill()
            elif mode == "hover":
                bg = AppKit.NSBezierPath.bezierPathWithRoundedRect_xRadius_yRadius_(
                    Foundation.NSMakeRect(0, 0, 28, 28), 14, 14)
                AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                    .08, .10, .16, .94).set()
                bg.fill()
                dot = AppKit.NSBezierPath.bezierPathWithOvalInRect_(
                    Foundation.NSMakeRect(8, 8, 12, 12))
                tint = ((.47, .52, .62, .85) if state.state == "off" else
                        (.33, .58, 1., 1.))
                AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(*tint).set()
                dot.fill()
            elif active:
                bg = AppKit.NSBezierPath.bezierPathWithRoundedRect_xRadius_yRadius_(
                    Foundation.NSMakeRect(0, 0, 38, 140), 19, 19)
                AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(.08, .10, .16, .90).set()
                bg.fill()
                if state.style == "ok" and now < state.message_until:
                    dot = AppKit.NSBezierPath.bezierPathWithOvalInRect_(
                        Foundation.NSMakeRect(13, 62, 12, 12))
                    AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                        .24, .82, .55, .95).set()
                    dot.fill()
                elif state.state == "processing":
                    # Three travelling dots are status, not simulated mic input.
                    center = (now - state.processing_since) * 12 % 76
                    for index in range(3):
                        y = 20 + (center - index * 10) % 76
                        dot = AppKit.NSBezierPath.bezierPathWithOvalInRect_(
                            Foundation.NSMakeRect(16, y, 6, 6))
                        AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                            .98, .73, .35, 1.0 - index * .25).set()
                        dot.fill()
                elif state.state == "listening":
                    for index, strength in enumerate(state.history):
                        bar = AppKit.NSBezierPath.bezierPathWithRoundedRect_xRadius_yRadius_(
                            Foundation.NSMakeRect(9, 12 + index * 8,
                                                  2 + 14 * strength, 3), 1.5, 1.5)
                        AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                            .33, .58, 1., .42 + .55 * strength).set()
                        bar.fill()
                close = AppKit.NSBezierPath.bezierPath()
                close.moveToPoint_(Foundation.NSMakePoint(16, 120))
                close.lineToPoint_(Foundation.NSMakePoint(22, 126))
                close.moveToPoint_(Foundation.NSMakePoint(22, 120))
                close.lineToPoint_(Foundation.NSMakePoint(16, 126))
                close.setLineWidth_(1.5)
                AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                    .78, .82, .91, .8).set()
                close.stroke()
            elif now < state.ready_until or now < state.message_until:
                dot = AppKit.NSBezierPath.bezierPathWithOvalInRect_(
                    Foundation.NSMakeRect(6, 57, 26, 26))
                if now < state.message_until and state.style == "error":
                    color = (.99, .44, .42)
                elif now < state.message_until and state.style == "warn":
                    color = (.98, .73, .35)
                else:
                    color = (.24, .82, .55)
                AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                    *color, .92).set()
                dot.fill()

        def mouseEntered_(self, _event):
            state.enter_idle()

        def mouseExited_(self, _event):
            state.hovered = False

        def mouseMoved_(self, _event):
            state.move_idle()

        def mouseDown_(self, event):
            point = self.convertPoint_fromView_(event.locationInWindow(), None)
            if state.visible():
                write_control("cancel" if point.y >= 114 else
                              "toggle" if state.state == "off" else "push")
            elif state.hovered:
                target = state.idle_target(point.y)
                if target is not None:
                    write_control(target)

        def openSettings_(self, _sender):
            open_settings()

        def rightMouseDown_(self, event):
            state.move_idle()
            menu = AppKit.NSMenu.alloc().initWithTitle_("voice-io")
            item = menu.addItemWithTitle_action_keyEquivalent_(
                "Settings…", "openSettings:", "")
            item.setTarget_(self)
            AppKit.NSMenu.popUpContextMenu_withEvent_forView_(menu, event, self)

    class OverlayPanel(AppKit.NSPanel):
        def canBecomeKeyWindow(self):
            return False

        def canBecomeMainWindow(self):
            return False

    panel = OverlayPanel.alloc().initWithContentRect_styleMask_backing_defer_(
        Foundation.NSMakeRect(0, 0, 8, 32),
        AppKit.NSWindowStyleMaskBorderless | AppKit.NSWindowStyleMaskNonactivatingPanel,
        AppKit.NSBackingStoreBuffered, False)

    panel.setFloatingPanel_(True)
    panel.setHidesOnDeactivate_(False)
    panel.setLevel_(AppKit.NSStatusWindowLevel + 1)
    panel.setCollectionBehavior_(AppKit.NSWindowCollectionBehaviorCanJoinAllSpaces |
                                 AppKit.NSWindowCollectionBehaviorFullScreenAuxiliary)
    panel.setOpaque_(False)
    panel.setBackgroundColor_(AppKit.NSColor.clearColor())
    panel.setHasShadow_(False)
    view = OverlayView.alloc().initWithFrame_(Foundation.NSMakeRect(0, 0, 8, 32))
    panel.setContentView_(view)
    tracking = AppKit.NSTrackingArea.alloc().initWithRect_options_owner_userInfo_(
        Foundation.NSMakeRect(0, 0, 0, 0),
        AppKit.NSTrackingMouseEnteredAndExited | AppKit.NSTrackingMouseMoved |
        AppKit.NSTrackingActiveAlways | AppKit.NSTrackingInVisibleRect,
        view, None)
    view.addTrackingArea_(tracking)
    screen = AppKit.NSScreen.mainScreen()
    if screen is None:
        raise RuntimeError("No macOS display available for voice-io overlay")
    frame = screen.visibleFrame()
    screen_frame = screen.frame()
    center_y = frame.origin.y + frame.size.height / 2
    # The idle palette touches the actual monitor edge, not the inset visibleFrame.
    idle_right = screen_frame.origin.x + screen_frame.size.width
    active_x = frame.origin.x + frame.size.width - 42
    panel.setFrameOrigin_(Foundation.NSMakePoint(idle_right - 8, center_y - 16))

    # Short live draft, outside the narrow pill and transparent to input.
    preview_panel = AppKit.NSPanel.alloc().initWithContentRect_styleMask_backing_defer_(
        Foundation.NSMakeRect(0, 0, 194, 28),
        AppKit.NSWindowStyleMaskBorderless | AppKit.NSWindowStyleMaskNonactivatingPanel,
        AppKit.NSBackingStoreBuffered, False)
    preview_panel.setFloatingPanel_(True)
    preview_panel.setHidesOnDeactivate_(False)
    preview_panel.setIgnoresMouseEvents_(True)
    preview_panel.setLevel_(AppKit.NSStatusWindowLevel + 1)
    preview_panel.setCollectionBehavior_(panel.collectionBehavior())
    preview_panel.setOpaque_(False)
    preview_panel.setBackgroundColor_(AppKit.NSColor.clearColor())
    preview_label = AppKit.NSTextField.labelWithString_("")
    preview_label.setFrame_(Foundation.NSMakeRect(0, 0, 194, 28))
    preview_label.setFont_(AppKit.NSFont.systemFontOfSize_(11))
    rgb = _preview_rgb(Config.load().ui.preview_color, (0.92, 0.92, 0.92))
    preview_label.setTextColor_(
        AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(*rgb, 0.9))
    preview_label.setAlignment_(AppKit.NSTextAlignmentRight)
    preview_panel.setContentView_(preview_label)
    preview_panel.setFrameOrigin_(Foundation.NSMakePoint(
        max(frame.origin.x + 4, panel.frame().origin.x - 202),
        panel.frame().origin.y + 56))

    # Warning/error copy uses a separate bounded, mouse-transparent sidecar.
    warning_width = min(240, max(64, active_x - frame.origin.x - 12))
    warning_panel = AppKit.NSPanel.alloc().initWithContentRect_styleMask_backing_defer_(
        Foundation.NSMakeRect(0, 0, warning_width, 48),
        AppKit.NSWindowStyleMaskBorderless | AppKit.NSWindowStyleMaskNonactivatingPanel,
        AppKit.NSBackingStoreBuffered, False)
    warning_panel.setFloatingPanel_(True)
    warning_panel.setHidesOnDeactivate_(False)
    warning_panel.setIgnoresMouseEvents_(True)
    warning_panel.setLevel_(AppKit.NSStatusWindowLevel + 1)
    warning_panel.setCollectionBehavior_(panel.collectionBehavior())
    warning_panel.setOpaque_(False)
    warning_panel.setBackgroundColor_(AppKit.NSColor.clearColor())

    class WarningView(AppKit.NSView):
        def isOpaque(self):
            return False

        def acceptsFirstResponder(self):
            return False

        def drawRect_(self, _rect):
            bg = AppKit.NSBezierPath.bezierPathWithRoundedRect_xRadius_yRadius_(
                Foundation.NSMakeRect(0, 0, warning_width, 48), 8, 8)
            AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                .08, .10, .16, .96).set()
            bg.fill()

    warning_view = WarningView.alloc().initWithFrame_(
        Foundation.NSMakeRect(0, 0, warning_width, 48))
    warning_label = AppKit.NSTextField.labelWithString_("")
    warning_label.setFrame_(Foundation.NSMakeRect(8, 5, warning_width - 16, 38))
    warning_label.setFont_(AppKit.NSFont.systemFontOfSize_(12))
    warning_label.setWraps_(True)
    warning_label.setMaximumNumberOfLines_(2)
    warning_label.setLineBreakMode_(AppKit.NSLineBreakByTruncatingTail)
    warning_view.addSubview_(warning_label)
    warning_panel.setContentView_(warning_view)
    warning_panel.setFrameOrigin_(Foundation.NSMakePoint(
        panel.frame().origin.x - warning_width - 8,
        panel.frame().origin.y + 46))

    displayed_mode = ["tab"]

    def sync_panel(mode: str):
        width, height = (38, 140) if mode == "active" else (
            (28, 28) if mode == "hover" else (8, 32))
        x = active_x if mode == "active" else idle_right - width
        y = center_y - height / 2
        panel.setFrame_display_(Foundation.NSMakeRect(x, y, width, height), True)
        preview_panel.setFrameOrigin_(Foundation.NSMakePoint(
            max(frame.origin.x + 4, x - 202), y + (height - 28) / 2))
        warning_panel.setFrameOrigin_(Foundation.NSMakePoint(
            x - warning_width - 8, y + (height - 48) / 2))

    def reader():
        for raw in sys.stdin.buffer:
            try:
                msg = json.loads(raw)
                if isinstance(msg, dict):
                    messages.put(msg)
            except (UnicodeDecodeError, json.JSONDecodeError):
                continue
        messages.put({"op": "quit"})

    threading.Thread(target=reader, name="voice-io-overlay-stdin", daemon=True).start()

    class Ticker(AppKit.NSObject):
        def tick_(self, _timer):
            while True:
                try:
                    state.handle(messages.get_nowait())
                except queue.Empty:
                    break
            if state.quit:
                app.stop_(None)
                return
            if state.visible():
                state.hovered = False
                state.idle_snoozed = False
            mode = state.dock_mode()
            if mode != displayed_mode[0]:
                sync_panel(mode)
                displayed_mode[0] = mode
            panel.orderFrontRegardless()  # never makes the panel key
            view.setNeedsDisplay_(True)
            message = state.message_text()
            if message:
                warning_label.setStringValue_(message)
                color = ((.98, .73, .35) if state.style == "warn" else
                         (.99, .44, .42) if state.style == "error" else
                         (.55, .95, .65) if state.style == "ok" else
                         (.92, .95, 1.))
                warning_label.setTextColor_(
                    AppKit.NSColor.colorWithCalibratedRed_green_blue_alpha_(
                        *color, 1.))
                warning_panel.orderFrontRegardless()
            else:
                warning_panel.orderOut_(None)
            preview = state.preview_text()
            if preview:
                preview_label.setStringValue_(preview)
                preview_panel.orderFrontRegardless()
            else:
                preview_panel.orderOut_(None)

    ticker = Ticker.alloc().init()
    timer = Foundation.NSTimer.scheduledTimerWithTimeInterval_target_selector_userInfo_repeats_(
        .05, ticker, "tick:", None, True)
    app.run()
    timer.invalidate()
    panel.orderOut_(None)
    preview_panel.orderOut_(None)
    warning_panel.orderOut_(None)
    return 0


def main() -> int:
    if sys.platform != "darwin":
        raise RuntimeError("AppKit overlay requires macOS")
    return _run_settings() if "--settings" in sys.argv else _run_overlay()


if __name__ == "__main__":
    sys.exit(main())
