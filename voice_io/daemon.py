"""Daemon state machine for live dictation, meeting, and collaborative capture.

The mic feeds VAD and wake/end KWS. Streaming ASR supplies tentative previews;
each completed dictation turn is re-decoded offline and saved as a recoverable
draft. Closing the session cleans, optionally polishes, and pastes once into
the verified focused editor. Meeting and collaborative capture are diarized.
"""

from __future__ import annotations

import json
import os
import queue
import subprocess
import sys
import tempfile
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import numpy as np

from voice_io.asr import StreamingASR
from voice_io.audio import MicSource, SR, read_wav, write_wav
from voice_io.config import Config
from voice_io.control import ControlFifo
from voice_io.cues import CuePlayer
from voice_io.executor import Executor, Result
from voice_io.kws import KWS
from voice_io.llm import LLM
from voice_io.meeting import (MeetingCancelled, MeetingProcessor, MeetingRecorder,
                              collaborative_text)
from voice_io.overlay import Overlay, _system_python, _x11_auth_env
from voice_io.polish import (cleanup, load_map, polish_guard, polish_prompt,
                             snippet_match)
from voice_io.recovery import RecoveryStore
from voice_io.refine import TurnRefiner
from voice_io.turn import Turn
from voice_io.vad import VAD

IDLE = "IDLE"
LISTENING = "LISTENING"
PARSING = "PARSING"


_FOCUS_PROBE = Path(__file__).resolve().parent / "focus_probe.py"


def _probe_focus() -> str | None:
    """Identity of the focused target, without focusing the overlay."""
    if sys.platform == "darwin":
        from voice_io.mac_focus import focused_window
        return focused_window()
    # Native Wayland clients need a compositor-backed window identity; a
    # missing bridge fails closed instead of pasting at an unverified cursor.
    python = _system_python()
    if python is None:
        return None
    try:
        r = subprocess.run([python, str(_FOCUS_PROBE)],
                           capture_output=True, text=True, timeout=3,
                           env=_x11_auth_env())
    except (OSError, subprocess.SubprocessError):
        return None
    try:
        data = json.loads(r.stdout.strip()) if r.stdout.strip() else None
    except ValueError:
        return None
    if data and data.get("app") and data.get("window"):
        window = str(data["window"])
        if window.startswith("gnome:"):
            return f"{data['app']}|{window}"
        if data.get("x11_focus"):
            return f"{data['app']}|{window}|x11:{data['x11_focus']}"
    return None



def _strip_end_phrase(text: str, phrase: str) -> str:
    """Remove the configured end phrase only as a complete trailing phrase."""
    trailing = text.rstrip().rstrip(".,!?").rstrip()
    start = len(trailing) - len(phrase)
    if (start >= 0 and trailing[start:].casefold() == phrase.casefold()
            and (start == 0 or not trailing[start - 1].isalpha())):
        return trailing[:start].rstrip(" ,.!?")
    return text.strip()


def _preview_tail(text: str) -> str:
    """Show the latest tentative words, not the entire running transcript."""
    return " ".join(text.split()[-3:])

# Use the terminal-specific shortcut when compositor focus is known.
_TERMINAL_APPS = ("gnome-terminal", "ghostty", "kitty", "alacritty", "foot",
                  "wezterm", "tilix", "xterm", "konsole", "ptyxis")

def _paste_chord(focus: str | None) -> str:
    """Use the current app's paste binding, or the common Linux fallback."""
    if sys.platform == "darwin":
        return "cmd+v"
    if focus is None:
        return "shift+Insert"
    app = focus.split("|", 1)[0].lower().replace(" ", "-")
    if app == "tern" or any(t in app for t in _TERMINAL_APPS):
        return "ctrl+shift+v"
    return "ctrl+v"


class Daemon:
    def __init__(self, cfg: Config, dry_run: bool = True,
                 record_path: Path | None = None):
        self.cfg = cfg
        # One-shot session recorder (test instrumentation): records the NEXT
        # session's stream audio (wake -> close) to record_path, then
        # self-disables — no wavs pile up after the test.
        self._record_path = record_path
        self._rec_buf: list[np.ndarray] | None = None
        self._record_done = False
        self.vad = VAD(cfg.resolve(cfg.vad.model), cfg.vad.threshold,
                       cfg.vad.min_silence_duration,
                       cfg.vad.max_speech_duration)
        self.kws = KWS(cfg.resolve(cfg.kws.model_dir),
                       cfg.kws.wake_phrase, cfg.kws.end_phrase,
                       chunk=cfg.kws.chunk, score=cfg.kws.score,
                       threshold=cfg.kws.threshold, threads=cfg.asr.threads)
        self.asr = StreamingASR(cfg.resolve(cfg.asr.streaming),
                                cfg.asr.threads)
        self.llm = LLM(cfg)
        self.executor = Executor(dry_run, auto_submit=cfg.exec.auto_submit)
        self.overlay = Overlay(cfg.overlay_enabled, dry_run)
        self.cues = CuePlayer(dry_run, enabled=cfg.ui.sounds)
        self.control = ControlFifo()
        # Refinements run in capture order while the microphone stays live.
        # The final paste is queued only after every turn has been refined.
        self._dictation_pool = ThreadPoolExecutor(max_workers=1)
        self._session_lock = threading.Lock()
        self._session_token = 0
        self._session_parts: list[str] = []
        self._draft_ts: float | None = None
        self._draft_storage_warned = False
        self._dictation_count = 0
        self._audio_spill: tempfile.TemporaryDirectory[str] | None = None
        self._spill_readers: set[Path] = set()
        self._retired_spills: list[tempfile.TemporaryDirectory[str]] = []
        self._paste_q: queue.Queue[tuple | None] = queue.Queue()
        self._meeting_recorder: MeetingRecorder | None = None
        self._meeting_processing_path: Path | None = None
        self._meeting_processor: MeetingProcessor | None = None
        self._paste_thread = threading.Thread(
            target=self._paste_loop, daemon=True, name="voice-io-paste")
        self._paste_thread.start()
        self.state = IDLE
        self.enabled = True
        self.language = "en"
        self._preroll: list[np.ndarray] = []
        self._wake_kw: str | None = None
        self._running = True
        # Session mode: "continuous" (wake-word, multiple commits per
        # session) or "ptt" (one-shot: single commit at key release).
        self._mode = "continuous"
        # Turn authority: ONE utterance per turn — the VAD endpoint commits
        # it, the stream opens at the first confirmed speech after the
        # trigger, and the next turn opens fresh. Single endpoint authority:
        # no cadence clock, no idle close.
        self._turn = Turn()
        self._stream_open = False
        self._last_preview = ""
        self._session_t0 = time.monotonic()
        self._noise_floor = 0.0
        # English full-turn re-decode after streaming preview.
        self._refiner = TurnRefiner(cfg)
        # ASR resilience: consecutive decode failures -> offline fallback.
        self._asr_failures = 0
        self._degraded = False
        # Custom vocabulary + snippets (deterministic, never in the LLM
        # prompt). Empty maps when the config paths are unset/missing.
        self._words = load_map(
            self.cfg.resolve(self.cfg.polish.custom_words)
            if self.cfg.polish.custom_words else None)
        self._snippets = load_map(
            self.cfg.resolve(self.cfg.polish.snippets)
            if self.cfg.polish.snippets else None)
        rec_dir = (self.cfg.resolve(self.cfg.dictation.recovery_dir)
                   if self.cfg.dictation.recovery_dir else None)
        self.recovery = RecoveryStore(rec_dir,
                                      self.cfg.dictation.recovery_ttl)
        hist = (self.cfg.resolve(self.cfg.dictation.history)
                if self.cfg.dictation.history
                else Path.home() / ".local" / "share" / "voice-io"
                / "history.jsonl")
        self.history_path: Path | None = hist
        try:
            hist.parent.mkdir(parents=True, exist_ok=True)
            if hist.is_symlink():
                raise OSError("History file must not be a symlink")
            if hist.exists():
                hist.chmod(0o600)  # migrate recordings from older installs
        except OSError:
            self.history_path = None  # unreadable storage: history off

    # ---- live loop ----

    def _wake(self, keyword: str | None = None) -> None:
        if self.cfg.meeting.mode in ("meeting", "collaborative"):
            try:
                self._meeting_processor = MeetingProcessor(self.cfg, self._refiner)
                self._meeting_processor._model_paths()
                self._meeting_recorder = MeetingRecorder()
            except FileNotFoundError as error:
                print(f"{self.cfg.meeting.mode} unavailable ({error})", flush=True)
                self.overlay.result("Install speaker models", "error",
                                    auto_close_s=3.0)
                return
            except OSError as error:
                print(f"{self.cfg.meeting.mode} storage unavailable ({error})",
                      flush=True)
                self.overlay.result("Audio storage unavailable", "error",
                                    auto_close_s=3.0)
                return
        with self._session_lock:
            self._session_token += 1
            self._session_parts.clear()
            self._draft_ts = None
            self._draft_storage_warned = False
            self._cleanup_audio_spill()
            self._dictation_count = 0
        self._wake_kw = keyword
        # The wake word's own audio is a VAD segment - discard it, else it
        # ends the session immediately (trigger vs content separation).
        self.vad.pop_segments()
        self.overlay.session_open(self.language)
        self.overlay.state("listening")
        if self.cfg.meeting.mode == "meeting":
            self.overlay.show("Meeting recording", "info")
        elif self.cfg.meeting.mode == "collaborative":
            self.overlay.show("Collaborative recording", "info")
        self.cues.play("listening")
        self._mode = "continuous"
        # Turn authority: the turn opens EMPTY at the trigger — the pre-
        # trigger ring (wake audio) is never transcribed. The ASR stream
        # opens at the first confirmed speech after the trigger (VAD gate);
        # the buffered audio (post-trigger tail + gap + onset) seeds it.
        self._turn.open()
        self._stream_open = False
        self._last_preview = ""
        self._session_t0 = time.monotonic()
        self._session_warned = False
        self._noise_floor = 0.0
        # One-shot recorder: start at the wake; the buffer seeds with the
        # preroll so the wav matches the session's audio exactly.
        if (self._record_path is not None and not self._record_done
                and self._rec_buf is None):
            self._rec_buf = list(self._preroll)
        self.state = LISTENING

    def _listen(self, samples: np.ndarray) -> None:
        if (self.cfg.meeting.mode in ("meeting", "collaborative")
                and self._meeting_recorder is not None):
            try:
                self._meeting_recorder.append(samples)
            except OSError as error:
                print(f"meeting recording interrupted ({error})", flush=True)
                self.overlay.show("Audio storage failed — saving captured portion",
                                  "warn")
                self._finish_session()
                return
        if self._rec_buf is not None:
            self._rec_buf.append(samples)
        speech = self.vad.is_speech()
        rms = float(np.sqrt(np.dot(samples, samples) / max(1, len(samples))))
        if not speech and not self._stream_open:
            self._noise_floor += 0.08 * (rms - self._noise_floor)
        # The same PCM feeds ASR; subtract the observed room floor but keep
        # showing quiet syllables while an open stream waits for a VAD endpoint.
        level = max(0.0, rms - 1.3 * self._noise_floor)
        self.overlay.level(level if speech or self._stream_open else 0.0)
        self._turn.add(samples)
        if not self._stream_open and not speech:
            self._turn.retain_last(0.6)
        if not self._stream_open:
            # VAD-gated stream open: the stream starts at the first
            # confirmed speech AFTER the trigger — wake audio never enters.
            # The buffered onset (post-trigger tail + gap + first words)
            # seeds it once; the front drop cuts the wake-tail overlap.
            if self._turn.is_open and speech:
                self.asr.new_stream()
                self._stream_open = True
                seed = self._turn.audio
                # Only a wake-triggered turn can contain wake-tail overlap.
                drop = (int(self.cfg.turn.front_drop * SR)
                        if self._wake_kw else 0)
                if seed is not None and len(seed) > drop:
                    try:
                        partial = self.asr.accept(seed[drop:])
                    except Exception as e:  # noqa: BLE001
                        self._asr_failures += 1
                        self.asr.recover()
                        self._keep_listening()
                        print(f"    ASR accept failed ({e}); recovered "
                              f"({self._asr_failures})", flush=True)
                        self.overlay.show("ASR error — recovered", "warn")
                        return
                else:
                    partial = ""
            else:
                if self.vad.pop_segments():
                    self._keep_listening()  # blip: discard its buffered audio
                return  # no speech yet: nothing to decode
        else:
            try:
                partial = self.asr.accept(samples)
            except Exception as e:  # noqa: BLE001 — decode failure -> recover
                self._asr_failures += 1
                self.asr.recover()
                self._keep_listening()
                print(f"    ASR accept failed ({e}); recovered "
                      f"({self._asr_failures})", flush=True)
                self.overlay.show("ASR error — recovered", "warn")
                return
        if partial and self.cfg.ui.live_preview and partial != self._last_preview:
            self._last_preview = partial
            self.overlay.preview(_preview_tail(partial))
        # Turn authority: the VAD endpoint (0.5s silence), the engine
        # endpoint (rule2 1.2s trailing silence), or the 20s cap commit the
        # turn. No cadence clock, no idle close.
        segs = self.vad.pop_segments()
        if segs and any(seg.duration >= 0.5 for seg in segs):
            print(f"    segpop dur={segs[-1].duration:.2f}s", flush=True)
            self._commit_turn()
        elif segs:
            self._keep_listening()  # blip: do not seed the next turn with it
        elif self._turn.is_open and self._turn.seconds >= self.cfg.turn.max_sec:
            print(f"    turn cap {self._turn.seconds:.1f}s", flush=True)
            self._commit_turn()
        elif self._stream_open and self.asr.is_endpoint():
            print("    engine endpoint", flush=True)
            self._commit_turn()

    def _stop_recording(self) -> None:
        """Close the one-shot session recording (write the wav, then stop).
        Idempotent; the recorder never re-arms after a session closes."""
        if self._rec_buf is None:
            return
        buf = self._rec_buf
        self._rec_buf = None
        self._record_done = True
        if self._record_path is None or not buf:
            return
        try:
            audio = np.concatenate(buf)
            write_wav(self._record_path, audio)
            print(f"recorded: {self._record_path} ({len(audio)/SR:.1f}s)",
                  flush=True)
        except Exception as e:  # noqa: BLE001 — test instrumentation
            print(f"record failed: {e}", flush=True)

    def _cancel_capture(self) -> None:
        """Discard the active session without saving deliberately cancelled speech."""
        recorder = getattr(self, "_meeting_recorder", None)
        self._meeting_recorder = None
        if recorder is not None:
            try:
                recorder.discard()
            except OSError as error:
                print(f"meeting discard failed ({error})", flush=True)
        path = getattr(self, "_meeting_processing_path", None)
        self._meeting_processing_path = None
        if path is not None:
            path.unlink(missing_ok=True)
        with self._session_lock:
            self._session_token += 1
            self._session_parts.clear()
            if self._draft_ts is not None:
                self.recovery.drop(self._draft_ts)
                self._draft_ts = None
            self._cleanup_audio_spill()
            self._dictation_count = 0
        self.overlay.clear()
        self._turn.close()
        self._stream_open = False
        self._stop_recording()
        self.overlay.session_close()
        self.overlay.state("idle")
        self.state = IDLE

    def _commit_turn(self, close_session: bool = False,
                     submit: bool = False) -> None:
        """End a VAD/engine-ended turn or close the current session.

        Streaming hypotheses are preview-only. Refine each turn in capture
        order and paste the cleaned session once on close.
        """
        if not self._turn.is_open:
            if close_session:
                self._after_turn(True, submit=submit)
            return
        # The open turn may contain only post-endpoint silence. No stream
        # means VAD has not confirmed any speech in it; finalizing the old
        # ASR stream here would paste the previous turn a second time.
        if not self._stream_open:
            if close_session:
                self._after_turn(True, submit=submit)
            else:
                self._keep_listening()
            return
        audio = self._turn.audio
        self._turn.close()
        self._stream_open = False
        self.state = PARSING
        if close_session:
            self.overlay.state("processing")
        if audio is None or len(audio) / SR < 0.5:
            if close_session:
                self._after_turn(True, submit=submit)
                return
            self._keep_listening()  # blip: ignore, keep listening
            return
        if self.cfg.meeting.mode in ("meeting", "collaborative"):
            self._wake_kw = None
            self._after_turn(close_session)
            return
        # Flush the streaming tail for a usable fallback if offline re-decode
        # fails; the streaming hypothesis itself is only a preview.
        try:
            final = self.asr.finalize(tail_s=self.cfg.turn.tail_s)
        except Exception as e:  # noqa: BLE001 — decode failure -> fallback
            self._asr_failures += 1
            self.asr.recover()
            if not self._degraded:
                self._degraded = True
                self.overlay.show("degraded: offline ASR", "warn")
            print(f"    ASR decode failed ({e}); recovered "
                  f"({self._asr_failures})", flush=True)
            final = ""
        # The wake audio precedes content, but a short overlap can survive
        # the front drop. Strip only the configured complete wake phrase.
        kw = self._wake_kw
        self._wake_kw = None
        wake_phrase = self.cfg.kws.wake_phrase
        if kw == "WAKE" and final.casefold().startswith(wake_phrase.casefold()):
            final = final[len(wake_phrase):].lstrip(" ,.!?")
        if close_session:
            final = _strip_end_phrase(final, self.cfg.kws.end_phrase)
        final = final.strip()
        print(f"turn capture={len(audio)/SR:.1f}s close={close_session} "
              f"final={final!r}", flush=True)
        ev = {"text": final, "refine": ""}
        with self._session_lock:
            self._dictation_count += 1
            token = self._session_token
        # The executor queue must not retain a session's worth of PCM when
        # offline ASR is slower than capture. Write one turn to a private local
        # directory before queuing it; the worker maps it on demand in order.
        source, spill_failed = self._spill_turn_audio(audio)
        self._dictation_pool.submit(
            self._dictation_worker, source, final, close_session,
            ev, token, kw == "WAKE")
        if spill_failed:
            self.overlay.show("Audio storage unavailable — finishing dictation",
                              "warn")
        self._after_turn(close_session or spill_failed, audio, submit=submit)

    def _cleanup_audio_spill(self) -> None:
        """Called under the session lock; let active mmap readers finish."""
        spill = getattr(self, "_audio_spill", None)
        self._audio_spill = None
        if spill is not None:
            if self._spill_readers:
                self._retired_spills.append(spill)
            else:
                spill.cleanup()

    def _spill_turn_audio(
            self, audio: np.ndarray) -> tuple[Path | np.ndarray, bool]:
        """Bound queued PCM to private local disk without waiting for ASR."""
        try:
            if self._audio_spill is None:
                self._audio_spill = tempfile.TemporaryDirectory(
                    prefix="voice-io-pcm-", dir=self.recovery.dir)
            path = (Path(self._audio_spill.name)
                    / f"{self._dictation_count:08d}.npy")
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "wb") as output:
                np.save(output, audio, allow_pickle=False)
            return path, False
        except OSError as error:
            print(f"    audio spill failed ({error}); finishing session",
                  flush=True)
            # Keep the one current turn in memory, never discard it, but end
            # capture so a storage failure cannot grow an unbounded RAM queue.
            return audio, True

    def _dictation_worker(self, audio: np.ndarray | Path, final: str,
                          close_session: bool, ev: dict,
                          token: int, strip_wake: bool) -> None:
        """Refine a turn off the mic thread; never paste an unfinished session."""
        path = audio if isinstance(audio, Path) else None
        try:
            with self._session_lock:
                if self._session_token != token:
                    return
                # Open the mapping while cancellation cannot remove the
                # directory. Once open, keep its directory until ASR returns.
                samples = (np.load(path, mmap_mode="r", allow_pickle=False)
                           if path is not None else audio)
                if path is not None:
                    self._spill_readers.add(path)
            text = self._refiner.transcribe(samples)
        except Exception as e:  # noqa: BLE001 — fall back to streaming final
            self._asr_failures += 1
            text = final
            print(f"    refine failed ({e}); streaming final fallback",
                  flush=True)
            if path is not None and isinstance(e, OSError):
                self.overlay.show("Audio unavailable — using live transcript",
                                  "warn")
        finally:
            if path is not None:
                with self._session_lock:
                    self._spill_readers.discard(path)
                    path.unlink(missing_ok=True)
                    if not self._spill_readers:
                        for spill in self._retired_spills:
                            spill.cleanup()
                        self._retired_spills.clear()

        if strip_wake:
            phrase = self.cfg.kws.wake_phrase
            if text.casefold().startswith(phrase.casefold()):
                text = text[len(phrase):].lstrip(" ,.!?")
        if close_session:
            text = _strip_end_phrase(text, self.cfg.kws.end_phrase)
        if text.strip() and text != final:
            ev["refine"] = text
        with self._session_lock:
            if self._session_token != token:
                return
            if text.strip():
                self._session_parts.append(text.strip())
                try:
                    stamp = self.recovery.spool({
                        "kind": "draft", "text": " ".join(self._session_parts),
                        "mode": self._mode})
                except OSError as error:
                    print(f"    live draft recovery unavailable ({error})",
                          flush=True)
                    if not self._draft_storage_warned:
                        self._draft_storage_warned = True
                        self.overlay.show("Draft recovery unavailable", "warn")
                else:
                    if self._draft_ts is not None:
                        self.recovery.drop(self._draft_ts)
                    self._draft_ts = stamp

    def _finish_dictation_worker(self, token: int, ev: dict,
                                 audio: np.ndarray, mode: str,
                                 submit: bool) -> None:
        """Join ordered refinements; clean once, polish once, insert once."""
        with self._session_lock:
            if self._session_token != token:
                return
            text = " ".join(self._session_parts).strip()
        snip = snippet_match(text, self._snippets)
        if snip:
            text = snip
        elif text and self.cfg.polish.deterministic:
            text = cleanup(text, self._words, self.cfg.polish.filler,
                           self.cfg.polish.emoji)
        if text and not snip:
            text = self._polish_text(text)
        with self._session_lock:
            if self._session_token != token:
                return
            if text:
                try:
                    ts = self.recovery.spool({"kind": "commit", "text": text,
                                              "mode": mode})
                except OSError as error:
                    print(f"    recovery storage unavailable ({error})", flush=True)
                    ts = None
                    self.overlay.show("Recovery unavailable — finishing paste",
                                      "warn")
                if ts is not None and self._draft_ts is not None:
                    self.recovery.drop(self._draft_ts)
                    self._draft_ts = None
                self._paste_q.put((text, ev, submit, ts, audio, mode, token))
                return
        ev["inserted"] = False
        ev["control"] = "Didn't understand"
        self.overlay.result("Didn't understand", "error", auto_close_s=2.0)
        self._finish_ui(token)
        self._print_outcome(ev)


    def _after_turn(self, close_session: bool,
                    audio: np.ndarray | None = None,
                    submit: bool = False) -> None:
        if close_session:
            self._finish_session(audio, submit=submit)
        else:
            self._keep_listening()

    def _finish_session(self, audio: np.ndarray | None = None,
                        submit: bool = False) -> None:
        if self.cfg.meeting.mode in ("meeting", "collaborative"):
            self._finish_meeting_session()
            return
        if not self._dictation_count:
            self._cancel_capture()
            return
        self._turn.close()
        self._stop_recording()
        self.state = PARSING
        self.overlay.state("processing")
        with self._session_lock:
            token = self._session_token
        self._dictation_pool.submit(
            self._finish_dictation_worker, token, {}, audio if audio is not None
            else np.empty(0, dtype=np.float32), self._mode, submit)

    def _finish_meeting_session(self) -> None:
        recorder = self._meeting_recorder
        self._meeting_recorder = None
        if recorder is None:
            return
        if recorder.samples < SR // 2:
            recorder.discard()
            self._cancel_capture()
            return
        try:
            recording = recorder.close()
        except OSError as error:
            print(f"{self.cfg.meeting.mode} audio close failed ({error}); "
                  f"kept {recorder.path}", flush=True)
            self.overlay.result("Audio not finalized — WAV retained",
                                "error", auto_close_s=4.0)
            self._finish_ui(self._session_token)
            return
        self._meeting_processing_path = recording
        self._turn.close()
        self._stop_recording()
        self.state = PARSING
        self.overlay.state("processing")
        with self._session_lock:
            token = self._session_token
        worker = (self._process_collaborative
                  if self.cfg.meeting.mode == "collaborative"
                  else self._process_meeting)
        self._dictation_pool.submit(worker, token, recording)

    def _process_meeting(self, token: int, recording: Path) -> None:
        def cancelled() -> bool:
            with self._session_lock:
                return self._session_token != token

        try:
            document, summary_error = self._meeting_processor.process(
                recording, cancelled=cancelled)
        except MeetingCancelled:
            recording.unlink(missing_ok=True)
            return
        except Exception as error:  # noqa: BLE001 — keep the WAV for recovery
            if not cancelled():
                print(f"meeting processing failed ({error}); audio: {recording}",
                      flush=True)
                self.overlay.result("Meeting processing failed — WAV retained",
                                    "error", auto_close_s=4.0)
                self.cues.play("error")
        else:
            if cancelled():
                document.unlink(missing_ok=True)
                return
            print(f"meeting transcript saved: {document}", flush=True)
            if summary_error:
                print(f"meeting summary unavailable ({summary_error})", flush=True)
            self.overlay.result(
                "Transcript saved; summary unavailable" if summary_error
                else "Meeting notes saved",
                "warn" if summary_error else "ok", auto_close_s=2.5,
                show_text=not summary_error)
            self.cues.play("error" if summary_error else "executed")
        finally:
            if not cancelled():
                self._meeting_processing_path = None
                self._finish_ui(token)

    def _process_collaborative(self, token: int, recording: Path) -> None:
        """Diarize once after Stop; keep a private WAV until the draft is safe."""
        def cancelled() -> bool:
            with self._session_lock:
                return self._session_token != token

        queued = False
        try:
            if cancelled():
                raise MeetingCancelled()
            samples, rate = read_wav(recording)
            if rate != SR:
                raise ValueError(f"Expected {SR} Hz audio, got {rate}")
            lines = self._meeting_processor.diarize(samples, cancelled)
            if cancelled():
                raise MeetingCancelled()
            deterministic = self.cfg.polish.deterministic
            text = collaborative_text(
                lines, self._words if deterministic else None,
                self.cfg.polish.filler if deterministic else False,
                self.cfg.polish.emoji if deterministic else False)
            if not text:
                raise RuntimeError("No intelligible speech found")
            if cancelled():
                raise MeetingCancelled()
            try:
                ts = self.recovery.spool({"kind": "commit", "text": text,
                                          "mode": "collaborative"})
            except OSError as error:
                print(f"    collaborative recovery unavailable ({error})",
                      flush=True)
                ts = None
                self.overlay.show("Recovery unavailable — finishing paste",
                                  "warn")
            if cancelled():
                if ts is not None:
                    self.recovery.drop(ts)
                raise MeetingCancelled()
            self._paste_q.put((text, {}, None, ts, recording,
                               "collaborative", token))
            queued = True
        except MeetingCancelled:
            recording.unlink(missing_ok=True)
        except Exception as error:  # noqa: BLE001 — retain raw audio on error
            if not cancelled():
                print(f"collaborative processing failed ({error}); "
                      f"audio: {recording}", flush=True)
                self.overlay.result("Transcription failed — WAV retained",
                                    "error", auto_close_s=4.0)
                self.cues.play("error")
        finally:
            if not queued and not cancelled():
                self._meeting_processing_path = None
                self._finish_ui(token)

    def _finish_ui(self, token: int) -> None:
        with self._session_lock:
            if self._session_token != token:
                return
            self._session_parts.clear()
            self._draft_ts = None
            self._cleanup_audio_spill()
            self._dictation_count = 0
            self.state = IDLE
            self.overlay.state("idle")
            self.overlay.session_close()

    def _keep_listening(self) -> None:
        """Continuous session: the next turn starts with empty audio and
        a new stream opens only when its own speech is detected."""
        self._turn.open()
        self.vad.clear()  # an engine endpoint may precede VAD's old endpoint
        self._stream_open = False
        self.state = LISTENING
        self.overlay.state("listening")
        self._preroll = []
        self._last_preview = ""
        self._wake_kw = None

    def _paste_loop(self) -> None:
        """Insert the fully formatted session once, off the capture thread."""
        while True:
            item = self._paste_q.get()
            if item is None:
                return
            text, ev, submit, ts, audio, mode, token = item
            with self._session_lock:
                current = self._session_token == token
            if not current:
                if ts is not None:
                    self.recovery.drop(ts)
                if isinstance(audio, Path):
                    audio.unlink(missing_ok=True)
                continue
            res = self._paste_at_focus(text, ev, submit=submit)
            inserted = res.ok and ev.get("inserted", False)
            self.cues.play("executed" if inserted else "error")
            persisted = inserted or ts is not None
            if inserted:
                # A Wayland paste shortcut can be delivered without any API
                # confirming the target accepted it; retain recovery quietly.
                if ts is not None and not ev.get("unverified"):
                    self.recovery.drop(ts)
                self._append_history(text, mode)
            else:
                if isinstance(audio, Path):
                    apath = ""  # completed text is spooled; no PCM copy needed
                else:
                    try:
                        apath = (self.recovery.save_audio(audio)
                                 if len(audio) else "")
                    except OSError as error:
                        print(f"    recovery audio unavailable ({error})",
                              flush=True)
                        apath = ""
                try:
                    self.recovery.spool({"kind": "failed", "text": text,
                                         "mode": mode, "audio": apath})
                    persisted = True
                    if ts is not None:
                        self.recovery.drop(ts)
                except OSError as error:
                    print(f"    recovery transcript unavailable ({error})",
                          flush=True)
                    if ts is None:
                        copied = self.executor.clipboard_only(text)
                        self.overlay.result(
                            "Paste failed — text copied; paste manually"
                            if copied.ok else "Paste failed — cannot save text",
                            "warn" if copied.ok else "error", auto_close_s=4.0)
            if isinstance(audio, Path):
                if persisted:
                    audio.unlink(missing_ok=True)
                else:
                    print(f"    collaborative audio retained: {audio}", flush=True)
                with self._session_lock:
                    if self._session_token == token:
                        self._meeting_processing_path = None
            if persisted:
                with self._session_lock:
                    if self._session_token == token and self._draft_ts is not None:
                        self.recovery.drop(self._draft_ts)
                        self._draft_ts = None
            self._finish_ui(token)
            self._print_outcome(ev)

    def _paste_at_focus(self, text: str, ev: dict,
                        submit: bool | None = False) -> Result:
        """Paste into the keyboard focus at completion, not at recording start.

        GNOME Wayland may hide the target identity. In that case a physical
        paste shortcut follows the current cursor, but never sends Enter.
        """
        try:
            focus_now = _probe_focus()
        except RuntimeError:
            focus_now = None
        if sys.platform == "darwin" and not focus_now:
            res = self.executor.clipboard_only(text)
            self.overlay.result(
                "Copied; paste manually" if res.ok
                else "Paste unavailable — transcript saved for recovery",
                "warn" if res.ok else "error", auto_close_s=3.0)
            ev["inserted"] = False
            ev["result"] = res
            return res
        if sys.platform == "darwin":
            res = self.executor.dictate(text, _paste_chord(focus_now), submit,
                                        expected_focus=focus_now)
        else:
            res = self.executor.dictate(
                text, _paste_chord(focus_now), submit if focus_now else None,
                expected_focus=focus_now,
                focus_probe=_probe_focus if focus_now else None)
        if not res.ok:
            self.cues.play("error")
        self.overlay.result(
            ("Inserted" if focus_now else "Paste sent") if res.ok else
            "Paste failed — transcript saved for recovery",
            "ok" if res.ok else "error", auto_close_s=2.5)
        ev["inserted"] = res.ok
        ev["unverified"] = res.ok and focus_now is None
        ev["result"] = res
        return res

    def _polish_text(self, text: str) -> str:
        """Polish short sessions with an optional endpoint; avoid a doomed
        600-token completion for an hour-long transcript."""
        if (not self.cfg.polish.enabled or not self.cfg.llm.endpoint
                or len(text) < 8 or len(text) > 6000):
            return text
        sys_, usr_ = polish_prompt(text)
        polished = self.llm.complete(sys_, usr_)
        if polished:
            return polish_guard(text, polished)
        return text

    def _append_history(self, text: str, mode: str) -> None:
        """Append one successfully pasted session to history (best-effort)."""
        if self.history_path is None or not text.strip():
            return
        try:
            line = json.dumps({"ts": time.time(), "text": text,
                               "mode": mode}, ensure_ascii=False)
            flags = os.O_WRONLY | os.O_APPEND | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
            fd = os.open(self.history_path, flags, 0o600)
            with os.fdopen(fd, "a", encoding="utf-8") as f:
                os.fchmod(f.fileno(), 0o600)
                f.write(line + "\n")
        except OSError:
            pass

    def _ptt_start(self) -> None:
        """Push-to-talk: begin a one-shot capture (ends on key release)."""
        self._wake()  # resets _mode to "continuous"
        self._mode = "ptt"

    def _ptt_end(self) -> None:
        """Key release: finalize the one-shot session (polish + paste once)."""
        self._finish_capture()

    def _finish_capture(self, submit: bool = False) -> None:
        """Stop the active dictation or speaker-labelled recording."""
        if self.cfg.meeting.mode in ("meeting", "collaborative"):
            self._finish_session()
            return
        self._commit_turn(close_session=True, submit=submit)

    def _on_control(self, msg: str) -> None:
        if msg == "identify":
            if self.state == IDLE:
                self.overlay.ready(self.enabled)
            return

        if msg == "toggle":
            self.enabled = not self.enabled
            if not self.enabled and self.state in (LISTENING, PARSING):
                self._cancel_capture()
            self.overlay.state("off" if not self.enabled else "idle")
            print(f"voice-io {'off' if not self.enabled else 'on'}", flush=True)
            return
        if not self.enabled:
            return
        if msg == "push":
            if self.state == IDLE:
                self._wake()
            elif self.state == LISTENING:
                self._finish_capture()
        elif msg == "ptt":
            # Push-to-talk: key press starts a one-shot session...
            if self.state == IDLE:
                self._ptt_start()
        elif msg == "ptt_stop":
            # ...key release finalizes it (polish + paste once + close).
            if self._mode == "ptt" and self.state in (LISTENING, PARSING):
                self._ptt_end()
        elif msg == "cancel":
            if self.state in (LISTENING, PARSING):
                self._cancel_capture()
                self.cues.play("idle")
                print("voice-io dictation cancelled", flush=True)
        elif msg == "status":
            print(f"state={self.state} language={self.language} "
                  f"enabled={self.enabled}", flush=True)
        elif msg == "quit":
            self._running = False

    def run_live(self, duration: float = 0.0) -> int:
        if sys.platform == "darwin":
            _probe_focus()  # Accessibility preflight; fail before opening mic.
        self.control.open()
        hotkeys = None
        mic = None
        try:
            if sys.platform == "darwin":
                from voice_io.mac_hotkeys import MacHotkeys
                from voice_io.control import write
                hotkeys = MacHotkeys(self.cfg.hotkeys, write)
                hotkeys.start()
            mic = MicSource()
            self.overlay.state("idle")
            self.cues.play("listening")
            print("voice-io daemon ready", flush=True)
            self.overlay.ready()
            # Warm the English final engine off the capture-critical path.
            self._dictation_pool.submit(self._refiner.warm)
            t0 = time.time()
            while self._running and (duration <= 0 or time.time() - t0 < duration):
                samples = mic.read(0.1)
                self.vad.accept(samples)
                # Pre-roll: keep the last ~0.6s of audio.
                self._preroll.append(samples)
                total = sum(len(c) for c in self._preroll)
                while total > int(0.6 * SR):
                    drop = self._preroll.pop(0)
                    total -= len(drop)
                for msg in self.control.read():
                    self._on_control(msg)
                if self.state == IDLE and self.enabled:
                    for kw in self.kws.accept(samples):
                        if kw == "SEND":
                            print(f"SEND ignored (idle): {kw}", flush=True)
                            continue
                        if kw == "WAKE":
                            print("WAKE detected", flush=True)
                            self._wake(kw)
                elif self.state == LISTENING:
                    if self.cfg.meeting.mode in ("meeting", "collaborative"):
                        self._listen(samples)
                    else:
                        detected = self.kws.accept(samples)
                        if "SEND" in detected:
                            print("SEND detected", flush=True)
                            self._finish_capture(submit=True)
                        elif detected:
                            print(f"WAKE ignored (in session): {detected}", flush=True)
                        else:
                            self._listen(samples)
                    self._check_session_limit()
        finally:
            if mic is not None:
                mic.close()
            if hotkeys is not None:
                hotkeys.stop()
        return 0

    def _check_session_limit(self, now: float | None = None) -> None:
        """Warn once before the session ceiling, then finalize captured turns."""
        if self.state != LISTENING:
            return
        elapsed = (time.monotonic() if now is None else now) - self._session_t0
        maximum = self.cfg.dictation.session_max_s
        if elapsed >= maximum:
            print("session max reached — finalizing", flush=True)
            self.overlay.show(
                f"Session limit reached — finishing {self.cfg.meeting.mode}",
                "warn")
            self._finish_capture()
            self.cues.play("idle")
        elif (not self._session_warned
              and elapsed >= maximum - min(60.0, maximum * 0.1)):
            self._session_warned = True
            self.overlay.show("Session ending soon — press Stop to finish",
                              "warn")
            self.cues.play("input")

    def _print_outcome(self, out: dict) -> None:
        if "text" in out:  # dictation event (voice = keyboard)
            print(f"    text:     {out['text']!r}")
            if out.get("refine"):
                print(f"    refine:   {out['refine']!r}")
            if not out["inserted"]:
                label = out.get("control") or "Didn't understand"
                print(f"    inserted: false ({label})")
            else:
                res = out.get("result")
                print(f"    inserted: {res.message if res else True}")

    # ---- lifecycle ----

    def close(self) -> None:
        if self.state == LISTENING:
            self._cancel_capture()
        self._stop_recording()
        # Finish accepted turns before terminating the paste worker; a
        # one-shot/quit must not silently drop a still-running re-decode.
        self._dictation_pool.shutdown(wait=True)
        self._paste_q.put(None)
        self._paste_thread.join()
        with self._session_lock:
            self._cleanup_audio_spill()
        self.overlay.close()
        self.control.close()
