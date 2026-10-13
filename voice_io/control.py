"""Control FIFO for recording hotkeys and daemon status.

The daemon owns the FIFO; desktop shortcuts send `push`, `ptt`/`ptt_stop`,
`toggle`, or `cancel`. `push` starts/stops a session, `cancel` discards it,
and `status`/`quit` provide explicit local control.
"""

from __future__ import annotations

import ast
import json
import os
import subprocess
from pathlib import Path

from voice_io.config import HotkeyCfg, parse_hotkey


def fifo_path() -> Path:
    base = Path(os.environ.get("XDG_RUNTIME_DIR", Path.home() / ".config"))
    return base / "voice-io" / "control.fifo"


def write(msg: str) -> bool:
    """Fire-and-forget write from a hotkey; never block on a stale FIFO."""
    try:
        fd = os.open(fifo_path(), os.O_WRONLY | os.O_NONBLOCK)
        try:
            os.write(fd, (msg + "\n").encode("utf-8"))
        finally:
            os.close(fd)
        return True
    except OSError:
        return False


class ControlFifo:
    """Non-blocking reader for the daemon."""

    def __init__(self):
        self._path = fifo_path()
        self._fd: int | None = None
        self._buf = b""

    def open(self) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        os.chmod(self._path.parent, 0o700)
        if not self._path.exists():
            os.mkfifo(self._path, 0o600)
        self._fd = os.open(self._path, os.O_RDONLY | os.O_NONBLOCK | os.O_CLOEXEC)

    def read(self) -> list[str]:
        """Drain pending lines (empty when nothing available)."""
        if self._fd is None:
            return []
        msgs = []
        try:
            data = os.read(self._fd, 4096)
        except BlockingIOError:
            data = b""
        except OSError:
            return []
        if data:
            self._buf += data
            while b"\n" in self._buf:
                line, self._buf = self._buf.split(b"\n", 1)
                msg = line.decode("utf-8", "replace").strip()
                if msg:
                    msgs.append(msg)
        return msgs

    def close(self) -> None:
        if self._fd is not None:
            os.close(self._fd)
            self._fd = None


# ---- gsettings hotkey registration ----

_SCHEMA = "org.gnome.settings-daemon.plugins.media-keys"
_KEYS_PATH = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/voiceio-ptt/"


def _gsettings(*args: str) -> str:
    result = subprocess.run(["gsettings", *args], capture_output=True,
                            text=True, timeout=10, check=True)
    return result.stdout.strip()


_TOGGLE_KEYS_PATH = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/voiceio-toggle/"


_CANCEL_KEYS_PATH = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/voiceio-cancel/"
_OWNED = {_KEYS_PATH, _TOGGLE_KEYS_PATH, _CANCEL_KEYS_PATH}
_GNOME_MODS = {"ctrl": "Control", "alt": "Alt",
               "shift": "Shift", "super": "Super"}
_GNOME_KEYS = {"enter": "Return", "escape": "Escape", "pageup": "Page_Up",
               "pagedown": "Page_Down", "backspace": "BackSpace",
               "delete": "Delete", "tab": "Tab", "up": "Up", "down": "Down",
               "left": "Left", "right": "Right", "home": "Home", "end": "End",
               "insert": "Insert"}


def to_gnome_binding(binding: str) -> str:
    """Translate portable ctrl+alt+space to GNOME's <Control><Alt>space."""
    mods, key = parse_hotkey(binding)
    gnome_key = key.upper() if key.startswith("f") and key[1:].isdigit() else _GNOME_KEYS.get(key, key)
    return "".join(f"<{_GNOME_MODS[mod]}>" for mod in
                   ("ctrl", "alt", "shift", "super") if mod in mods) + gnome_key


def _paths() -> list[str]:
    raw = _gsettings("get", _SCHEMA, "custom-keybindings")
    if raw.startswith("@as "):
        raw = raw[4:]
    paths = ast.literal_eval(raw)
    if not isinstance(paths, list) or not all(isinstance(p, str) for p in paths):
        raise ValueError("Unexpected GNOME custom-keybindings value")
    return paths


def _set_keybinding(path: str, name: str, binding: str, command: str) -> None:
    """Update only this voice-io-owned entry and preserve every other entry."""
    if path not in _OWNED:
        raise ValueError("Refusing to modify non-voice-io GNOME shortcut")
    paths = _paths()
    schema = f"{_SCHEMA}.custom-keybinding:{path}"
    # Write the shortcut before adding its path so GNOME never activates an
    # incomplete entry. An existing path is changed only on explicit apply.
    _gsettings("set", schema, "name", name)
    _gsettings("set", schema, "binding", to_gnome_binding(binding))
    _gsettings("set", schema, "command", command)
    if path not in paths:
        _gsettings("set", _SCHEMA, "custom-keybindings",
                   json.dumps([*paths, path]))


def _install(path: str, name: str, binding: str, action: str) -> str | None:
    try:
        _set_keybinding(path, name, binding,
                        f"bash -c 'echo {action} > {fifo_path()}'")
    except (OSError, subprocess.SubprocessError, ValueError, SyntaxError) as e:
        return str(e)
    return None


def install_hotkey(binding: str = "ctrl+alt+space") -> str | None:
    """Explicitly install/update voice-io's push shortcut only."""
    return _install(_KEYS_PATH, "Voice push-to-talk", binding, "push")


def install_toggle_hotkey(binding: str = "ctrl+alt+t") -> str | None:
    """Explicitly install/update voice-io's mute shortcut only."""
    return _install(_TOGGLE_KEYS_PATH, "Voice on/off", binding, "toggle")


def install_cancel_hotkey(binding: str = "ctrl+alt+x") -> str | None:
    """Explicitly install/update voice-io's cancel shortcut only."""
    return _install(_CANCEL_KEYS_PATH, "Voice cancel", binding, "cancel")


def apply_hotkeys(hotkeys: HotkeyCfg) -> str | None:
    """Apply voice-io's three shortcuts after an explicit user action."""
    for install, binding in ((install_hotkey, hotkeys.push),
                             (install_toggle_hotkey, hotkeys.toggle),
                             (install_cancel_hotkey, hotkeys.cancel)):
        error = install(binding)
        if error:
            return error
    return None


def uninstall_hotkey() -> str | None:
    """Unregister only voice-io paths; leave unrelated GNOME bindings intact."""
    try:
        paths = _paths()
        retained = [p for p in paths if p not in _OWNED]
        if retained != paths:
            _gsettings("set", _SCHEMA, "custom-keybindings", json.dumps(retained))
    except (OSError, subprocess.SubprocessError, ValueError, SyntaxError) as e:
        return str(e)
    return None
