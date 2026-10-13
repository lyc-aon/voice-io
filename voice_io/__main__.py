"""CLI entry for live capture, settings, models, recovery, and shortcuts."""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from pathlib import Path

from voice_io.config import Config
from voice_io.control import (install_cancel_hotkey, install_hotkey,
                              install_toggle_hotkey, uninstall_hotkey)


def main() -> int:
    ap = argparse.ArgumentParser(prog="voice-io",
                                 description="Local voice dictation and meeting capture")
    ap.add_argument("--config", type=Path, default=None,
                    help="config TOML (default ~/.config/voice-io/config.toml)")
    ap.add_argument("--threads", type=int, default=None)
    sub = ap.add_subparsers(dest="mode", required=True)

    pl = sub.add_parser("live", help="live daemon loop (mic)")
    pl.add_argument("--duration", type=float, default=0.0,
                    help="stop after N seconds (0 = run forever)")
    pl.add_argument("--dry-run", action="store_true",
                    help="no real paste/cues/overlay")
    pl.add_argument("--record", type=Path, default=None,
                    help="one-shot: record the NEXT session's stream audio "
                         "to this wav (test instrumentation, never re-arms)")

    sub.add_parser("settings", help="edit shortcuts, wake phrases, and display")
    sub.add_parser("recover", help="copy latest saved transcript for manual paste")
    pm = sub.add_parser("models", help="install or verify offline model weights")
    pm.add_argument("action", choices=("install", "verify"))
    pm.add_argument("--meeting", action="store_true",
                    help="include optional offline diarization models with dictation models")

    ph = sub.add_parser("hotkey", help="hotkey registration")
    ph.add_argument("--register", action="store_true")
    ph.add_argument("--register-toggle", action="store_true")
    ph.add_argument("--register-cancel", action="store_true")
    ph.add_argument("--unregister", action="store_true")
    ph.add_argument("--binding", default=None)
    ph.add_argument("--toggle-binding", default=None)
    ph.add_argument("--cancel-binding", default=None)

    args = ap.parse_args()
    cfg = Config.load(args.config)
    if args.threads is not None:
        cfg.asr.threads = args.threads

    if args.mode == "settings":
        if sys.platform == "darwin":
            command = [sys.executable, "-m", "voice_io.overlay_mac",
                       "--settings"]
        else:
            command = ["/usr/bin/python3",
                       str(Path(__file__).with_name("overlay_gtk.py")),
                       "--settings"]
        env = ({**os.environ, "VOICE_IO_CONFIG": str(args.config.resolve())}
               if args.config is not None else None)
        return subprocess.call(command, env=env)
    if args.mode == "models":
        from voice_io.install_models import (ModelInstallError, install_models,
                                             verify_models)
        try:
            installed = (install_models(meeting=args.meeting) if args.action == "install"
                         else verify_models(meeting=args.meeting))
        except ModelInstallError as exc:
            print(f"models: {exc}", file=sys.stderr)
            return 1
        for name, path in installed.items():
            print(f"{name}: {path}")
        return 0
    if args.mode == "recover":
        from voice_io.executor import Executor
        from voice_io.recovery import RecoveryStore
        directory = (cfg.resolve(cfg.dictation.recovery_dir)
                     if cfg.dictation.recovery_dir else None)
        entry = RecoveryStore(directory, cfg.dictation.recovery_ttl).latest()
        text = entry.get("text") if entry else None
        if not isinstance(text, str) or not text.strip():
            print("No saved transcript to recover", file=sys.stderr)
            return 1
        result = Executor(dry_run=False).clipboard_only(text)
        if not result.ok:
            print(f"Could not copy saved transcript: {result.message}",
                  file=sys.stderr)
            return 1
        print("Transcript copied. Paste into the intended editor; "
              "the saved copy remains available until discarded or expired.")
        return 0
    if args.mode == "live":
        from voice_io.daemon import Daemon
        daemon = Daemon(cfg, dry_run=args.dry_run,
                        record_path=args.record)
        try:
            return daemon.run_live(args.duration)
        finally:
            daemon.close()
    if args.mode == "hotkey":
        if sys.platform == "darwin":
            print("macOS hotkeys run with the daemon; edit them in settings "
                  "and restart voice-io", file=sys.stderr)
            return 2
        if args.unregister:
            err = uninstall_hotkey()
        elif args.register_cancel:
            err = install_cancel_hotkey(args.cancel_binding or cfg.hotkeys.cancel)
        elif args.register_toggle:
            err = install_toggle_hotkey(args.toggle_binding or cfg.hotkeys.toggle)
        else:
            err = install_hotkey(args.binding or cfg.hotkeys.push)
        if err:
            print(f"hotkey: {err}")
            return 1
        print("hotkey ok")
        return 0
    return 2


if __name__ == "__main__":
    sys.exit(main())
