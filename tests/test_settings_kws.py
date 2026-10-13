"""Portable settings, model pronunciations, and GNOME ownership contracts."""

import json
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

from voice_io import control
from voice_io.config import Config, parse_hotkey
from voice_io.kws import KWS, keyword_lines


class ConfigTests(unittest.TestCase):
    def test_hotkeys_are_portable_and_invalid_keys_rejected(self):
        self.assertEqual(parse_hotkey("Control+Option+Space"),
                         (frozenset({"ctrl", "alt"}), "space"))
        self.assertEqual(control.to_gnome_binding("super+shift+f3"),
                         "<Shift><Super>F3")
        for bad in ("space", "ctrl+alt+", "ctrl+ctrl+a", "ctrl+hyper+a",
                    "ctrl+alt+randomkey"):
            with self.subTest(bad=bad), self.assertRaises(ValueError):
                parse_hotkey(bad)

    def test_save_persists_selected_fields_preserving_other_sections(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "config.toml"
            before = ('# user comment\n[llm]\nendpoint = "local"\n'
                      '[ui]\nsounds = false\n# keep this note\n'
                      '[hotkeys]\npush = "ctrl+alt+space"\n')
            path.write_text(before, encoding="utf-8")
            with patch.object(control, "_gsettings") as gsettings:
                Config.load(path)
                Config.save_user({"ui": {"sounds": True, "theme": "light",
                                         "live_preview": False,
                                         "preview_color": "#53cc88"},
                                  "hotkeys": {"push": "super+shift+p"},
                                  "kws": {"wake_phrase": "hello computer",
                                          "end_phrase": "computer stop"}}, path)
                gsettings.assert_not_called()  # Loading/saving never overwrites GNOME.
            loaded = Config.load(path)
            self.assertEqual(loaded.hotkeys.push, "super+shift+p")
            self.assertEqual(loaded.kws.wake_phrase, "hello computer")
            self.assertEqual(loaded.kws.end_phrase, "computer stop")
            self.assertEqual(loaded.ui.theme, "light")
            self.assertTrue(loaded.ui.sounds)
            self.assertFalse(loaded.ui.live_preview)
            self.assertEqual(loaded.ui.preview_color, "#53cc88")
            text = path.read_text(encoding="utf-8")
            self.assertIn('# user comment\n[llm]\nendpoint = "local"\n', text)
            self.assertIn("# keep this note\n", text)
            with self.assertRaises(ValueError):
                Config.save_user({"hotkeys": {"push": "not a shortcut"}}, path)
            self.assertEqual(path.read_text(encoding="utf-8"), text)
            with self.assertRaisesRegex(ValueError, "must be distinct"):
                Config.save_user({"hotkeys": {"push": "control+option+t"}}, path)
            self.assertEqual(path.read_text(encoding="utf-8"), text)
            for invalid in ("green", "#abc", "#ffffff; color:red", 7):
                with self.subTest(invalid=invalid), self.assertRaisesRegex(
                        ValueError, "ui.preview_color"):
                    Config.save_user({"ui": {"preview_color": invalid}}, path)
                self.assertEqual(path.read_text(encoding="utf-8"), text)

    def test_invalid_preview_color_is_rejected_when_loading_user_configuration(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "config.toml"
            path.write_text('[ui]\npreview_color = "not a hex color"\n',
                            encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "ui.preview_color"):
                Config.load(path)


    def test_session_guardrails_reject_unbounded_or_immediate_close(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "config.toml"
            for section, key, value in (
                    ("dictation", "session_max_s", "0"),
                    ("dictation", "session_max_s", "inf"),
                    ("turn", "max_sec", "-1")):
                with self.subTest(setting=f"{section}.{key}"):
                    path.write_text(f"[{section}]\n{key} = {value}\n")
                    with self.assertRaisesRegex(ValueError, "finite positive"):
                        Config.load(path)
            path.write_text("[dictation]\nsession_max_s = 7200\n")
            self.assertEqual(Config.load(path).dictation.session_max_s, 7200)



class KeywordTests(unittest.TestCase):
    def test_default_model_phrases_compile_with_stable_labels(self):
        cfg = Config()
        self.assertEqual((cfg.kws.wake_phrase, cfg.kws.end_phrase),
                         ("hey voice", "voice send"))
        with tempfile.TemporaryDirectory() as d:
            model = Path(d)
            (model / "en.phone").write_text(
                "HEY HH EY1\nVOICE V OY1 S\nSEND S EH1 N D\n",
                encoding="utf-8")
            expected = ("HH EY1 V OY1 S @WAKE\n"
                        "V OY1 S S EH1 N D @SEND\n")
            self.assertEqual(keyword_lines(model, cfg.kws.wake_phrase,
                                           cfg.kws.end_phrase), expected)

            def fake_spotter(**kwargs):
                self.assertEqual(Path(kwargs["keywords_file"]).read_text(), expected)
                return types.SimpleNamespace(create_stream=lambda: object())

            with patch.dict("sys.modules", {"sherpa_onnx":
                            types.SimpleNamespace(KeywordSpotter=fake_spotter)}):
                kws = KWS(model, cfg.kws.wake_phrase, cfg.kws.end_phrase)
            self.assertIsNotNone(kws.spotter)
            detections = iter(("@WAKE", "SEND"))
            ready = iter((True, True, False))
            kws._stream = types.SimpleNamespace(accept_waveform=lambda *args: None)
            kws.spotter = types.SimpleNamespace(
                is_ready=lambda stream: next(ready),
                decode_stream=lambda stream: None,
                get_result=lambda stream: next(detections),
                reset_stream=lambda stream: None)
            self.assertEqual(kws.accept([]), ["WAKE", "SEND"])

    def test_unsupported_word_is_actionable(self):
        with tempfile.TemporaryDirectory() as d:
            model = Path(d)
            (model / "en.phone").write_text("HEY HH EY1\nSEND S EH1 N D\n",
                                            encoding="utf-8")
            with self.assertRaisesRegex(ValueError, r"unsupported word.*zorp.*en.phone"):
                keyword_lines(model, "hey zorp", "send")
            with self.assertRaisesRegex(ValueError, "invalid words"):
                keyword_lines(model, "hey!", "send")
            with self.assertRaisesRegex(ValueError, "same pronunciation"):
                keyword_lines(model, "hey", "hey")


class GnomeOwnershipTests(unittest.TestCase):
    def test_install_only_adds_owned_path_and_preserves_other_binding(self):
        others = ["/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/user0/"]
        calls = []

        def gsettings(*args):
            calls.append(args)
            if args[0] == "get":
                return json.dumps(others)
            return ""

        with patch.object(control, "_gsettings", side_effect=gsettings):
            self.assertIsNone(control.install_hotkey("ctrl+alt+space"))
        self.assertEqual(calls[-1], ("set", control._SCHEMA, "custom-keybindings",
                                     json.dumps(others + [control._KEYS_PATH])))
        self.assertEqual([args for args in calls if args[:2] == ("set", control._SCHEMA)],
                         [calls[-1]])
        self.assertTrue(any(args[2:] == ("binding", "<Control><Alt>space")
                            for args in calls if len(args) == 4))

    def test_uninstall_removes_only_owned_entries(self):
        others = ["/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/user0/"]
        calls = []

        def gsettings(*args):
            calls.append(args)
            if args[0] == "get":
                return json.dumps([control._KEYS_PATH, *others,
                                   control._TOGGLE_KEYS_PATH, control._CANCEL_KEYS_PATH])
            return ""

        with patch.object(control, "_gsettings", side_effect=gsettings):
            self.assertIsNone(control.uninstall_hotkey())
        self.assertEqual(calls[-1], ("set", control._SCHEMA, "custom-keybindings",
                                     json.dumps(others)))

    def test_existing_path_updates_binding_without_replacing_list(self):
        with patch.object(control, "_gsettings", side_effect=lambda *args:
                          json.dumps([control._KEYS_PATH]) if args[0] == "get" else "") as gs:
            self.assertIsNone(control.install_hotkey("ctrl+alt+t"))
        self.assertFalse(any(args[0] == "set" and args[2] == "custom-keybindings"
                             for args, _ in (call for call in gs.call_args_list)))

    def test_fifo_writer_never_blocks_without_daemon(self):
        with patch.object(control.os, "open", side_effect=OSError("no reader")) as opener:
            self.assertFalse(control.write("push"))
        self.assertEqual(opener.call_args.args[1],
                         control.os.O_WRONLY | control.os.O_NONBLOCK)


if __name__ == "__main__":
    unittest.main()
