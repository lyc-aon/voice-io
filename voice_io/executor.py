"""Focused clipboard insertion and recovery-safe text copying."""

from __future__ import annotations

import os
import subprocess
import time
import sys
from collections.abc import Callable
from dataclasses import dataclass

# Handy's Linux paste timings: delay after clipboard write (before the chord)
# and after the chord (before restoring the saved clipboard). A too-short
# after-delay loses the race (#502: the target reads the clipboard after
# restore and pastes stale content back).
_PASTE_DELAY_MS = 60
_PASTE_DELAY_AFTER_MS = 60

# Sentinel: the clipboard probe failed (no owner responded) — restore must
# not touch the clipboard.
_UNKNOWN = object()

@dataclass
class Result:
    ok: bool
    message: str = ""
    executed: bool = False


class FocusChanged(RuntimeError):
    """The editor moved before clipboard text could be inserted."""


class Executor:
    def __init__(self, dry_run: bool = True, auto_submit: str = "",
                 mac_io=None):
        self.dry_run = dry_run
        self.auto_submit = auto_submit
        # Clipboard capability is retried per dictation: an intermittent
        # compositor failure must not disable future pastes for the session.
        self._mac_io = mac_io

    def _mac(self):
        if self._mac_io is None:
            from voice_io.mac_focus import MacTextIO
            self._mac_io = MacTextIO()
        return self._mac_io

    # ---- dictation (voice = keyboard) ----

    def dictate(self, text: str, chord: str = "ctrl+v",
                submit: bool | None = False, expected_focus: str | None = None,
                focus_probe: Callable[[], str | None] | None = None) -> Result:
        """Insert at the current cursor, checking known focus before paste.

        None suppresses both the spoken-send Enter and configured auto-submit
        when Wayland cannot verify the receiving window.
        """
        if not text.strip():
            return Result(ok=False, message="empty transcript")
        if self.dry_run:
            return Result(ok=True,
                          message=f"[dry-run] would insert: {text}",
                          executed=False)
        if sys.platform == "darwin":
            try:
                mac = self._mac()
                mac.paste(text, expected_focus=expected_focus)
                if expected_focus is not None and mac.focus() != expected_focus:
                    return Result(ok=False, message="focus changed after paste; submit suppressed",
                                  executed=True)
                if submit is not None:
                    self._after_paste(submit)
            except Exception as exc:
                return Result(ok=False, message=f"insert failed: {exc}")
            return Result(ok=True, message=f"inserted: {text}", executed=True)
        try:
            self._clipboard_paste(text, chord, expected_focus, focus_probe)
            if submit is not None:
                if (expected_focus is not None and focus_probe is not None
                        and focus_probe() != expected_focus):
                    return Result(ok=True, message="pasted; submit suppressed after focus change",
                                  executed=True)
                self._after_paste(submit)
            return Result(ok=True, message=f"paste sent: {text}", executed=True)
        except FocusChanged:
            return Result(ok=False, message="focus changed before paste; "
                          "transcript copied to clipboard")
        except Exception as exc:  # noqa: BLE001 — never type into an unverified target
            return Result(ok=False, message=f"paste failed: {exc}; "
                          "transcript saved for recovery")

    def _after_paste(self, submit: bool) -> None:
        if submit:
            # The configured end phrase confirms dictation; press Enter to
            # send the message.
            if sys.platform == "darwin":
                self._mac().key("enter")
            else:
                self._send_key("enter")
        else:
            self._submit()

    def _submit(self) -> None:
        """Post-insert submit key (Handy's auto-submit). Best-effort."""
        if not self.auto_submit:
            return
        keys = {"enter": "enter", "ctrl+enter": "ctrl+enter"}
        chord = keys.get(self.auto_submit)
        if chord is None:
            return
        if sys.platform == "darwin":
            self._mac().key(chord)
        else:
            self._send_key(chord)

    @staticmethod
    def _send_key(chord: str) -> bool:
        """Only inject through senders that can reach the active desktop.

        Xdotool targets Xwayland's last focused window, which may not be the
        native Wayland editor under the user's current typing cursor.
        """
        wayland = os.environ.get("XDG_SESSION_TYPE") == "wayland"
        senders = [["ydotool", "key", chord]]
        if not wayland:
            senders.append(["xdotool", "key", chord])
        else:
            parts = chord.split("+")
            wtype = ["wtype"]
            for modifier in parts[:-1]:
                wtype.extend(["-M", modifier])
            wtype.extend(["-k", parts[-1]])
            for modifier in reversed(parts[:-1]):
                wtype.extend(["-m", modifier])
            senders.append(wtype)
        for argv in senders:
            try:
                subprocess.run(argv, check=True, timeout=3)
                return True
            except (OSError, subprocess.SubprocessError,
                    subprocess.TimeoutExpired):
                continue
        return False


    def clipboard_only(self, text: str) -> Result:
        """Focus drifted: verify the transcript is available for manual paste."""
        if not text.strip():
            return Result(ok=False, message="empty transcript")
        if self.dry_run:
            return Result(ok=True, message=f"[dry-run] would copy: {text}",
                          executed=False)
        if sys.platform == "darwin":
            try:
                self._mac().copy(text)
            except Exception as exc:
                return Result(ok=False, message=f"clipboard failed: {exc}")
            return Result(ok=True, message=f"copied: {text}", executed=True)
        try:
            self._spawn_copy(text.encode("utf-8"), "text/plain")
            time.sleep(_PASTE_DELAY_MS / 1000)
            if not self._verify_serve(text):
                raise RuntimeError("clipboard serve failed")
        except Exception as exc:  # noqa: BLE001
            return Result(ok=False, message=f"clipboard failed: {exc}")
        return Result(ok=True, message="transcript copied to clipboard",
                      executed=True)

    # ---- clipboard save/paste/restore (Handy's reliable-paste pattern) ----

    def _snapshot_clipboard(self) -> tuple[str, bytes] | None | object:
        """('text', data) | ('image', data) | None | UNKNOWN.

        None = nothing on the clipboard (restore clears). UNKNOWN = the probe
        failed (no owner responded) — restore must NOT clear: the user may
        have clipboard content we couldn't read.
        """
        try:
            types = subprocess.run(["wl-paste", "--list-types"],
                                   capture_output=True, text=True, timeout=1)
        except (OSError, subprocess.TimeoutExpired):
            return _UNKNOWN
        if types.returncode != 0:
            return _UNKNOWN
        listed = [t for t in types.stdout.split("\n") if t]
        if any("text" in t for t in listed):
            try:
                data = subprocess.run(["wl-paste"], capture_output=True,
                                      timeout=1)
            except (OSError, subprocess.TimeoutExpired):
                return _UNKNOWN
            if data.returncode == 0:
                return ("text", data.stdout)
        if any("image" in t for t in listed):
            try:
                data = subprocess.run(["wl-paste", "-t", "image/png"],
                                      capture_output=True, timeout=1)
            except (OSError, subprocess.TimeoutExpired):
                return _UNKNOWN
            if data.returncode == 0 and data.stdout:
                return ("image", data.stdout)
        return None

    def _restore_clipboard(self, snap: tuple[str, bytes] | None | object) -> None:
        try:
            if snap is None:
                subprocess.run(["wl-copy", "--clear"], timeout=3)
            elif snap is not _UNKNOWN and snap[0] == "text":
                self._spawn_copy(snap[1], "text/plain")
            elif snap is not _UNKNOWN:
                self._spawn_copy(snap[1], "image/png")
            # _UNKNOWN: probe failed — leave the transcript on the clipboard
            # rather than risk clearing the user's content.
        except Exception:  # noqa: BLE001 — restore is best-effort
            pass

    def _verify_serve(self, text: str) -> bool:
        """Read the clipboard back — proves the new content is actually being
        served before we fire the paste chord. Without this, a broken
        clipboard path silently pastes stale content."""
        try:
            r = subprocess.run(["wl-paste", "--no-newline"],
                               capture_output=True, text=True, timeout=1)
        except (OSError, subprocess.TimeoutExpired):
            return False
        return r.returncode == 0 and r.stdout == text

    @staticmethod
    def _spawn_copy(data: bytes, mime: str) -> None:
        """Fire-and-forget clipboard write. wl-copy does not return while it
        owns the selection (it holds it in the foreground) — the running
        process IS the clipboard owner on Wayland, so we never wait."""
        p = subprocess.Popen(["wl-copy", "--type", mime],
                             stdin=subprocess.PIPE,
                             start_new_session=True)
        assert p.stdin is not None
        p.stdin.write(data)
        p.stdin.close()

    def _clipboard_paste(
            self, text: str, chord: str = "ctrl+v",
            expected_focus: str | None = None,
            focus_probe: Callable[[], str | None] | None = None) -> None:
        snap = self._snapshot_clipboard()
        pasted = False
        try:
            self._spawn_copy(text.encode("utf-8"), "text/plain")
            time.sleep(_PASTE_DELAY_MS / 1000)
            if not self._verify_serve(text):
                raise RuntimeError("clipboard serve failed")
            if (expected_focus is not None and focus_probe is not None
                    and focus_probe() != expected_focus):
                raise FocusChanged()
            if not self._send_key(chord):
                raise RuntimeError("no chord sender available")
            time.sleep(_PASTE_DELAY_AFTER_MS / 1000)
            pasted = True
        finally:
            # On failure, keep the transcript available for manual paste.
            # After success, do not overwrite a concurrent clipboard change.
            if pasted and self._verify_serve(text):
                self._restore_clipboard(snap)

