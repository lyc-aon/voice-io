"""Daemon-side JSONL client for native desktop overlay processes.

Linux runs GTK3 via system Python (venv lacks gi) under XWayland; macOS
runs the Cocoa overlay in the project interpreter. Neither steals editor
focus. The overlay process only handles visuals and sends control commands
to the daemon's FIFO.
"""

from __future__ import annotations

import json
import logging
import os
import shutil
import subprocess
import sys
import time
from pathlib import Path

log = logging.getLogger(__name__)

_GTK_SCRIPT = Path(__file__).resolve().parent / "overlay_gtk.py"


def _system_python() -> str | None:
    # venv python lacks gi; overlay must run under system python3.
    for cand in ("/usr/bin/python3",):
        if Path(cand).exists():
            return cand
    cand = shutil.which("python3")
    return cand if cand and Path(cand).resolve() != Path(sys.executable).resolve() else None


def _x11_auth_env() -> dict[str, str]:
    """Share the compositor's XWayland cookie with GTK and focus probes."""
    env = dict(os.environ)
    if not env.get("XAUTHORITY"):
        runtime = Path(env.get("XDG_RUNTIME_DIR", ""))
        if runtime.is_dir():
            cookie = next(runtime.glob(".mutter-Xwaylandauth.*"), None)
            if cookie is not None:
                env["XAUTHORITY"] = str(cookie)
    return env


class Overlay:
    def __init__(self, enabled: bool = True, dry_run: bool = False):
        self.enabled = enabled and not dry_run
        self.dry_run = dry_run
        self._proc: subprocess.Popen | None = None
        self._degraded = False

    def _spawn(self) -> bool:
        if self._degraded:
            return False
        env = dict(os.environ)
        if sys.platform == "darwin":
            argv = [sys.executable, "-m", "voice_io.overlay_mac"]
        elif sys.platform == "linux":
            py = _system_python()
            if py is None:
                log.error("GTK overlay requires system Python with gi/GTK3")
                self._degraded = True
                return False
            argv = [py, str(_GTK_SCRIPT)]
            env = _x11_auth_env()
            env["GDK_BACKEND"] = "x11"
            env["NO_AT_BRIDGE"] = "1"
        else:
            log.error("overlay unsupported on %s", sys.platform)
            self._degraded = True
            return False
        try:
            self._proc = subprocess.Popen(
                argv, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                stderr=None, env=env)
        except OSError as error:
            log.error("could not launch overlay: %s", error)
            self._proc = None
            self._degraded = True
            return False
        return True

    def show(self, text: str, style: str = "info") -> None:
        if self.dry_run:
            print(f"  [overlay:{style}] {text}", flush=True)
            return
        if not self.enabled:
            return
        self._send({"op": "show", "text": text, "style": style})

    def state(self, value: str) -> None:
        """Pill state: idle|listening|processing|off|error."""
        if self.dry_run:
            print(f"  [overlay:state] {value}", flush=True)
            return
        if not self.enabled:
            return
        self._send({"op": "state", "value": value})

    def session_open(self, language: str) -> None:
        self._send({"op": "session", "open": True,
                    "started_at": time.time(), "language": language})

    def session_close(self) -> None:
        self._send({"op": "session", "open": False})

    def preview(self, text: str) -> None:
        """Update optional adjacent transcript preview, never the waveform."""
        if self.dry_run:
            print(f"  [overlay:preview] {text}", flush=True)
            return
        if not self.enabled:
            return
        self._send({"op": "preview", "text": text})

    def ready(self, active: bool = True) -> None:
        """Pulse a small status dot; leave the idle recording pill hidden."""
        if self.enabled:
            self._send({"op": "ready", "active": active})

    def level(self, value: float) -> None:
        """Speech-gated raw mic RMS (0.0-1.0). 10x/s — no dry-run spam."""
        if self.dry_run or not self.enabled:
            return
        self._send({"op": "level", "value": max(0.0, min(1.0, float(value)))})


    def result(self, text: str, style: str = "ok", auto_close_s: float = 2.0,
               show_text: bool = False) -> None:
        """Finish the session; only display success copy when explicitly requested."""
        if self.dry_run:
            print(f"  [overlay:result] {text}", flush=True)
            return
        if not self.enabled:
            return
        self._send({"op": "result", "text": text, "style": style,
                    "auto_close_s": auto_close_s, "show_text": show_text})

    def clear(self) -> None:
        self._send({"op": "clear"})

    def _send(self, msg: dict) -> None:
        if self._proc is None or self._proc.poll() is not None:
            self._proc = None
            if not self._spawn():
                return
        try:
            assert self._proc.stdin is not None
            self._proc.stdin.write(
                (json.dumps(msg, ensure_ascii=False) + "\n").encode())
            self._proc.stdin.flush()
        except (OSError, BrokenPipeError):
            self._proc = None

    def close(self) -> None:
        if self._proc is None or self._proc.poll() is not None:
            self._proc = None
            return
        self._send({"op": "quit"})
        if self._proc is not None:
            try:
                self._proc.wait(timeout=2)
            except (subprocess.TimeoutExpired, OSError):
                self._proc.terminate()
        self._proc = None
