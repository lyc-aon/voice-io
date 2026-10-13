#!/usr/bin/env python3
"""Print a verified focused-window identity, or nothing if focus is unknown.

GNOME Wayland does not expose native clients (including Tern) through AT-SPI.
The optional, explicitly installed gnome_focus@voice-io Shell extension reads
the compositor's current focus. Its per-window sequence and extension-instance
ID remain stable while recording, even if the window title changes. Without
that bridge, native Wayland focus is unknown: never substitute stale AT-SPI or
the focused Xwayland window for the compositor's current keyboard target.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

import gi

gi.require_version("Atspi", "2.0")
from gi.repository import Atspi, Gio, GLib  # noqa: E402


_FOCUS_BUS = "org.voiceio.Focus1"
_FOCUS_PATH = "/org/voiceio/Focus1"


def _call(connection, name: str, interface: str, method: str,
          arguments):
    return connection.call_sync(
        name, _FOCUS_PATH if interface == _FOCUS_BUS else "/org/freedesktop/DBus",
        interface, method, arguments, None, Gio.DBusCallFlags.NONE, 500,
        None).unpack()


def _wayland_focus() -> dict | None:
    """Trust only a reply from the running GNOME Shell, not a bus-name clone."""
    connection = Gio.bus_get_sync(Gio.BusType.SESSION, None)
    bus = "org.freedesktop.DBus"
    owner, = _call(connection, bus, bus, "GetNameOwner",
                   GLib.Variant("(s)", (_FOCUS_BUS,)))
    pid, = _call(connection, bus, bus, "GetConnectionUnixProcessID",
                 GLib.Variant("(s)", (owner,)))
    if os.path.basename(os.readlink(f"/proc/{pid}/exe")) != "gnome-shell":
        return None
    app, title, identity = _call(connection, owner, _FOCUS_BUS,
                                 "GetFocusedWindow", None)
    if not app or not identity:
        return None
    return {"app": app, "window": f"gnome:{identity}", "title": title}


def _safe_name(obj) -> str:
    try:
        return obj.get_name() or ""
    except Exception:  # noqa: BLE001 — probe must never crash
        return ""


def _contains_focus(window) -> bool:
    """Some toolkits focus the text widget, not its containing window."""
    pending = [(window, 0)]
    seen = 0
    while pending and seen < 96:
        node, depth = pending.pop(0)
        seen += 1
        try:
            states = node.get_state_set()
            if states is not None and (
                    states.contains(Atspi.StateType.FOCUSED)
                    or (depth == 0 and states.contains(Atspi.StateType.ACTIVE))):
                return True
            if depth < 4:
                for index in range(min(node.get_child_count(), 32)):
                    child = node.get_child_at_index(index)
                    if child is not None:
                        pending.append((child, depth + 1))
        except Exception:  # noqa: BLE001 — inaccessible child
            continue
    return False

def _x11_focus(title: str) -> str | None:
    """Tag an X11 editor with its actual keyboard focus, not stale AT-SPI."""
    if not title:
        return None
    try:
        xid = subprocess.run(["xdotool", "getwindowfocus"],
                             capture_output=True, text=True, timeout=0.5)
        if xid.returncode != 0 or not xid.stdout.strip():
            return None
        name = subprocess.run(
            ["xdotool", "getwindowname", xid.stdout.strip()],
            capture_output=True, text=True, timeout=0.5)
        if name.returncode == 0 and name.stdout.strip() == title:
            return xid.stdout.strip()
    except (OSError, subprocess.SubprocessError):
        pass
    return None

def focused_window() -> dict | None:
    if os.environ.get("XDG_SESSION_TYPE") == "wayland":
        return _wayland_focus()
    root = Atspi.get_desktop(0)
    if root is None:
        return None
    try:
        count = root.get_child_count()
    except Exception:  # noqa: BLE001
        return None
    for i in range(count):
        try:
            app = root.get_child_at_index(i)
        except Exception:  # noqa: BLE001
            continue
        if app is None:
            continue
        app_name = _safe_name(app)
        # Window-manager decorations are not an editable focus target.
        if app_name in ("gnome-shell", "mutter-x11-frames"):
            continue
        try:
            win_count = app.get_child_count()
        except Exception:  # noqa: BLE001
            continue
        for j in range(win_count):
            try:
                win = app.get_child_at_index(j)
            except Exception:  # noqa: BLE001
                continue
            if win is None:
                continue
            if _contains_focus(win):
                name = _safe_name(win)
                return {"app": app_name, "window": name,
                        "x11_focus": _x11_focus(name)}
    return None


def main() -> int:
    try:
        win = focused_window()
    except Exception:  # noqa: BLE001 — probe must never crash the daemon
        return 0
    if win and win["app"]:
        print(json.dumps(win, ensure_ascii=False), flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
