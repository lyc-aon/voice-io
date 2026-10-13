"""Live-session transitions for dictation and diarized capture."""

import queue
import tempfile
import threading
import time
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import numpy as np

from voice_io.config import Config
from voice_io.daemon import Daemon, IDLE, LISTENING, PARSING, _strip_end_phrase
from voice_io.executor import Executor, Result
from voice_io.levels import speech_level
from voice_io.meeting import (MeetingCancelled, MeetingProcessor,
                              MeetingRecorder, SpeechLine)
from voice_io.overlay_mac import OverlayState
from voice_io.recovery import RecoveryStore
from voice_io.turn import Turn


class LiveTurnTests(unittest.TestCase):
    def setUp(self):
        cfg = Config()
        self.daemon = Daemon.__new__(Daemon)
        self.daemon.cfg = cfg
        self.daemon.enabled = True
        self.daemon.language = "en"
        self.daemon._asr_failures = 0
        self.daemon._degraded = False
        self.daemon.vad = SimpleNamespace(pop_segments=lambda: [], clear=lambda: None)
        self.daemon._turn = Turn()
        self.daemon._stream_open = False
        self.daemon._last_preview = ""
        self.daemon._wake_kw = None
        self.daemon._preroll = []
        self.daemon._mode = "continuous"
        self.daemon._rec_buf = None
        self.daemon._noise_floor = 0.0
        self.daemon._session_lock = threading.Lock()
        self.daemon._session_token = 0
        self.daemon._session_parts = []
        self.daemon._draft_ts = None
        self.daemon._draft_storage_warned = False
        self.daemon._dictation_count = 0
        self.daemon._audio_spill = None
        self.daemon._spill_readers = set()
        self.daemon._retired_spills = []
        self.daemon._session_t0 = time.monotonic()
        self.daemon._session_warned = False
        self.spill_home = tempfile.TemporaryDirectory()
        self.addCleanup(self.spill_home.cleanup)
        self.daemon.overlay = SimpleNamespace(state=lambda _: None,
                                              session_close=lambda: None,
                                              clear=lambda: None)
        self.daemon.recovery = SimpleNamespace(
            dir=Path(self.spill_home.name), spool=lambda _: None,
            drop=lambda _: None)
        self.focus = patch("voice_io.daemon._probe_focus", return_value=None)
        self.focus.start()
        self.addCleanup(self.focus.stop)

    def test_cancel_discards_previous_turns_and_closes_pill(self):
        d = self.daemon
        d._turn.open()
        d._turn.add(np.ones(16000, dtype=np.float32))
        d._turn.close()
        d._keep_listening()
        self.assertEqual(d.state, LISTENING)
        self.assertIsNone(d._turn.audio)
        d._session_parts.append("Draft release notes.")
        d._dictation_count = 1
        d._turn.add(np.full(8000, 0.25, dtype=np.float32))
        d._cancel_capture()
        self.assertEqual(d.state, IDLE)
        self.assertFalse(d._turn.is_open)
        self.assertEqual(d._session_parts, [])
        self.assertEqual(d._dictation_count, 0)
        self.assertEqual(d._session_token, 1)

    def test_refined_turns_are_durable_without_early_paste_and_cancel_discards(self):
        d = self.daemon
        d.state = LISTENING
        d._paste_q = queue.Queue()
        d._refiner = SimpleNamespace(transcribe=lambda audio: (
            "First thought." if audio[0] < 0.5 else "Second thought."))
        d.recovery = RecoveryStore(Path(self.spill_home.name) / "drafts")
        for sample in (0.25, 0.75):
            d._dictation_worker(np.full(16000, sample, dtype=np.float32),
                                "", False, {}, 0, False)
        self.assertEqual(d.recovery.latest()["text"],
                         "First thought. Second thought.")
        self.assertEqual(d.recovery.latest()["kind"], "draft")
        self.assertEqual(len(list(d.recovery.spool_dir.glob("*.json"))), 1)
        self.assertTrue(d._paste_q.empty())
        d._cancel_capture()
        self.assertIsNone(d.recovery.latest())


    def test_saved_text_remains_recoverable_without_interrupting_new_dictation(self):
        d = self.daemon
        d.recovery = RecoveryStore(Path(self.spill_home.name) / "saved")
        saved_id = d.recovery.spool(
            {"kind": "failed", "text": "Previous text I still need."})
        messages = []
        d.overlay.session_open = lambda _: None
        d.overlay.show = lambda text, style: messages.append((text, style))
        d.cues = SimpleNamespace(play=lambda _: None)
        d._record_path = None

        d._wake()
        self.assertEqual(d.state, LISTENING)
        self.assertTrue(d._turn.is_open)
        self.assertEqual(messages, [])
        d._cancel_capture()
        self.assertEqual(d.recovery.latest()["id"], saved_id)
        self.assertEqual(d.recovery.latest()["text"],
                         "Previous text I still need.")

    def test_configured_end_phrase_only_removes_trailing_phrase(self):
        self.assertEqual(_strip_end_phrase("Ship the release. voice send!",
                                           "voice send"), "Ship the release")
        self.assertEqual(_strip_end_phrase("The voice send phrase matters",
                                           "voice send"),
                         "The voice send phrase matters")

    def test_meeting_stop_writes_speaker_notes_without_pasting(self):
        d = self.daemon
        d.cfg.meeting.mode = "meeting"
        d._meeting_recorder = MeetingRecorder(Path(self.spill_home.name) / "meetings")
        d._meeting_recorder.append(np.ones(16000, dtype=np.float32))
        d._meeting_processing_path = None
        d._meeting_processor = MeetingProcessor(d.cfg, refiner=None)
        d._meeting_processor.diarize = lambda _samples, _cancelled: [
            SpeechLine(0, 1, 0, "Team decided to review the design.")]
        d._dictation_pool = ThreadPoolExecutor(max_workers=1)
        self.addCleanup(d._dictation_pool.shutdown, wait=True)
        d._paste_q = queue.Queue()
        d.cues = SimpleNamespace(play=lambda _: None)
        d.overlay.result = lambda *_args, **_kwargs: None
        d._paste_at_focus = lambda *_args, **_kwargs: self.fail(
            "meeting transcription pasted into editor")
        d.state = LISTENING
        d._turn.open()

        d._finish_capture()
        d._dictation_pool.submit(lambda: None).result(timeout=5)
        documents = list((Path(self.spill_home.name) / "meetings").glob("*.md"))
        self.assertEqual(len(documents), 1)
        self.assertIn("Speaker 1: Team decided to review the design.",
                      documents[0].read_text())
        self.assertFalse(list(documents[0].parent.glob("*.wav")))
        self.assertTrue(d._paste_q.empty())
        self.assertEqual(d.state, IDLE)

    def _collaborative_round(self, paste_fails=False):
        d = self.daemon
        d.cfg.meeting.mode = "collaborative"
        d._words = {}
        d._meeting_recorder = MeetingRecorder(Path(self.spill_home.name) / "meetings")
        d._meeting_recorder.append(np.ones(16000, dtype=np.float32))
        d._meeting_processing_path = None
        d._meeting_processor = MeetingProcessor(d.cfg, refiner=None)
        d._meeting_processor.diarize = lambda _samples, _cancelled: [
            SpeechLine(0, .7, 0, "um let's design the tool."),
            SpeechLine(.7, 1, 1, "I will test it.")]
        d._dictation_pool = ThreadPoolExecutor(max_workers=1)
        self.addCleanup(d._dictation_pool.shutdown, wait=True)
        d._paste_q = queue.Queue()
        d.cues = SimpleNamespace(play=lambda _: None)
        d.overlay.result = lambda *_args, **_kwargs: None
        d.overlay.show = lambda *_args, **_kwargs: None
        d.recovery = RecoveryStore(Path(self.spill_home.name) / "recovery")
        d.history_path = None
        d.executor = Executor(dry_run=False, auto_submit="enter")
        inserts = []
        if paste_fails:
            def blocked(text, *_args):
                inserts.append(text)
                raise OSError("paste blocked")
            d.executor._clipboard_paste = blocked
        else:
            d.executor._clipboard_paste = lambda text, *_args: inserts.append(text)
        d.executor._send_key = lambda _: self.fail("collaborative draft sent Enter")
        d._print_outcome = lambda _: completed.set()
        completed = threading.Event()
        d._paste_thread = threading.Thread(target=d._paste_loop, daemon=True)
        d._paste_thread.start()
        self.addCleanup(lambda: (d._paste_q.put(None), d._paste_thread.join(5)))
        d.state = LISTENING
        d._turn.open()
        with patch("voice_io.daemon._probe_focus", return_value="tern|pane:42"):
            d._finish_capture(submit=True)  # even an explicit send must not submit
            self.assertTrue(completed.wait(5), "collaborative paste did not finish")
        return d, inserts

    def test_collaborative_stop_pastes_labelled_draft_without_sending_or_summary(self):
        d, inserts = self._collaborative_round()
        self.assertEqual(inserts, [
            "Speaker 1: let's design the tool.\n"
            "Speaker 2: I will test it."])
        self.assertEqual(d.state, IDLE)
        self.assertFalse(list((Path(self.spill_home.name) / "meetings").glob("*.wav")))
        self.assertFalse(list((Path(self.spill_home.name) / "meetings").glob("*.md")))
        self.assertIsNone(d.recovery.latest())

    def test_collaborative_paste_failure_preserves_editable_speaker_draft(self):
        d, inserts = self._collaborative_round(paste_fails=True)
        self.assertEqual(d.state, IDLE)
        self.assertEqual(d.recovery.latest()["kind"], "failed")
        self.assertEqual(d.recovery.latest()["text"], inserts[0])
        self.assertFalse(list((Path(self.spill_home.name) / "meetings").glob("*.wav")))

    def test_collaborative_diarizer_failure_keeps_private_audio_for_recovery(self):
        d = self.daemon
        recorder = MeetingRecorder(Path(self.spill_home.name) / "meetings")
        recorder.append(np.ones(16000, dtype=np.float32))
        recording = recorder.close()
        d._meeting_processing_path = recording
        d._meeting_processor = SimpleNamespace(
            diarize=lambda *_args: (_ for _ in ()).throw(RuntimeError("decode failed")))
        d.overlay.result = lambda *_args, **_kwargs: None
        d.cues = SimpleNamespace(play=lambda _: None)
        d.state = PARSING

        d._process_collaborative(d._session_token, recording)

        self.assertTrue(recording.exists())
        self.assertEqual(d.state, IDLE)
        self.assertIsNone(d._meeting_processing_path)

    def test_collaborative_cancel_discards_wav_without_pasting(self):
        d = self.daemon
        recorder = MeetingRecorder(Path(self.spill_home.name) / "meetings")
        recorder.append(np.ones(16000, dtype=np.float32))
        recording = recorder.close()
        d._meeting_processing_path = recording
        d._meeting_recorder = None
        d.state = PARSING
        d._paste_q = queue.Queue()

        def cancel_during_diarization(_samples, _cancelled):
            d._cancel_capture()
            raise MeetingCancelled()

        d._meeting_processor = SimpleNamespace(diarize=cancel_during_diarization)
        d._process_collaborative(d._session_token, recording)

        self.assertFalse(recording.exists())
        self.assertTrue(d._paste_q.empty())
        self.assertEqual(d.state, IDLE)

    def test_meter_rejects_room_noise_but_follows_detected_speech(self):
        d = self.daemon
        d._turn.open()
        levels = []
        heard = []
        speech = False
        d.overlay.level = levels.append
        d.vad = SimpleNamespace(is_speech=lambda: speech,
                                pop_segments=lambda: [])
        d.asr = SimpleNamespace(new_stream=lambda: None,
                                accept=lambda audio: heard.append(audio) or "",
                                is_endpoint=lambda: False)
        d._listen(np.full(1600, 0.04, dtype=np.float32))
        self.assertEqual(levels, [0.0])
        self.assertEqual(heard, [])

        speech = True
        d._listen(np.full(1600, 0.015, dtype=np.float32))
        self.assertAlmostEqual(levels[-1], 0.015 - 1.3 * 0.0032, places=5)
        self.assertEqual(len(heard), 1)
        speech = False
        d._listen(np.full(1600, 0.007, dtype=np.float32))
        self.assertGreater(levels[-1], 0.0)
        self.assertLess(levels[-1], levels[-2])
        self.assertEqual(len(heard), 2)

    def test_live_speech_shows_short_preview_without_early_paste(self):
        d = self.daemon
        d._turn.open()
        d._paste_q = queue.Queue()
        previews = []
        d.overlay.level = lambda _: None
        d.overlay.preview = previews.append
        d.vad = SimpleNamespace(is_speech=lambda: True,
                                pop_segments=lambda: [])
        phrases = iter(("I", "I am testing", "I am testing long",
                        "I am testing long form", "I am testing long form now"))
        d.asr = SimpleNamespace(new_stream=lambda: None,
                                accept=lambda _: next(phrases),
                                is_endpoint=lambda: False)
        for _ in range(4):
            d._listen(np.full(1600, 0.015, dtype=np.float32))
        self.assertEqual(previews, ["I", "I am testing", "am testing long",
                                    "testing long form"])
        self.assertTrue(d._paste_q.empty())
        self.assertEqual(d._dictation_count, 0)
        d.cfg.ui.live_preview = False
        d._listen(np.full(1600, 0.015, dtype=np.float32))
        self.assertEqual(previews[-1], "testing long form")

    def test_idle_capture_cannot_turn_a_long_wait_into_a_long_decode(self):
        d = self.daemon
        d._turn.open()
        d.overlay.level = lambda _: None
        speech = False
        d.vad = SimpleNamespace(is_speech=lambda: speech,
                                pop_segments=lambda: [])
        heard = []
        d.asr = SimpleNamespace(new_stream=lambda: None,
                                accept=lambda audio: heard.append(audio) or "",
                                is_endpoint=lambda: False)
        quiet = np.zeros(1600, dtype=np.float32)
        for _ in range(600):  # one minute waiting for the first utterance
            d._listen(quiet)
        self.assertLessEqual(d._turn.seconds, 0.6)
        speech = True
        d._listen(np.full(1600, 0.015, dtype=np.float32))
        self.assertEqual(len(heard), 1)
        self.assertLessEqual(len(heard[0]), int(0.7 * 16000))

    def test_explicit_close_after_endpoint_never_replays_old_stream(self):
        d = self.daemon
        d._turn.open()
        d._turn.add(np.zeros(16000, dtype=np.float32))  # trailing silence
        d.asr = SimpleNamespace(finalize=lambda **_: self.fail("stale ASR final"))
        d._commit_turn(close_session=True)
        self.assertEqual(d.state, IDLE)
        self.assertFalse(d._turn.is_open)

    def test_command_shaped_english_turn_is_saved_and_inserted_as_dictation(self):
        d = self.daemon
        d.cfg.polish.deterministic = False
        d._dictation_pool = ThreadPoolExecutor(max_workers=1)
        self.addCleanup(d._dictation_pool.shutdown, wait=True)
        d._paste_q = queue.Queue()
        d.recovery = RecoveryStore(Path(self.spill_home.name) / "commands-as-text")
        d._refiner = SimpleNamespace(transcribe=lambda *_: "open firefox")
        d.asr = SimpleNamespace(finalize=lambda **_: "open firefox")
        d._snippets, d._words = {}, {}
        d._polish_text = lambda text: text
        d._print_outcome = lambda _: None
        d.cues = SimpleNamespace(play=lambda _: None)
        d._turn.open()
        d._turn.add(np.ones(16000, dtype=np.float32))
        d._stream_open = True
        d.state = LISTENING

        with patch("voice_io.executor.subprocess.Popen") as launch:
            d._commit_turn()
            d._dictation_pool.submit(lambda: None).result(timeout=5)
            self.assertEqual(d.recovery.latest()["kind"], "draft")
            self.assertEqual(d.recovery.latest()["text"], "open firefox")
            self.assertTrue(d._paste_q.empty())
            d._finish_capture()
            d._dictation_pool.submit(lambda: None).result(timeout=5)
            launch.assert_not_called()
        self.assertEqual(d._paste_q.get_nowait()[0], "open firefox")

    def test_stale_refinement_after_cancel_cannot_restore_draft(self):
        d = self.daemon
        d.state = LISTENING
        d._session_token = 2
        d._refiner = SimpleNamespace(transcribe=lambda *_: "Draft release notes.")
        d._paste_q = queue.Queue()
        audio = np.ones(16000, dtype=np.float32)

        d._dictation_worker(audio, "draft release notes", False, {},
                            1, False)
        self.assertEqual(d._session_parts, [])
        self.assertTrue(d._paste_q.empty())

        d._dictation_worker(audio, "draft release notes", False, {},
                            2, False)
        self.assertEqual(d._session_parts, ["Draft release notes."])
        self.assertTrue(d._paste_q.empty())

    def test_spoken_control_phrases_do_not_enter_final_paste(self):
        d = self.daemon
        d._refiner = SimpleNamespace(
            transcribe=lambda *_: "hey voice Ship the report. voice send")
        d._paste_q = queue.Queue()
        d._snippets = {}
        d._words = {}
        d._polish_text = lambda text: text
        d.recovery = SimpleNamespace(spool=lambda entry: 4.0,
                                     drop=lambda _: None)
        d._dictation_worker(np.ones(16000, dtype=np.float32), "Ship the report",
                            True, {}, 0, True)
        d._finish_dictation_worker(
            0, {}, np.empty(0, dtype=np.float32), "continuous", True)
        self.assertEqual(d._paste_q.get_nowait()[0], "Ship the report")

    def test_stop_pastes_once_after_ordered_full_session_cleanup(self):
        d = self.daemon
        d._dictation_pool = ThreadPoolExecutor(max_workers=1)
        self.addCleanup(d._dictation_pool.shutdown, wait=True)
        d._paste_q = queue.Queue()
        d._snippets = {}
        d._words = {"hextee": "HexT"}
        d._mode = "continuous"
        d._refiner = SimpleNamespace(
            transcribe=lambda audio: (
                "um hextee first." if audio[0] < 0.25 else "uh second."))
        finals = iter(["um hextee first.", "uh second."])
        d.asr = SimpleNamespace(finalize=lambda **_: next(finals))
        polished = []
        d._polish_text = lambda text: polished.append(text) or text.replace(
            "second.", "Second.")
        events = []
        d.overlay.result = lambda *args, **kwargs: events.append(("result", args))
        d.overlay.state = lambda state: events.append(("state", state))
        d.overlay.session_close = lambda: events.append(("closed", None))
        d._print_outcome = lambda _: None
        d.cues = SimpleNamespace(play=lambda _: None)
        d.recovery = SimpleNamespace(
            dir=Path(self.spill_home.name),
            spool=lambda entry: events.append(("spool", entry)) or 17.0,
            drop=lambda ts: events.append(("drop", ts)))
        d._append_history = lambda text, mode: events.append(("history", text))
        def insert(text, ev, submit):
            events.append(("paste", text, submit))
            ev["inserted"] = True
            return Result(ok=True, message="pasted")
        d._paste_at_focus = insert

        d._turn.open()
        d._turn.add(np.full(16000, 0.2, dtype=np.float32))
        d._stream_open = True
        d._commit_turn()
        self.assertEqual(d.state, LISTENING)
        self.assertTrue(d._paste_q.empty())

        d._turn.add(np.full(16000, 0.3, dtype=np.float32))
        d._stream_open = True
        d._commit_turn()
        self.assertTrue(d._paste_q.empty())
        d._finish_capture()  # no residual stream; Stop must not press Enter
        self.assertNotEqual(d.state, IDLE)  # processing until insertion
        d._dictation_pool.submit(lambda: None).result(timeout=5)
        self.assertEqual(polished, ["HexT first. second."])
        self.assertEqual(d._paste_q.qsize(), 1)

        d._paste_q.put(None)
        d._paste_loop()
        self.assertEqual(
            [event for event in events if event[0] == "paste"],
            [("paste", "HexT first. Second.", False)])
        self.assertEqual([event for event in events if event[0] == "history"],
                         [("history", "HexT first. Second.")])
        self.assertEqual(d.state, IDLE)
        self.assertEqual([e for e in events if e[0] == "closed"],
                         [("closed", None)])

    def test_session_ceiling_warns_once_then_finalizes_one_paste(self):
        d = self.daemon
        self.assertEqual(d.cfg.dictation.session_max_s, 3600)
        self.assertEqual(d.cfg.turn.max_sec, 20)
        d.cfg.dictation.session_max_s = 100
        d.cfg.polish.deterministic = False
        d._session_t0 = 1000.0
        d._dictation_pool = ThreadPoolExecutor(max_workers=1)
        self.addCleanup(d._dictation_pool.shutdown, wait=True)
        self.addCleanup(lambda: d._audio_spill and d._audio_spill.cleanup())
        d._paste_q = queue.Queue()
        d._refiner = SimpleNamespace(transcribe=lambda *_: "captured words")
        d.asr = SimpleNamespace(finalize=lambda **_: "captured words")
        d._snippets, d._words = {}, {}
        d._polish_text = lambda text: text
        d.recovery = SimpleNamespace(
            dir=Path(self.spill_home.name), spool=lambda _: 1.0,
            drop=lambda _: None)
        d.cues = SimpleNamespace(play=lambda _: None)
        warnings = []
        d.overlay.show = lambda text, kind: warnings.append((text, kind))
        d._turn.open()
        d._turn.add(np.ones(16000, dtype=np.float32))
        d._stream_open = True
        d.state = LISTENING

        d._check_session_limit(now=1091.0)
        d._check_session_limit(now=1098.0)
        self.assertEqual(warnings,
                         [("Session ending soon — press Stop to finish", "warn")])
        self.assertEqual(d.state, LISTENING)
        d._check_session_limit(now=1100.0)
        d._dictation_pool.submit(lambda: None).result(timeout=5)
        self.assertEqual(d.state, "PARSING")
        self.assertEqual(d._paste_q.get_nowait()[0], "captured words")
        self.assertTrue(d._paste_q.empty())
        self.assertEqual(warnings[-1],
                         ("Session limit reached — finishing dictation", "warn"))
        spill = Path(d._audio_spill.name)
        d._finish_ui(d._session_token)
        self.assertFalse(spill.exists())

    def test_slow_refinement_spills_in_order_and_cancel_clears_backlog(self):
        d = self.daemon
        d.cfg.polish.deterministic = False
        d._dictation_pool = ThreadPoolExecutor(max_workers=1)
        self.addCleanup(d._dictation_pool.shutdown, wait=True)
        d._paste_q = queue.Queue()
        d.asr = SimpleNamespace(finalize=lambda **_: "spoken words")
        d._snippets, d._words = {}, {}
        d._polish_text = lambda text: text
        d._print_outcome = lambda _: None
        d.cues = SimpleNamespace(play=lambda _: None)
        with tempfile.TemporaryDirectory() as root:
            d.recovery = SimpleNamespace(dir=Path(root), spool=lambda _: 1.0,
                                         drop=lambda _: None)
            for cancel in (False, True):
                entered, release = threading.Event(), threading.Event()

                def transcribe(samples):
                    entered.set()
                    release.wait(5)
                    return f"segment {int(samples[0])}"

                d._refiner = SimpleNamespace(transcribe=transcribe)
                d.state = LISTENING
                d._turn.open()
                for number in (1, 2, 3):
                    d._turn.add(np.full(16000, number, dtype=np.float32))
                    d._stream_open = True
                    d._commit_turn()
                self.assertTrue(entered.wait(5))
                spill = Path(d._audio_spill.name)
                paths = list(spill.glob("*.npy"))
                self.assertEqual(len(paths), 3)
                self.assertEqual(spill.stat().st_mode & 0o777, 0o700)
                self.assertTrue(all(p.stat().st_mode & 0o777 == 0o600
                                    for p in paths))
                self.assertIsNone(d._turn.audio)
                if cancel:
                    d._cancel_capture()
                    self.assertTrue(spill.exists())  # active reader still owns it
                else:
                    d._finish_capture()
                    self.assertTrue(d._paste_q.empty())
                release.set()
                d._dictation_pool.submit(lambda: None).result(timeout=5)
                if cancel:
                    self.assertEqual(d._session_parts, [])
                    self.assertFalse(spill.exists())
                    self.assertTrue(d._paste_q.empty())
                else:
                    self.assertEqual(d._paste_q.get_nowait()[0],
                                     "segment 1 segment 2 segment 3")
                    self.assertTrue(d._paste_q.empty())
                    self.assertFalse(list(spill.glob("*.npy")))
                    d._finish_ui(d._session_token)
                    self.assertFalse(spill.exists())

    def test_cancel_during_cleanup_discards_delayed_paste(self):
        d = self.daemon
        d._dictation_pool = ThreadPoolExecutor(max_workers=1)
        self.addCleanup(d._dictation_pool.shutdown, wait=True)
        d._paste_q = queue.Queue()
        d._session_parts = ["Draft speech."]
        d._dictation_count = 1
        d._snippets = {}
        d._words = {}
        d._turn.open()
        d._turn.add(np.zeros(1600, dtype=np.float32))
        d.state = LISTENING
        started = threading.Event()
        released = threading.Event()
        def delayed_polish(text):
            started.set()
            released.wait(5)
            return text
        d._polish_text = delayed_polish
        d.recovery = SimpleNamespace(
            spool=lambda _: self.fail("cancelled text spooled"))
        d.cues = SimpleNamespace(play=lambda _: None)
        d._finish_capture()
        self.assertTrue(started.wait(5))
        d._on_control("cancel")
        released.set()
        d._dictation_pool.submit(lambda: None).result(timeout=5)
        self.assertEqual(d.state, IDLE)
        self.assertTrue(d._paste_q.empty())

    def test_failed_paste_does_not_count_as_successful_insertion(self):
        d = self.daemon
        d._paste_q = queue.Queue()
        d.cues = SimpleNamespace(play=lambda _: None)
        d.executor = SimpleNamespace(
            dictate=lambda *args, **kwargs: Result(ok=False, message="key sender unavailable"))
        d.overlay.result = lambda *args, **kwargs: None
        d._print_outcome = lambda _: None
        d._append_history = lambda *_: self.fail("failed paste in history")
        spooled = []
        d.recovery = SimpleNamespace(
            drop=lambda _: None, save_audio=lambda _: "",
            spool=spooled.append)
        ev = {"text": "Final cleaned draft."}
        d._paste_q.put(("Final cleaned draft.", ev, False, 17.0,
                        np.empty(0, dtype=np.float32), "continuous", 0))
        d._paste_q.put(None)
        with patch("voice_io.daemon._probe_focus", return_value="new editor"):
            d._paste_loop()
        self.assertFalse(ev["inserted"])
        self.assertEqual(spooled[0]["kind"], "failed")
        self.assertEqual(spooled[0]["text"], "Final cleaned draft.")

    def test_both_platform_meters_follow_pcm_envelope(self):
        state = OverlayState()
        state.handle({"op": "session", "open": True})
        for rms in (0.0, 0.001, 0.015, 0.002):
            state.handle({"op": "level", "value": rms})
        self.assertEqual(state.history[-4], 0.0)
        self.assertEqual(state.history[-3], speech_level(0.001))
        self.assertGreater(state.history[-2], state.history[-1])
        self.assertGreater(state.history[-3], 0.2)
        state.handle({"op": "session", "open": True})
        self.assertFalse(any(state.history))


if __name__ == "__main__":
    unittest.main()
