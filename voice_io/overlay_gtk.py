#!/usr/bin/env python3
"""voice-io overlay server — standalone GTK3/X11 pill (run via system python3).

Uses the system Python with GTK3/gi/cairo; the application venv need not
contain those distro-specific bindings. Reads JSONL ops from stdin:
  {"op":"state","value":"idle|listening|processing|off|error"}
  {"op":"ready","active":true|false} brief status pulse (no recording)
  {"op":"session","open":true|false,"started_at":ts}
  {"op":"preview","text":"..."}       transient, click-through side preview
  {"op":"level","value":0.0-1.0}      speech-gated raw mic RMS
  {"op":"show","text":"...","style":"info|ok|warn|prompt|error"}
  {"op":"result","text":"...","style":"...","auto_close_s":2}
  {"op":"clear"}  {"op":"quit"}
EOF -> quit.

Design: a tiny resting tab stays at the right edge and expands on hover
to separate Transcribe and Settings controls. A 38×140 vertical waveform
docks there only during a session. Listening follows speech-gated RMS;
a brief three-word draft appears beside the pill as words change, never
in it. Processing uses a static status meter; success fades away.
Warnings appear beside the pill.
Drag vertically to move the active strip; its center persists in
~/.config/voice-io/overlay.json. Click to finish, × to cancel, right-click
for Mute / Settings / Quit. No overlay window takes editor focus.

Rendering constraint: this gi build lacks _gi_cairo — the GTK draw signal
AND Gdk.pixbuf_get_from_surface both fail. Everything visual is drawn into
a pycairo ImageSurface -> GdkPixbuf.Pixbuf.new_from_data -> Gtk.Image. No
Gtk.DrawingArea anywhere.

Theming: [ui] in ~/.config/voice-io/config.toml — pill (pill|vu-meter|
waveform), accent (hex), theme (auto|dark|light).
"""

from __future__ import annotations

import json
import math
import os
import subprocess
import sys
import time
import tomllib
from pathlib import Path

# Launched as a standalone script by overlay.py, not always as a package.
if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from voice_io.config import Config
from voice_io.control import (apply_hotkeys, install_cancel_hotkey,
                              install_hotkey, install_toggle_hotkey)
from voice_io.levels import speech_level
from voice_io.kws import keyword_lines

import cairo  # pycairo: no gi cairo marshalling in this build (_gi_cairo absent)

import gi

gi.require_version("Gtk", "3.0")
gi.require_version("Gdk", "3.0")  # else Gdk resolves to 4.0 and GTK3 import fails
from gi.repository import Gdk, GdkPixbuf, GLib, Gtk  # noqa: E402

_DRAG_THRESHOLD_PX = 4.0

# Compact side dock: 13 bars fit the narrow active indicator.
_BAR_WIDTH = 2.0
_BAR_GAP = 2.0
_BAR_MIN = 2.0
_BAR_MAX = 14.0
_NUM_BARS = 13

# The resting tab stays flush with the screen edge; the active waveform
# retains its own inset position and size.
_TAB_W = 7
_TAB_H = 32
_CAPSULE_W = 38
_CAPSULE_H = 140
_CAPSULE_R = 999
_DOCK_GAP = 4
_PREVIEW_W = 240
_PREVIEW_H = 32
_PREVIEW_GAP = 8
_READY_W = 28
_READY_H = 28
_IDLE_EXPOSE_MS = 1800  # stale Wayland/XWayland leave events cannot pin the dot

_PILL_STYLES = ("pill", "vu-meter", "waveform")


_STYLE_COLORS = {
    "info": (1.0, 1.0, 1.0),
    "ok": (0.55, 0.95, 0.65),
    "warn": (1.0, 0.80, 0.35),
    "prompt": (0.60, 0.78, 1.0),
    "error": (1.0, 0.45, 0.45),
}


def _config_path() -> Path:
    override = os.environ.get("VOICE_IO_CONFIG")
    if override:
        return Path(override)
    base = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))
    return base / "voice-io" / "config.toml"


def _overlay_json() -> Path:
    base = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))
    return base / "voice-io" / "overlay.json"


def _control_fifo() -> Path:
    base = Path(os.environ.get("XDG_RUNTIME_DIR", Path.home() / ".config"))
    return base / "voice-io" / "control.fifo"


def _write_fifo(msg: str) -> None:
    fifo = _control_fifo()
    try:
        fd = os.open(fifo, os.O_WRONLY | os.O_NONBLOCK)
        try:
            os.write(fd, (msg + "\n").encode())
        finally:
            os.close(fd)
    except OSError:
        pass  # daemon not running; ignore


# --------------------------------------------------------------------------
# Theme: [ui] config + light/dark automatch
# --------------------------------------------------------------------------


def _hex_to_rgb(value: str) -> tuple[float, float, float] | None:
    value = (value or "").strip().lstrip("#")
    if len(value) != 6:
        return None
    try:
        return tuple(int(value[i:i + 2], 16) / 255.0 for i in (0, 2, 4))
    except ValueError:
        return None


def _prefer_dark() -> bool:
    """GNOME color-scheme automatch (via the Gtk settings that GNOME sets)."""
    try:
        st = Gtk.Settings.get_default()
        if st is not None:
            return bool(st.get_property("gtk-application-prefer-dark-theme"))
    except Exception:
        pass
    try:
        from gi.repository import Gio
        s = Gio.Settings.new("org.gnome.desktop.interface")
        return s.get_string("color-scheme") == "prefer-dark"
    except Exception:
        return True  # default dark (the pill's primary look)


class Theme:
    """Resolved pill colors from [ui] config + automatch."""

    def __init__(self) -> None:
        cfg = {}
        try:
            with open(_config_path(), "rb") as f:
                cfg = tomllib.load(f)
        except (OSError, tomllib.TOMLDecodeError):
            cfg = {}
        ui = cfg.get("ui", {}) if isinstance(cfg, dict) else {}
        self.pill_style = str(ui.get("pill", "pill"))
        if self.pill_style not in _PILL_STYLES:
            self.pill_style = "pill"
        self.accent = _hex_to_rgb(str(ui.get("accent", "#4f8cff"))) or (0.31, 0.55, 1.0)
        self.preview_color = (_hex_to_rgb(str(ui.get("preview_color", "")))
                              or _STYLE_COLORS["info"])
        choice = str(ui.get("theme", "auto"))
        self.dark = (choice != "light") if choice in ("dark", "light") \
            else _prefer_dark()

    # ---- palette ----
    @property
    def capsule_bg(self) -> str:
        return "rgba(4,4,5,0.96)" if self.dark else "rgba(250,250,252,0.96)"

    @property
    def capsule_border(self) -> str:
        return ("rgba(255,255,255,0.075)" if self.dark
                else "rgba(15,10,26,0.20)")

    @property
    def error_border(self) -> str:
        return ("rgba(255,80,80,0.30)" if self.dark
                else "rgba(200,30,30,0.35)")

    @property
    def text(self) -> tuple[float, float, float, float]:
        return (0.95, 0.95, 0.97, 1.0) if self.dark else (0.06, 0.04, 0.10, 1.0)

    @property
    def text_dim(self) -> tuple[float, float, float, float]:
        return (1.0, 1.0, 1.0, 0.55) if self.dark else (0.06, 0.04, 0.10, 0.55)

    @property
    def bars(self) -> tuple[float, float, float]:
        return (1.0, 1.0, 1.0) if self.dark else (0.10, 0.08, 0.16)



# --------------------------------------------------------------------------
# Audio meter and small transient status messages
# --------------------------------------------------------------------------


def _clamp(v: float, lo: float, hi: float) -> float:
    return max(lo, min(hi, v))

def _cairo_rgb(r: float, g: float, b: float) -> tuple[float, float, float]:
    """Cairo's native BGRA bytes are read as RGBA by Gtk.Image's pixbuf."""
    return b, g, r


# --------------------------------------------------------------------------
# Meter: animated audio visual (pill | vu-meter | waveform)
# --------------------------------------------------------------------------


def _rounded_rect(cr: cairo.Context, x: float, y: float, w: float, h: float,
                  r: float) -> None:
    r = min(r, w / 2.0, h / 2.0)
    cr.new_sub_path()
    cr.arc(x + w - r, y + r, r, -math.pi / 2, 0)
    cr.arc(x + w - r, y + h - r, r, 0, math.pi / 2)
    cr.arc(x + r, y + h - r, r, math.pi / 2, math.pi)
    cr.arc(x + r, y + r, r, math.pi, 3 * math.pi / 2)
    cr.close_path()


class Meter(Gtk.Image):
    """Animated mic meter. States: recording|processing|success|error|idle."""

    W = 22
    H = 82

    def __init__(self, theme: Theme) -> None:
        super().__init__()
        self.theme = theme
        self.state = "idle"
        self.level = 0.0
        self.phase = 0.0
        self.state_since = time.monotonic()
        self._heights = [0.0] * (_NUM_BARS if theme.pill_style == "pill"
                                 else 32)
        self._history = [0.0] * len(self._heights)
        self.set_size_request(self.W, self.H)
        self._render()

    def do_get_preferred_width(self):
        return (self.W, self.W)

    def do_get_preferred_height(self):
        return (self.H, self.H)

    # ---- input ----
    def set_state(self, state: str) -> None:
        if state != self.state:
            if state == "recording":
                self._history[:] = [0.0] * len(self._history)
                self.level = 0.0
            self.state = state
            self.state_since = time.monotonic()

    def set_level(self, value: float) -> None:
        self.level = speech_level(value)
        self._history.pop(0)
        self._history.append(self.level)

    def advance(self, dt: float) -> None:
        self.phase = (self.phase + dt * 7.2) % (2 * math.pi)

    # ---- animation ----
    def tick(self, dt: float) -> None:
        n = len(self._heights)
        if self.theme.pill_style == "waveform":
            target = self._wave_target(n)
        else:
            target = self._pill_target(n)
        rise = 1.0 - math.exp(-dt * 18.0)
        fall = 1.0 - math.exp(-dt * 9.0)
        changed = False
        for i in range(n):
            t = target[i]
            blend = rise if t > self._heights[i] else fall
            new = self._heights[i] + (t - self._heights[i]) * blend
            if abs(new - self._heights[i]) > 0.002:
                self._heights[i] = new
                changed = True
            else:
                self._heights[i] = t if abs(t - self._heights[i]) < 0.002 \
                    else self._heights[i]
        if changed or self.state in ("success", "error"):
            self._render()

    def _pill_target(self, n: int) -> list[float]:
        if self.state == "recording":
            return self._history
        if self.state == "processing":
            return self._processing_target(n)
        if self.state == "error":
            blink = 0.25 + 0.75 * abs(math.sin(self.phase * 2.5))
            return [blink] * n
        if self.state == "success":
            return [0.18] * n
        return [0.0] * n

    def _processing_target(self, n: int) -> list[float]:
        # A moving status sweep, independent of microphone level/history.
        center = (self.phase / (2 * math.pi)) * n
        return [0.08 + 0.55 * math.exp(
            -0.5 * (min(abs(i - center), n - abs(i - center)) / 1.5) ** 2)
                for i in range(n)]

    def _wave_target(self, n: int) -> list[float]:
        if self.state == "recording":
            return self._history
        if self.state == "processing":
            return self._processing_target(n)
        if self.state == "error":
            blink = 0.25 + 0.75 * abs(math.sin(self.phase * 2.5))
            return [blink] * n
        if self.state == "success":
            pulse = 0.7 + 0.3 * math.sin(self.phase)
            return [_clamp(max(h, 0.5) * pulse, 0.0, 1.0) for h in self._heights]
        return [0.0] * n

    # ---- drawing ----
    def _surface(self, scale: int):
        W, H = int(self.W * scale), int(self.H * scale)
        surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, W, H)
        cr = cairo.Context(surface)
        cr.scale(scale, scale)
        cr.set_operator(cairo.OPERATOR_CLEAR)
        cr.paint()
        cr.set_operator(cairo.OPERATOR_SOURCE)
        return surface, cr

    def _finish(self, surface: cairo.ImageSurface, scale: int) -> None:
        W, H = int(self.W * scale), int(self.H * scale)
        data = surface.get_data()
        pixbuf = GdkPixbuf.Pixbuf.new_from_data(
            data, GdkPixbuf.Colorspace.RGB, True, 8, W, H, W * 4)
        self.set_from_pixbuf(pixbuf)

    def _render(self) -> None:
        # Gtk.Image draws pixbuf pixels as widget pixels; a 2x buffer clips.
        scale = 1
        surface, cr = self._surface(scale)
        style = self.theme.pill_style
        if style == "pill":
            self._draw_pill(cr)
        elif style == "vu-meter":
            self._draw_vu(cr)
        else:
            self._draw_wave(cr)
        self._finish(surface, scale)

    def _bar_color(self, alpha: float) -> tuple[float, float, float, float]:
        if self.state == "processing":
            r, g, b = (0.98, 0.73, 0.35) if self.theme.dark else (0.64, 0.36, 0.06)
        elif self.state == "recording":
            r, g, b = self.theme.accent
            if self.theme.dark:
                r, g, b = (0.18 + 0.82 * r, 0.18 + 0.82 * g,
                           0.18 + 0.82 * b)
            else:
                r, g, b = (0.78 * r, 0.78 * g, 0.78 * b)
        elif self.state == "error":
            r, g, b = ((1.0, 0.42, 0.42) if self.theme.dark
                       else (0.7, 0.16, 0.16))
        else:
            r, g, b = self.theme.bars
        return (*_cairo_rgb(r, g, b), alpha)

    def _draw_pill(self, cr: cairo.Context) -> None:
        success = self.state == "success"
        elapsed = time.monotonic() - self.state_since
        if success:
            t = _clamp(elapsed / 0.22, 0.0, 1.0)
            morph = 1.0 - (1.0 - t) ** 3
            fade = 1.0 if elapsed <= 0.72 else max(
                0.0, 1.0 - (elapsed - 0.72) / 0.28)
        else:
            morph, fade = 0.0, 1.0
        total = _NUM_BARS * _BAR_WIDTH + (_NUM_BARS - 1) * _BAR_GAP
        start_y = (self.H - total) / 2.0
        cx = self.W / 2.0
        for i in range(_NUM_BARS):
            width = _BAR_MIN + _clamp(self._heights[i], 0.0, 1.0) * (
                _BAR_MAX - _BAR_MIN)
            opacity = 0.94
            if success:
                width = width * (1.0 - morph) + 3.0 * morph
                opacity *= fade
            elif self.state == "error":
                opacity *= 0.55 + 0.45 * abs(math.sin(self.phase * 2.5))
            if opacity <= 0.01:
                continue
            _rounded_rect(cr, cx - width / 2, start_y + i * (
                _BAR_WIDTH + _BAR_GAP), width, _BAR_WIDTH, _BAR_WIDTH / 2)
            cr.set_source_rgba(*self._bar_color(opacity))
            cr.fill()

    def _accent_at(self, t: float) -> tuple[float, float, float, float]:
        r, g, b = self.theme.accent
        # gradient: accent -> blended toward white 45%
        return (*_cairo_rgb(r + (1 - r) * t * 0.45,
                            g + (1 - g) * t * 0.45,
                            b + (1 - b) * t * 0.45), 1.0)

    def _draw_vu(self, cr: cairo.Context) -> None:
        pad = 5.0
        bar_w = self.W - pad * 2
        bar_h = self.H - pad * 2
        level = self.level if self.state == "recording" else 0.0
        if self.state == "error":
            level = 0.25 + 0.75 * abs(math.sin(self.phase * 2.5))
        elif self.state == "success":
            level = self.level
        alpha = 1.0
        if self.state == "success":
            elapsed = time.monotonic() - self.state_since
            alpha = 1.0 if elapsed <= 0.72 else max(
                0.0, 1.0 - (elapsed - 0.72) / 0.28)
        cr.set_source_rgba(0.15, 0.15, 0.15, alpha)
        _rounded_rect(cr, pad, pad, bar_w, bar_h, bar_h / 2.0)
        cr.fill()
        if self.state == "processing":
            # Moving segment, rather than a fabricated microphone reading.
            segment_h = 14.0
            segment_y = pad + (bar_h - segment_h) * (
                0.5 - 0.5 * math.cos(self.phase))
            cr.set_source_rgba(*self._bar_color(0.95))
            _rounded_rect(cr, pad, segment_y, bar_w, segment_h, bar_w / 2.0)
            cr.fill()
        else:
            lh = bar_h * _clamp(level, 0.0, 1.0)
            if lh > 0.5:
                grad = cairo.LinearGradient(0, pad + bar_h, 0, pad)
                grad.add_color_stop_rgb(0.0, *_cairo_rgb(*self.theme.accent))
                grad.add_color_stop_rgb(1.0, *self._accent_at(1.0)[:3])
                cr.set_source(grad)
                _rounded_rect(cr, pad, pad + bar_h - lh, bar_w, lh, bar_w / 2.0)
                cr.fill()


    def _draw_wave(self, cr: cairo.Context) -> None:
        n = len(self._heights)
        gap = 1.0
        min_w = 2.0
        bh = (self.H - (n - 1) * gap) / n
        total = n * bh + (n - 1) * gap
        start_y = (self.H - total) / 2.0
        cx = self.W / 2.0
        success = self.state == "success"
        elapsed = time.monotonic() - self.state_since
        pulse = 1.0
        if success:
            pulse = 0.7 + 0.3 * (0.7 + 0.3 * math.sin(self.phase))
            alpha_scale = 1.0 if elapsed <= 0.72 else max(
                0.0, 1.0 - (elapsed - 0.72) / 0.28)
        else:
            alpha_scale = 1.0
        for i in range(n):
            t = i / max(1, n - 1)
            color = (self._bar_color(1.0) if self.state == "processing"
                     else self._accent_at(t))
            w = max(min_w, _clamp(self._heights[i], 0.0, 1.0) * (self.W - 4))
            if success:
                w *= pulse
            opacity = 0.9 * alpha_scale
            if self.state == "error":
                opacity *= 0.55 + 0.45 * abs(math.sin(self.phase * 2.5))
            cr.set_source_rgba(color[0], color[1], color[2], opacity)
            _rounded_rect(cr, cx - w / 2, start_y + i * (bh + gap),
                          w, bh, min(bh / 2.0, w / 2.0))
            cr.fill()


# --------------------------------------------------------------------------
# Transient warnings beside the active dock
# --------------------------------------------------------------------------


class PreviewPanel(Gtk.Image):
    """Transparent, click-through preview and status text."""

    MSG_FONT = 13.0

    def __init__(self, theme: Theme) -> None:
        super().__init__()
        self._message = ""
        self._preview_color = theme.preview_color
        self._message_color = _STYLE_COLORS["info"]
        self.set_size_request(_PREVIEW_W, _PREVIEW_H)
        self._render()

    def do_get_preferred_width(self):
        return (_PREVIEW_W, _PREVIEW_W)

    def do_get_preferred_height(self):
        return (_PREVIEW_H, _PREVIEW_H)

    def set_message(self, text: str, style: str, *, preview: bool = False) -> None:
        self._message = text or ""
        self._message_color = (self._preview_color if preview else
                               _STYLE_COLORS.get(style, _STYLE_COLORS["info"]))
        self._render()

    def clear(self) -> None:
        self._message = ""
        self._render()

    def _surface(self, scale: int):
        W, H = int(_PREVIEW_W * scale), int(_PREVIEW_H * scale)
        surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, W, H)
        cr = cairo.Context(surface)
        cr.scale(scale, scale)
        cr.set_operator(cairo.OPERATOR_CLEAR)
        cr.paint()
        cr.set_operator(cairo.OPERATOR_SOURCE)
        return surface, cr

    def _finish(self, surface: cairo.ImageSurface, scale: int) -> None:
        W, H = int(_PREVIEW_W * scale), int(_PREVIEW_H * scale)
        data = surface.get_data()
        pixbuf = GdkPixbuf.Pixbuf.new_from_data(
            data, GdkPixbuf.Colorspace.RGB, True, 8, W, H, W * 4)
        self.set_from_pixbuf(pixbuf)

    def _wrap(self, text: str, cr: cairo.Context, font: float) -> list[str]:
        lines: list[str] = []
        cur = ""
        max_w = _PREVIEW_W - 16
        for word in (text or "").split():
            trial = (cur + " " + word).strip()
            cr.select_font_face("sans-serif", cairo.FONT_SLANT_NORMAL,
                                cairo.FONT_WEIGHT_NORMAL)
            cr.set_font_size(font)
            ext = cr.text_extents(trial)
            if ext.width > max_w and cur:
                lines.append(cur)
                cur = word
                if len(lines) == 2:
                    break
            else:
                cur = trial
        if cur and len(lines) < 2:
            lines.append(cur)
        return lines

    def _render(self) -> None:
        scale = 1
        surface, cr = self._surface(scale)
        if self._message:
            self._draw_message(cr, self._message)
        self._finish(surface, scale)

    def _draw_message(self, cr: cairo.Context, text: str) -> None:
        lines = self._wrap(text, cr, self.MSG_FONT)
        r, g, b = self._message_color
        cr.select_font_face("sans-serif", cairo.FONT_SLANT_NORMAL,
                            cairo.FONT_WEIGHT_NORMAL)
        cr.set_font_size(self.MSG_FONT)
        total_h = len(lines) * (self.MSG_FONT + 5)
        y = (_PREVIEW_H - total_h) / 2.0 + self.MSG_FONT
        for line in lines:
            ext = cr.text_extents(line)
            x = (_PREVIEW_W - ext.width) / 2.0
            cr.set_source_rgba(*_cairo_rgb(r, g, b), 0.95)
            cr.move_to(x, y)
            cr.show_text(line)
            y += self.MSG_FONT + 5


# --------------------------------------------------------------------------
# Capsule / window
# --------------------------------------------------------------------------


def _css_rgba(c: tuple[float, float, float, float]) -> str:
    """GTK3 CSS truncates fractional RGB values to ~0; emit integers."""
    r, g, b, a = c
    return f"rgba({round(r * 255)},{round(g * 255)},{round(b * 255)},{a:.2f})"


def _css_for(theme: Theme, radius: int, border: str, bg: str,
             state: str) -> str:
    text_css = _css_rgba(theme.text)
    dim_css = _css_rgba(theme.text_dim)
    if state == "off":
        dot_css = dim_css
    elif state == "error":
        dot_css = "rgb(255,105,105)" if theme.dark else "rgb(178,41,41)"
    else:
        dot_css = _css_rgba((*theme.accent, 1.0))
    if theme.dark:
        btn_bg = "rgba(255,255,255,0.14)"
        btn_bg_hover = "rgba(255,255,255,0.22)"
    else:
        btn_bg = "rgba(15,10,26,0.10)"
        btn_bg_hover = "rgba(15,10,26,0.16)"
    return f"""
window {{ background: transparent; }}
.voiceio {{ background: {bg}; border: 1px solid {border};
           border-radius: {radius}px; }}
.voiceio .status-dot {{ font-size: 12px; color: {dot_css}; }}
.voiceio button {{ font-size: 14px; padding: 0 4px;
                   background: {btn_bg};
                   color: {text_css};
                   border: none; border-radius: 12px; }}
.voiceio button:hover {{ background: {btn_bg_hover}; }}
"""


def _make_popup() -> Gtk.Window:
    win = Gtk.Window(type=Gtk.WindowType.POPUP)
    win.set_decorated(False)
    win.set_keep_above(True)
    # RGBA visual: lets the window background be fully transparent, so only
    # the rounded capsule shows (no sharp-cornered window rectangle behind).
    screen = win.get_screen()
    if screen is not None:
        rgba = screen.get_rgba_visual()
        if rgba is not None:
            win.set_visual(rgba)
    return win


def _load_pos() -> int | None:
    try:
        data = json.loads(_overlay_json().read_text(encoding="utf-8"))
        return int(data["dock_center_y"])
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
        return None


def _save_pos(center_y: int) -> None:
    try:
        p = _overlay_json()
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(json.dumps({"dock_center_y": center_y}))
    except OSError:
        pass




class OverlayWin:
    def __init__(self):
        self.theme = Theme()
        self._build_pill()
        self._build_preview()
        self._build_ready()
        self._session = False
        self._state = "idle"
        self._tick_id: int | None = None
        self._last_tick = time.monotonic()
        self._drag_active = False
        self._drag_dy = 0.0
        self._drag_moved = False
        self._dock_center_y = _load_pos()
        self._preview_until = 0.0
        self._preview_style = ""
        self._ready_id: int | None = None
        self._ready_active = True
        self._idle_expanded = False
        self._idle_leave_id: int | None = None
        self._idle_expose_id: int | None = None
        self._idle_snoozed = False
        self._success_pending = False
        self._buf = b""
        self._place()
        self._show_idle()
        self._apply_css()

    # ------------------------------------------------------------------ UI

    def _build_pill(self) -> None:
        self.pill = _make_popup()
        # A click starts/stops capture without stealing keyboard focus from
        # the editor that will receive the transcript.
        self.pill.set_accept_focus(False)
        self.pill.set_focus_on_map(False)
        self.pill.add_events(Gdk.EventMask.BUTTON_PRESS_MASK
                             | Gdk.EventMask.BUTTON_MOTION_MASK
                             | Gdk.EventMask.BUTTON_RELEASE_MASK)
        self.pill.connect("map", lambda *_a: self._rerender_all())
        self.status_dot = Gtk.Label(label="●")
        self.status_dot.get_style_context().add_class("status-dot")
        self.cancel_btn = Gtk.Button(label="×")
        self.cancel_btn.connect("clicked", self._on_cancel)
        self.meter = Meter(self.theme)
        stack = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=4)
        stack.set_halign(Gtk.Align.CENTER)
        stack.pack_start(self.cancel_btn, False, False, 0)
        stack.pack_start(self.meter, True, True, 0)
        stack.pack_start(self.status_dot, False, False, 0)
        self._box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        self._box.pack_start(stack, True, True, 0)
        self._box.set_margin_top(5)
        self._box.set_margin_bottom(5)
        self._box.set_margin_start(4)
        self._box.set_margin_end(4)
        self.pill.add(self._box)
        self.pill.get_style_context().add_class("voiceio")
        self.pill.connect("button-press-event", self._on_press)
        self.pill.connect("button-release-event", self._on_release)
        self.pill.connect("motion-notify-event", self._on_motion)
        self._menu = Gtk.Menu()
        for label, cb in (
            ("Mute / unmute", self._menu_toggle),
            ("Settings…", self._menu_settings),
            ("Quit voice-io", self._menu_quit),
        ):
            item = Gtk.MenuItem(label=label)
            item.connect("activate", cb)
            self._menu.append(item)
        self._menu.show_all()

    def _build_preview(self) -> None:
        self.preview_win = _make_popup()
        self.preview_win.set_accept_focus(False)
        self.preview_win.set_focus_on_map(False)
        self.preview_win.set_app_paintable(True)
        self.preview_win.connect(
            "map", lambda win: win.get_window().set_pass_through(True))
        self.preview_panel = PreviewPanel(self.theme)
        self.preview_win.add(self.preview_panel)
        self.preview_win.show_all()
        self.preview_win.move(-400, -400)  # hidden until first session

    def _build_ready(self) -> None:
        self.ready_win = _make_popup()
        self.ready_win.set_accept_focus(False)
        self.ready_win.set_focus_on_map(False)
        self.ready_win.set_app_paintable(True)
        self.ready_win.add_events(Gdk.EventMask.ENTER_NOTIFY_MASK
                                  | Gdk.EventMask.LEAVE_NOTIFY_MASK
                                  | Gdk.EventMask.BUTTON_PRESS_MASK)
        self.ready_win.connect("enter-notify-event", self._on_idle_enter)
        self.ready_win.connect("leave-notify-event", self._on_idle_leave)
        self.ready_win.connect("button-press-event", self._on_idle_press)

        # The edge tab folds out into one clickable status dot.
        self.ready_stack = Gtk.Stack()
        self.ready_stack.set_transition_type(Gtk.StackTransitionType.NONE)
        self.ready_stack.set_hhomogeneous(False)
        self.ready_stack.set_vhomogeneous(False)
        self.ready_tab = Gtk.EventBox()
        self.ready_tab.set_visible_window(True)
        self.ready_tab.add_events(Gdk.EventMask.ENTER_NOTIFY_MASK
                                  | Gdk.EventMask.LEAVE_NOTIFY_MASK
                                  | Gdk.EventMask.BUTTON_PRESS_MASK)
        self.ready_tab.connect("enter-notify-event", self._on_idle_enter)
        self.ready_tab.connect("leave-notify-event", self._on_idle_leave)
        self.ready_tab.connect("button-press-event", self._on_idle_press)
        self.ready_tab.set_size_request(_TAB_W, _TAB_H)
        self.ready_tab.get_style_context().add_class("idle-tab")
        self.ready_stack.add_named(self.ready_tab, "tab")

        self.ready_palette = Gtk.EventBox()
        self.ready_palette.set_visible_window(True)
        self.ready_palette.set_size_request(_READY_W, _READY_H)
        self.ready_palette.add_events(Gdk.EventMask.ENTER_NOTIFY_MASK
                                      | Gdk.EventMask.LEAVE_NOTIFY_MASK
                                      | Gdk.EventMask.POINTER_MOTION_MASK
                                      | Gdk.EventMask.BUTTON_PRESS_MASK
                                      | Gdk.EventMask.BUTTON_RELEASE_MASK)
        self.ready_palette.connect("enter-notify-event", self._on_idle_enter)
        self.ready_palette.connect("leave-notify-event", self._on_idle_leave)
        self.ready_palette.connect("motion-notify-event", self._on_idle_motion)
        self.ready_palette.connect("button-press-event", self._on_idle_press)
        self.ready_palette.connect("button-release-event", self._on_idle_release)
        self.ready_palette.get_style_context().add_class("idle-palette")
        self.ready_dot = Gtk.Image()
        self._render_idle_dot(False)
        self.ready_palette.add(self.ready_dot)
        self.ready_stack.add_named(self.ready_palette, "palette")
        self.ready_stack.set_visible_child_name("tab")
        self.ready_win.add(self.ready_stack)

        accent = _css_rgba((*self.theme.accent, 1.0))
        accent_dim = _css_rgba((*self.theme.accent, 0.75))
        css = f"""
window {{ background: transparent; }}
.idle-tab {{ background: {self.theme.capsule_bg};
             border: 1px solid {self.theme.capsule_border};
             border-right: none;
             border-radius: 5px 0 0 5px;
             box-shadow: inset 2px 0 {accent_dim}; }}
.idle-tab.pulse {{ box-shadow: inset 3px 0 {accent}; }}
.idle-tab.muted {{ opacity: 0.6; }}
.idle-palette {{ background: {self.theme.capsule_bg};
                 border: 1px solid {self.theme.capsule_border};
                 border-radius: 14px; }}
"""
        provider = Gtk.CssProvider()
        provider.load_from_data(css.encode())
        for widget in (self.ready_win, self.ready_stack, self.ready_tab,
                       self.ready_palette, self.ready_dot):
            widget.get_style_context().add_provider(
                provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

    def _render_idle_dot(self, muted: bool) -> None:
        # A font bullet is not optically centered; paint a circular pixel
        # target at the exact center of the 28×28 resting button.
        surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, _READY_W, _READY_H)
        cr = cairo.Context(surface)
        cr.set_operator(cairo.OPERATOR_CLEAR)
        cr.paint()
        cr.set_operator(cairo.OPERATOR_SOURCE)
        r, g, b = (self.theme.text_dim[:3] if muted else self.theme.accent)
        cr.set_source_rgba(*_cairo_rgb(r, g, b), 0.55 if muted else 1.0)
        cr.arc(_READY_W / 2, _READY_H / 2, 6.0, 0, 2 * math.pi)
        cr.fill()
        pixels = surface.get_data()
        pixbuf = GdkPixbuf.Pixbuf.new_from_data(
            pixels, GdkPixbuf.Colorspace.RGB, True, 8,
            _READY_W, _READY_H, _READY_W * 4)
        self.ready_dot.set_from_pixbuf(pixbuf)

    def _apply_css(self) -> None:
        radius = _CAPSULE_R
        border = self.theme.error_border if self._state == "error" \
            else self.theme.capsule_border
        provider = Gtk.CssProvider()
        provider.load_from_data(_css_for(self.theme, radius, border,
                                         self.theme.capsule_bg,
                                         self._state).encode())
        # GTK3 does not cascade window providers to children — attach to each.
        for w in (self.pill, self.status_dot, self.cancel_btn):
            w.get_style_context().add_provider(
                provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

    # ----------------------------------------------------------- placement

    def _geo(self) -> Gdk.Rectangle:
        display = Gdk.Display.get_default()
        return display.get_primary_monitor().get_geometry()

    def _place(self) -> None:
        geo = self._geo()
        width = _CAPSULE_W
        height = _CAPSULE_H
        center_y = (self._dock_center_y if self._dock_center_y is not None
                    else geo.y + geo.height // 2)
        y = max(geo.y + _DOCK_GAP,
                min(center_y - height // 2,
                    geo.y + geo.height - height - _DOCK_GAP))
        self.pill.move(geo.x + geo.width - width - _DOCK_GAP, y)

    def _place_ready(self) -> None:
        geo = self._geo()
        width = _READY_W if self._idle_expanded else _TAB_W
        height = _READY_H if self._idle_expanded else _TAB_H
        center_y = (self._dock_center_y if self._dock_center_y is not None
                    else geo.y + geo.height // 2)
        y = max(geo.y, min(center_y - height // 2,
                            geo.y + geo.height - height))
        # No right-hand gap: the resting hit area reaches the screen edge.
        self.ready_win.move(geo.x + geo.width - width, y)

    def _sync_preview_pos(self) -> None:
        x, y = self.pill.get_position()
        geo = self._geo()
        pw = self.preview_panel.get_allocated_width() or _PREVIEW_W
        self.preview_win.move(max(geo.x + _DOCK_GAP,
                                  x - pw - _PREVIEW_GAP),
                              max(geo.y + _DOCK_GAP,
                                  min(y, geo.y + geo.height - _PREVIEW_H - _DOCK_GAP)))

    def _rerender_all(self) -> None:
        # Keep the image in sync after the popup has been mapped.
        self.meter._render()
        self.preview_panel._render()

    # -------------------------------------------------------------- dragging

    def _on_cancel_target(self, ev: Gdk.EventButton) -> bool:
        widget = Gtk.get_event_widget(ev)
        while widget is not None:
            if widget is self.cancel_btn:
                return True
            widget = widget.get_parent()
        return False

    def _on_press(self, win: Gtk.Window, ev: Gdk.EventButton) -> bool:
        if self._on_cancel_target(ev):
            self._drag_active = False
            return False
        if ev.button == 1:
            _x, y = win.get_position()
            self._drag_active = True
            self._drag_moved = False
            self._drag_dy = ev.y_root - y
        elif ev.button == 3:
            self._menu.popup_at_pointer(ev)
            return True
        return False

    def _menu_toggle(self, *_args) -> None:  # noqa: ANN002 — GTK callback
        _write_fifo("toggle")

    def _menu_settings(self, *_args) -> None:  # noqa: ANN002 — GTK callback
        subprocess.Popen(
            [sys.executable, os.path.abspath(__file__), "--settings"])

    def _on_idle_release(self, _widget: Gtk.Widget, ev: Gdk.EventButton) -> bool:
        if ev.button == 1 and self._idle_expanded:
            # A deliberate click on the dot; hovering never starts capture.
            _write_fifo("toggle" if self._state == "off" else "push")
            return True
        return False

    def _on_idle_press(self, _widget: Gtk.Widget, ev: Gdk.EventButton) -> bool:
        if ev.button == 3:
            if self._idle_expanded:
                self._touch_idle()
            self._menu.popup_at_pointer(ev)
            return True
        return False

    def _cancel_idle_leave(self) -> None:
        if self._idle_leave_id is not None:
            GLib.source_remove(self._idle_leave_id)
            self._idle_leave_id = None

    def _cancel_idle_exposure(self) -> None:
        if self._idle_expose_id is not None:
            GLib.source_remove(self._idle_expose_id)
            self._idle_expose_id = None

    def _touch_idle(self) -> None:
        self._cancel_idle_exposure()
        self._idle_expose_id = GLib.timeout_add(
            _IDLE_EXPOSE_MS, self._expire_idle)

    def _expire_idle(self) -> bool:
        self._idle_expose_id = None
        self._idle_snoozed = True
        self._collapse_idle()
        return False

    def _on_idle_motion(self, *_args) -> bool:  # noqa: ANN002 — GTK callback
        if self._idle_snoozed:
            self._idle_snoozed = False
            self._expand_idle()
        elif self._idle_expanded:
            self._touch_idle()
        return False

    def _expand_idle(self) -> None:
        self._cancel_idle_leave()
        if self._session or not self.ready_win.get_visible():
            return
        if not self._idle_expanded:
            self._idle_expanded = True
            self.ready_stack.set_visible_child_name("palette")
            self.ready_win.resize(_READY_W, _READY_H)
            self._place_ready()
            self._touch_idle()

    def _collapse_idle(self) -> None:
        self._cancel_idle_leave()
        self._cancel_idle_exposure()
        if self._idle_expanded:
            self._idle_expanded = False
            self.ready_stack.set_visible_child_name("tab")
            self.ready_win.resize(_TAB_W, _TAB_H)
        self._place_ready()

    def _on_idle_enter(self, *_args) -> bool:  # noqa: ANN002 — GTK callback
        if not self._idle_snoozed:
            self._expand_idle()
        return False

    def _on_idle_leave(self, *_args) -> bool:  # noqa: ANN002 — GTK callback
        # X11 emits leave/enter notifications while the popup grows leftwards
        # and while crossing child widgets. Inspect the actual pointer after
        # geometry settles rather than collapsing on those intermediate events.
        if self._idle_leave_id is None:
            self._idle_leave_id = GLib.timeout_add(90, self._collapse_if_outside)
        return False

    def _collapse_if_outside(self) -> bool:
        self._idle_leave_id = None
        window = self.ready_win.get_window()
        if window is not None and self.ready_win.get_visible():
            pointer = Gdk.Display.get_default().get_default_seat().get_pointer()
            _child, x, y, _mask = window.get_device_position(pointer)
            width, height = self.ready_win.get_size()
            if not (0 <= x < width and 0 <= y < height):
                self._idle_snoozed = False
                self._collapse_idle()
        return False

    def _menu_quit(self, *_args) -> None:  # noqa: ANN002 — GTK callback
        _write_fifo("quit")
        self.quit()

    def _on_motion(self, win: Gtk.Window, ev: Gdk.EventMotion) -> bool:
        if not self._drag_active:
            return False
        if not self._drag_moved:
            _x, y = win.get_position()
            if abs(ev.y_root - y - self._drag_dy) < _DRAG_THRESHOLD_PX:
                return False
            self._drag_moved = True
        geo = self._geo()
        _width, height = win.get_size()
        ny = max(geo.y + _DOCK_GAP,
                 min(int(ev.y_root - self._drag_dy),
                     geo.y + geo.height - height - _DOCK_GAP))
        win.move(win.get_position()[0], ny)
        self._sync_preview_pos()
        return True

    def _on_release(self, win: Gtk.Window, ev: Gdk.EventButton) -> bool:
        if self._on_cancel_target(ev):
            self._drag_active = False
            return False
        if ev.button == 1 and self._drag_active:
            if self._drag_moved:
                self._dock_center_y = win.get_position()[1] + win.get_size()[1] // 2
                _save_pos(self._dock_center_y)
            else:
                _write_fifo("toggle" if self._state == "off" else "push")
            self._drag_active = False
        return False

    def _on_cancel(self, *_args) -> None:  # noqa: ANN002 — GTK callback
        _write_fifo("cancel")

    # ------------------------------------------------------------- session

    def _show_idle(self) -> None:
        self.preview_win.hide()
        self.pill.hide()
        self._collapse_idle()
        self.ready_win.show_all()

    def _end_ready_pulse(self) -> bool:
        self.ready_tab.get_style_context().remove_class("pulse")
        self._ready_id = None
        return False

    def _update_ready_style(self) -> None:
        muted = self._state == "off" or not self._ready_active
        ctx = self.ready_tab.get_style_context()
        if muted:
            ctx.add_class("muted")
        else:
            ctx.remove_class("muted")
        self._render_idle_dot(muted)

    def ready(self, active: bool) -> None:
        self._ready_active = active
        self._update_ready_style()
        if self._session:
            return
        if self._ready_id is not None:
            GLib.source_remove(self._ready_id)
            self._end_ready_pulse()
        if active:
            self.ready_tab.get_style_context().add_class("pulse")
            self._ready_id = GLib.timeout_add(4000, self._end_ready_pulse)
        if (self._state in ("idle", "off") and not self.ready_win.get_visible()
                and not self._success_pending and not self._preview_until):
            self._show_idle()


    def _show_session(self) -> None:
        if self._ready_id is not None:
            GLib.source_remove(self._ready_id)
            self._end_ready_pulse()
        self._cancel_idle_leave()
        self._idle_snoozed = False
        self.ready_win.hide()
        self.preview_win.hide()
        self.preview_panel.clear()
        self._preview_until = 0.0
        self._preview_style = ""
        self.pill.resize(_CAPSULE_W, _CAPSULE_H)
        self._place()
        self.pill.show_all()


    def _start_tick(self) -> None:
        if self._tick_id is None:
            self._last_tick = time.monotonic()
            self._tick_id = GLib.timeout_add(33, self._tick)

    def _stop_tick(self) -> None:
        if self._tick_id is not None:
            GLib.source_remove(self._tick_id)
            self._tick_id = None

    def _tick(self) -> bool:
        now = time.monotonic()
        dt = min(0.05, max(0.001, now - self._last_tick))
        self._last_tick = now
        self.meter.advance(dt)
        self.meter.tick(dt)
        if self._preview_until and now >= self._preview_until:
            self._preview_until = 0.0
            self._preview_style = ""
            self.preview_win.hide()
            self.preview_panel.clear()
            if self.meter.state == "error" and self._session:
                self.meter.set_state("recording")
            if not self._session:
                self._success_pending = False
                if self._state != "processing":
                    self._settle_idle()
        # Success: after the fade completes, settle back to idle.
        if (self._success_pending
                and self.meter.state == "success"
                and now - self.meter.state_since > 1.0):
            self._success_pending = False
            self.meter.set_state("recording" if self._session else "idle")
            self._apply_css()
            if (not self._session and not self._preview_until
                    and self._state != "processing"):
                self._settle_idle()
        if (self._session or self._success_pending or self._preview_until):
            return True
        self._stop_tick()
        return False

    def _settle_idle(self) -> None:
        self._preview_until = 0.0
        self._preview_style = ""
        self.meter.set_state("idle")
        self.preview_panel.clear()
        self._show_idle()
        self._apply_css()
        self.pill.queue_draw()

    # ------------------------------------------------------------------ ops

    def state(self, value: str) -> None:
        self._state = value
        if value in ("idle", "off"):
            self._ready_active = value != "off"
            self._update_ready_style()
            if (not self._session and not self._success_pending
                    and not self._preview_until):
                self._settle_idle()
        elif value in ("listening", "processing", "error"):
            self._cancel_idle_leave()
            self.ready_win.hide()
        if value == "processing" and self._preview_style == "info":
            self.clear()
        self._apply_css()
        self.pill.queue_draw()
        if not self._success_pending and not (
                self._preview_until > time.monotonic()
                and self.meter.state == "error" and value == "idle"):
            meter_state = {"listening": "recording",
                           "processing": "processing",
                           "error": "error"}.get(value, "idle")
            self.meter.set_state(meter_state)

    def session_open(self, started_at: float, language: str = "en") -> None:
        self._session = True
        self._success_pending = False
        self._show_session()
        self._apply_css()
        self._start_tick()

    def session_close(self) -> None:
        self._session = False
        if self._state == "listening":
            self.state("idle")
        if self._success_pending:
            return  # allow success to fade before hiding
        if time.monotonic() < self._preview_until < float("inf"):
            return  # let a transient warning remain readable after closing
        if self._state == "processing":
            return  # retain the active pill until processing actually ends
        self._settle_idle()

    def preview(self, text: str) -> None:
        now = time.monotonic()
        if (not self._session or self._state != "listening" or not text
                or (self._preview_style not in ("", "info")
                    and now < self._preview_until)):
            return
        self.preview_panel.set_message(text, "info", preview=True)
        self._preview_style = "info"
        self._preview_until = now + 1.1
        self._sync_preview_pos()
        self.preview_win.show()
        self._start_tick()

    def level(self, value: float) -> None:
        if self._session:
            self.meter.set_level(value)

    def show(self, text: str, style: str) -> None:
        if not text:
            self.clear()
            return
        if style == "info":
            self.preview(text)
            return
        self.preview_panel.set_message(text, style)
        self._preview_style = style
        self._sync_preview_pos()
        self.preview_win.show()
        self._preview_until = (float("inf") if style == "prompt"
                               else time.monotonic() + 1.4)
        self._start_tick()

    def result(self, text: str, style: str, auto_close_s: float,
               show_text: bool = False) -> None:
        if style == "ok":
            self.clear()
            self._success_pending = True
            self.meter.set_state("success")
            self.meter.state_since = time.monotonic()
            if show_text and text:
                self.show(text, "ok")
                self._preview_until = time.monotonic() + auto_close_s
        else:
            self.show(text, style)
            self._preview_until = time.monotonic() + auto_close_s
            self.meter.set_state("error")
            self.meter.state_since = time.monotonic()
        self._start_tick()

    def clear(self) -> None:
        self._preview_until = 0.0
        self._preview_style = ""
        self.preview_win.hide()
        self.preview_panel.clear()

    def quit(self) -> None:
        Gtk.main_quit()

    # ---------------------------------------------------------------- loop

    def _handle(self, msg: dict) -> None:
        op = msg.get("op")
        if op == "state":
            self.state(str(msg.get("value", "idle")))
        elif op == "session":
            if msg.get("open"):
                self.session_open(float(msg.get("started_at", 0)),
                                  str(msg.get("language", "en")))
            else:
                self.session_close()
        elif op == "ready":
            self.ready(bool(msg.get("active", True)))
        elif op == "preview":
            self.preview(str(msg.get("text", "")))
        elif op == "level":
            self.level(float(msg.get("value", 0.0)))
        elif op == "show":
            self.show(str(msg.get("text", "")), str(msg.get("style", "info")))
        elif op == "result":
            self.result(str(msg.get("text", "")), str(msg.get("style", "ok")),
                        float(msg.get("auto_close_s", 2)),
                        bool(msg.get("show_text", False)))
        elif op == "clear":
            self.clear()
        elif op == "quit":
            self.quit()

    def feed(self, data: bytes) -> None:
        self._buf += data
        while b"\n" in self._buf:
            line, self._buf = self._buf.split(b"\n", 1)
            line = line.decode("utf-8", "replace").strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue
            self._handle(msg)



# --------------------------------------------------------------------------
# Settings window (--settings mode)
# --------------------------------------------------------------------------



def _rgba_hex(rgba: Gdk.RGBA) -> str:
    return "#%02x%02x%02x" % (int(rgba.red * 255 + 0.5),
                              int(rgba.green * 255 + 0.5),
                              int(rgba.blue * 255 + 0.5))


def _run_settings() -> int:
    cfg = Config.load(_config_path())
    ui = {"pill": cfg.ui.pill, "theme": cfg.ui.theme,
          "accent": cfg.ui.accent, "sounds": cfg.ui.sounds,
          "live_preview": cfg.ui.live_preview,
          "preview_color": cfg.ui.preview_color}
    win = Gtk.Window(title="voice-io settings")
    screen = Gdk.Screen.get_default()
    win.set_default_size(420, min(560, max(200, screen.get_height()
                                         // max(1, win.get_scale_factor()) - 96))
                         if screen else 560)
    win.set_border_width(12)
    win.get_style_context().add_class("voiceio-settings")
    dark = cfg.ui.theme == "dark" or (cfg.ui.theme == "auto" and _prefer_dark())
    if dark:
        rgb = _hex_to_rgb(cfg.ui.accent) or (0.26, 0.84, 0.60)
        accent = "#%02x%02x%02x" % tuple(round(c * 255) for c in rgb)
        hover = "#%02x%02x%02x" % tuple(
            round((c * 0.78 + 0.22) * 255) for c in rgb)
        contrast = "#0c1c18" if sum(rgb) > 1.35 else "#f8fbff"
        provider = Gtk.CssProvider()
        css = b"""
.voiceio-settings { background-color: #15191f; color: #eaf0f4; }
.voiceio-settings label { color: #eaf0f4; }
.voiceio-settings label.section-title { color: #42d69a; font-weight: 700; }
.voiceio-settings entry, .voiceio-settings button {
    color: #eef5f8; background-color: #242d38; background-image: none;
    border: 1px solid #425062; border-radius: 8px; padding: 4px 9px;
    box-shadow: none; text-shadow: none;
}
.voiceio-settings button:hover {
    background-color: #344454; background-image: none;
}
.voiceio-settings entry:focus { border-color: #42d69a; }
.voiceio-settings button.primary {
    color: #0c1c18; background-color: #42d69a; background-image: none;
    font-weight: 700;
}
.voiceio-settings button.primary:hover {
    background-color: #69e5b1; background-image: none;
}
.voiceio-settings checkbutton check:checked {
    background-color: #42d69a; background-image: none;
    border-color: #42d69a;
}
"""
        provider.load_from_data(
            css.replace(b"#42d69a", accent.encode())
               .replace(b"#69e5b1", hover.encode())
               .replace(b"#0c1c18", contrast.encode()))
        Gtk.StyleContext.add_provider_for_screen(
            Gdk.Screen.get_default(), provider,
            Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)

    def combo(values, current):
        cb = Gtk.ComboBoxText()
        for v in values:
            cb.append_text(v)
        cb.set_active(values.index(current) if current in values else 0)
        return cb

    style_box = combo(list(_PILL_STYLES), str(ui.get("pill", "pill")))
    theme_box = combo(["auto", "dark", "light"], str(ui.get("theme", "auto")))
    color_btn = Gtk.ColorButton()
    rgba = Gdk.RGBA()
    if not rgba.parse(str(ui.get("accent", "#4f8cff"))):
        rgba.parse("#4f8cff")
    color_btn.set_rgba(rgba)
    preview_color_btn = Gtk.ColorButton()
    preview_rgba = Gdk.RGBA()
    if not preview_rgba.parse(ui["preview_color"] or "#ffffff"):
        preview_rgba.parse("#ffffff")
    preview_color_btn.set_rgba(preview_rgba)
    sounds = Gtk.CheckButton(label="Recording sounds")
    sounds.set_active(bool(ui.get("sounds", True)))
    live_preview = Gtk.CheckButton(label="Live three-word preview")
    live_preview.set_active(bool(ui["live_preview"]))
    entries = {}
    for key, label, value in (
            ("push", "Push shortcut", cfg.hotkeys.push),
            ("toggle", "Toggle shortcut", cfg.hotkeys.toggle),
            ("cancel", "Cancel shortcut", cfg.hotkeys.cancel),
            ("wake_phrase", "Wake phrase", cfg.kws.wake_phrase),
            ("end_phrase", "End phrase", cfg.kws.end_phrase)):
        entry = Gtk.Entry()
        entry.set_text(value)
        entry.set_width_chars(23)
        entries[key] = (label, entry)
    original_hotkeys = (cfg.hotkeys.push, cfg.hotkeys.toggle, cfg.hotkeys.cancel)
    mode_box = combo(["dictation", "meeting", "collaborative"], cfg.meeting.mode)
    meeting_entries = {}
    for key, value in (
            ("summary_endpoint", cfg.meeting.summary_endpoint),
            ("summary_model", cfg.meeting.summary_model),
            ("summary_api_key_env", cfg.meeting.summary_api_key_env)):
        entry = Gtk.Entry()
        entry.set_text(value)
        entry.set_width_chars(23)
        meeting_entries[key] = entry

    pos = _load_pos()
    pos_label = Gtk.Label(
        label=(f"Right-edge center: {pos}" if pos is not None
               else "Dock position: right edge"), xalign=0)
    reset_btn = Gtk.Button(label="Reset dock position")
    pos_box = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
    pos_box.pack_start(pos_label, True, True, 0)
    pos_box.pack_start(reset_btn, False, False, 0)

    note = Gtk.Label(
        label="Drag vertically to move the bar along the right edge.",
        xalign=0)

    box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=10)

    def heading(text):
        label = Gtk.Label(label=text, xalign=0)
        label.get_style_context().add_class("section-title")
        box.add(label)

    def row(label_text, widget):
        l = Gtk.Label(label=label_text, xalign=1)
        hbox = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=8)
        hbox.pack_start(l, True, True, 0)
        hbox.pack_start(widget, False, False, 0)
        box.add(hbox)

    heading("APPEARANCE")
    row("Recording pill", style_box)
    row("Theme", theme_box)
    row("Accent", color_btn)
    box.add(sounds)
    box.add(live_preview)
    row("Preview text color", preview_color_btn)
    heading("MODE & MEETING")
    row("Recording mode", mode_box)
    meeting_note = Gtk.Label(
        label="Meeting saves transcript + optional summary, not pasted text. "
              "Collaborative pastes an editable speaker-labelled draft on Stop; "
              "it never submits.",
        xalign=0)
    meeting_note.set_line_wrap(True)
    meeting_note.set_max_width_chars(46)
    box.add(meeting_note)
    install_note = Gtk.Label(
        label="Meeting and Collaborative require voice-io models install --meeting. "
              "Leave endpoint or model empty to disable meeting summaries.",
        xalign=0)
    install_note.set_line_wrap(True)
    install_note.set_max_width_chars(46)
    box.add(install_note)
    row("Summary endpoint", meeting_entries["summary_endpoint"])
    row("Summary model", meeting_entries["summary_model"])
    row("API key env name", meeting_entries["summary_api_key_env"])
    heading("VOICE & SHORTCUTS")
    for label, entry in entries.values():
        row(label, entry)
    hint = Gtk.Label(label="Shortcuts: ctrl+alt+space (modifiers + key). "
                          "Save, then enable GNOME shortcuts once.", xalign=0)
    hint.set_line_wrap(True)
    hint.set_max_width_chars(46)
    box.add(hint)
    status = Gtk.Label(label="", xalign=0)
    status.set_line_wrap(True)
    save_btn = Gtk.Button(label="Save settings")
    save_btn.get_style_context().add_class("primary")
    register_btn = Gtk.Button(label="Enable GNOME shortcuts")
    box.add(register_btn)
    box.add(pos_box)
    box.add(note)
    scroller = Gtk.ScrolledWindow()
    scroller.set_policy(Gtk.PolicyType.NEVER, Gtk.PolicyType.AUTOMATIC)
    scroller.add(box)
    outer = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=8)
    outer.pack_start(scroller, True, True, 0)
    outer.pack_start(status, False, False, 0)
    outer.pack_start(save_btn, False, False, 0)
    win.add(outer)

    def save(*_a):
        nonlocal original_hotkeys
        hotkeys = {key: entries[key][1].get_text().strip()
                   for key in ("push", "toggle", "cancel")}
        phrases = {key: entries[key][1].get_text().strip()
                   for key in ("wake_phrase", "end_phrase")}
        try:
            # Reject unsupported vocabulary before persisting unusable phrases.
            keyword_lines(cfg.resolve(cfg.kws.model_dir),
                          phrases["wake_phrase"], phrases["end_phrase"])
            Config.save_user({
                "ui": {"pill": style_box.get_active_text() or "pill",
                       "theme": theme_box.get_active_text() or "auto",
                       "accent": _rgba_hex(color_btn.get_rgba()),
                       "sounds": sounds.get_active(),
                       "live_preview": live_preview.get_active(),
                       "preview_color": _rgba_hex(preview_color_btn.get_rgba())},
                "hotkeys": hotkeys, "kws": phrases,
                "meeting": {
                    "mode": mode_box.get_active_text() or "dictation",
                    **{key: entry.get_text().strip()
                       for key, entry in meeting_entries.items()},
                }}, _config_path())
            changed = tuple(hotkeys[key] for key in ("push", "toggle", "cancel"))
            for old, new, install in zip(
                    original_hotkeys, changed,
                    (install_hotkey, install_toggle_hotkey,
                     install_cancel_hotkey)):
                if old != new:
                    error = install(new)
                    if error:
                        raise RuntimeError("Settings saved, but GNOME shortcut "
                                           f"could not be applied: {error}")
            original_hotkeys = changed
            status.set_text("Settings saved. Restart voice-io to apply changes.")
        except (OSError, ValueError, RuntimeError, tomllib.TOMLDecodeError) as e:
            status.set_text(str(e))

    def reset_pos(*_a):
        try:
            _overlay_json().unlink()
        except OSError:
            pass
        pos_label.set_text("Dock position: right edge")

    def register_hotkeys(*_a):
        error = apply_hotkeys(Config.load(_config_path()).hotkeys)
        status.set_text(f"Could not enable shortcuts: {error}" if error
                        else "GNOME shortcuts enabled.")

    save_btn.connect("clicked", save)
    register_btn.connect("clicked", register_hotkeys)
    reset_btn.connect("clicked", reset_pos)

    win.show_all()
    Gtk.main()
    return 0


def main() -> int:
    if "--settings" in sys.argv:
        return _run_settings()
    win = OverlayWin()

    def on_io(fd: int, _cond: int) -> bool:
        try:
            data = os.read(fd, 4096)
        except OSError:
            return False
        if not data:  # EOF: writer went away
            win.quit()
            return False
        win.feed(data)
        return True

    GLib.io_add_watch(sys.stdin.fileno(), GLib.IO_IN | GLib.IO_HUP, on_io)
    Gtk.main()
    return 0


if __name__ == "__main__":
    sys.exit(main())
