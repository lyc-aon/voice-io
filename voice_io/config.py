"""Configuration: built-in defaults + optional ~/.config/voice-io/config.toml.

TOML keys mirror the dataclasses below (e.g. [vad] threshold = 0.5).
Portable hotkeys use modifier+key strings, e.g. ctrl+alt+space.
"""

from __future__ import annotations

import json
import math
import os
import re
import tempfile
import tomllib
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parent.parent
CONFIG_PATH = Path(
    os.environ.get("VOICE_IO_CONFIG",
                   Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))
                   / "voice-io" / "config.toml")
)

_MODIFIERS = {"ctrl": "ctrl", "control": "ctrl", "alt": "alt",
              "option": "alt", "shift": "shift", "super": "super",
              "cmd": "super", "command": "super", "meta": "super"}
_KEY_NAMES = {"space", "enter", "tab", "escape", "backspace", "delete",
              "up", "down", "left", "right", "home", "end", "pageup",
              "pagedown", "insert"}
_KEY_NAMES.update(f"f{n}" for n in range(1, 25))


def parse_hotkey(binding: str) -> tuple[frozenset[str], str]:
    """Parse a portable modifier+key shortcut, independent of the desktop.

    ctrl/alt/shift/super are the canonical modifiers; aliases control,
    option, cmd/command and meta are accepted. Keys are a-z, 0-9,
    F1-F24, or a name from _KEY_NAMES. A modifier is required.
    """
    if not isinstance(binding, str):
        raise ValueError("Hotkey must be a modifier+key string, e.g. ctrl+alt+space")
    parts = [part.strip().lower() for part in binding.split("+")]
    if len(parts) < 2 or any(not part for part in parts):
        raise ValueError(f"Invalid hotkey {binding!r}; expected ctrl+alt+space")
    modifiers = [_MODIFIERS.get(part) for part in parts[:-1]]
    if any(mod is None for mod in modifiers) or len(set(modifiers)) != len(modifiers):
        raise ValueError(f"Invalid hotkey modifiers in {binding!r}")
    key = parts[-1]
    if not (re.fullmatch(r"[a-z0-9]", key) or key in _KEY_NAMES):
        raise ValueError(f"Unsupported hotkey key {key!r} in {binding!r}")
    return frozenset(modifiers), key


def _toml_value(value: object) -> str:
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (int, float)):
        return str(value)
    raise TypeError(f"Unsupported config value: {value!r}")


def _merge_sections(text: str, updates: dict[str, dict[str, object]]) -> str:
    """Replace only requested TOML keys, preserving all other text/sections."""
    pending = {section: dict(values) for section, values in updates.items()}
    out: list[str] = []
    section: str | None = None

    def add_remaining() -> None:
        if section in pending:
            values = pending.pop(section)
            if values and out and not out[-1].endswith("\n"):
                out.append("\n")
            for key, value in values.items():
                out.append(f"{key} = {_toml_value(value)}\n")

    for line in text.splitlines(keepends=True):
        heading = re.fullmatch(r"\s*\[([A-Za-z0-9_-]+)\]\s*(?:#.*)?", line.strip())
        if heading:
            add_remaining()
            section = heading.group(1)
        match = re.match(r"^(\s*)([A-Za-z0-9_-]+)(\s*=)", line)
        if match and section in pending and match.group(2) in pending[section]:
            key = match.group(2)
            out.append(f"{match.group(1)}{key}{match.group(3)} {_toml_value(pending[section].pop(key))}\n")
        else:
            out.append(line)
    add_remaining()
    for name, values in pending.items():
        if values:
            if out and not out[-1].endswith("\n"):
                out.append("\n")
            out.append(f"\n[{name}]\n")
            out.extend(f"{key} = {_toml_value(value)}\n" for key, value in values.items())
    return "".join(out)


@dataclass
class VadCfg:
    model: str = "models/silero_vad_v5.onnx"
    threshold: float = 0.5
    # End a speech turn after half a second of silence; a turn boundary does
    # not close an open dictation session.
    min_silence_duration: float = 0.5
    # VAD ceiling per utterance (sherpa rule3): a 20s+ turn is closed and a
    # new one opens. Safety, not a commit clock.
    max_speech_duration: float = 20.0


@dataclass
class KwsCfg:
    model_dir: str = "models/sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20"
    chunk: str = "chunk-16"  # chunk-16 = 320ms window
    score: float = 1.0
    wake_phrase: str = "hey voice"
    end_phrase: str = "voice send"
    threshold: float = 0.25


@dataclass
class AsrCfg:
    streaming: str = "models/sherpa-onnx-x-asr-480ms-streaming-zipformer-transducer-zh-en-int8-2026-06-05"
    final_en: str = "models/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8"
    threads: int = 4


@dataclass
class LlmCfg:
    # Optional OpenAI-compatible endpoint for dictation polish.
    endpoint: str = ""
    model: str = ""


@dataclass
class ExecCfg:
    # Optional submit for a normal Stop/push ("" = off, "enter", "ctrl+enter").
    # Only the explicit spoken end phrase sends Enter; otherwise optional.
    auto_submit: str = ""


@dataclass
class PolishCfg:
    # Deterministic cleanup (custom words / filler / emoji) before any paste.
    deterministic: bool = True
    filler: bool = True
    emoji: bool = True
    # Optional guarded LLM polish runs once on the full session after
    # deterministic cleanup, before its single final paste.
    enabled: bool = True
    custom_words: str = ""  # JSON {"misheard": "correct"} applied post-ASR
    snippets: str = ""      # JSON {"keyword": "exact phrase"} — never polished


@dataclass
class TurnCfg:
    # Silence closes a turn; this per-utterance ceiling limits decoder input.
    max_sec: float = 20.0
    front_drop: float = 0.1  # exclude wake-tail overlap at stream open (s)
    tail_s: float = 1.0      # streaming ASR flush tail for fallback (s)



@dataclass
class DictationCfg:
    history: str = ""  # JSONL path; "" = ~/.local/share/voice-io/history.jsonl
    recovery_dir: str = ""  # "" = ~/.local/share/voice-io/recovery
    recovery_ttl: float = 86400.0  # escape-recovery window (24h)
    session_max_s: float = 3600.0  # safety: close an open session after 1h


@dataclass
class MeetingCfg:
    mode: str = "dictation"  # dictation | meeting | collaborative; selected in Settings
    summary_endpoint: str = ""  # OpenAI-compatible base URL, e.g. http://localhost:11434/v1
    summary_model: str = ""  # empty endpoint/model = transcript only
    summary_api_key_env: str = ""  # environment variable NAME, never its secret value
    segmentation_model: str = "models/sherpa-onnx-pyannote-segmentation-3-0/model.int8.onnx"
    embedding_model: str = "models/nemo_en_titanet_small.onnx"


def _validate_meeting(meeting: MeetingCfg) -> None:
    if meeting.mode not in ("dictation", "meeting", "collaborative"):
        raise ValueError("meeting.mode must be 'dictation', 'meeting', or 'collaborative'")
    if not all(isinstance(v, str) for v in (
            meeting.summary_endpoint, meeting.summary_model,
            meeting.summary_api_key_env, meeting.segmentation_model,
            meeting.embedding_model)):
        raise ValueError("meeting settings must be strings")
    if meeting.summary_api_key_env and not re.fullmatch(
            r"[A-Za-z_][A-Za-z0-9_]*", meeting.summary_api_key_env):
        raise ValueError("meeting.summary_api_key_env must name an environment variable")
    if meeting.summary_endpoint:
        url = urlsplit(meeting.summary_endpoint)
        if (url.scheme not in ("http", "https") or not url.netloc
                or url.username or url.password or url.query or url.fragment):
            raise ValueError("meeting.summary_endpoint must be an HTTP(S) base URL without credentials")


@dataclass
class UiCfg:
    live_preview: bool = True  # brief three-word side preview, not inside pill
    preview_color: str = ""  # empty keeps each platform's quiet preview tint
    sounds: bool = True        # boundary cues (listening/idle/input)
    pill: str = "pill"
    theme: str = "auto"
    accent: str = "#4f8cff"


def _validate_preview_color(value: object) -> None:
    if not isinstance(value, str) or (
            value and not re.fullmatch(r"#[0-9a-fA-F]{6}", value)):
        raise ValueError("ui.preview_color must be #RRGGBB or empty for the default")


@dataclass
class HotkeyCfg:
    push: str = "ctrl+alt+space"
    toggle: str = "ctrl+alt+t"
    cancel: str = "ctrl+alt+x"


@dataclass
class Config:
    vad: VadCfg = field(default_factory=VadCfg)
    kws: KwsCfg = field(default_factory=KwsCfg)
    asr: AsrCfg = field(default_factory=AsrCfg)
    llm: LlmCfg = field(default_factory=LlmCfg)
    exec: ExecCfg = field(default_factory=ExecCfg)
    polish: PolishCfg = field(default_factory=PolishCfg)
    turn: TurnCfg = field(default_factory=TurnCfg)
    dictation: DictationCfg = field(default_factory=DictationCfg)
    meeting: MeetingCfg = field(default_factory=MeetingCfg)
    ui: UiCfg = field(default_factory=UiCfg)
    hotkeys: HotkeyCfg = field(default_factory=HotkeyCfg)
    overlay_enabled: bool = True

    def resolve(self, p: str) -> Path:
        """Resolve models via the user model store; other paths via repo root."""
        path = Path(p)
        if path.is_absolute():
            return path
        if path.parts and path.parts[0] == "models":
            from voice_io.model_store import resolve_model_path
            return resolve_model_path(p)
        return ROOT / path

    @classmethod
    def save_user(cls, updates: dict[str, dict[str, object]],
                  path: Path | None = None) -> None:
        """Persist selected TOML settings without touching unrelated keys.

        Settings UIs pass e.g. {'hotkeys': {'push': 'ctrl+alt+space'}}.
        The daemon loads settings; merely loading never changes GNOME bindings.
        Applying shortcuts is an explicit call to control.apply_hotkeys().
        """
        allowed = {"hotkeys": HotkeyCfg, "kws": KwsCfg, "ui": UiCfg,
                   "meeting": MeetingCfg}
        for section, values in updates.items():
            if section not in allowed or not isinstance(values, dict):
                raise ValueError(f"Unsupported config section: {section}")
            for key, value in values.items():
                if key not in allowed[section].__dataclass_fields__:
                    raise ValueError(f"Unsupported config key: {section}.{key}")
                if section == "hotkeys":
                    if not isinstance(value, str):
                        raise ValueError(f"{section}.{key} must be a hotkey string")
                    parse_hotkey(value)
                if section == "kws" and key in ("wake_phrase", "end_phrase"):
                    if not isinstance(value, str) or not value.strip():
                        raise ValueError(f"{section}.{key} must be a nonempty phrase")
                if section == "ui" and key == "preview_color":
                    _validate_preview_color(value)
        p = Path(path) if path is not None else CONFIG_PATH
        text = p.read_text(encoding="utf-8") if p.exists() else ""
        merged = _merge_sections(text, updates)
        # A malformed existing config must not be silently overwritten.
        parsed = tomllib.loads(merged)
        shortcuts = parsed.get("hotkeys", {})
        if shortcuts:
            defaults = HotkeyCfg()
            bindings = [parse_hotkey(shortcuts.get(key, getattr(defaults, key)))
                        for key in ("push", "toggle", "cancel")]
            if len(set(bindings)) != len(bindings):
                raise ValueError("Push, toggle, and cancel shortcuts must be distinct")
        settings = MeetingCfg()
        for key, value in parsed.get("meeting", {}).items():
            if hasattr(settings, key):
                setattr(settings, key, value)
        _validate_meeting(settings)
        p.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=p.parent,
                                         prefix=".config-", delete=False) as f:
            tmp = Path(f.name)
            try:
                f.write(merged)
                f.flush()
                os.fsync(f.fileno())
            except BaseException:
                tmp.unlink(missing_ok=True)
                raise
        try:
            os.replace(tmp, p)
        except BaseException:
            tmp.unlink(missing_ok=True)
            raise

    @classmethod
    def load(cls, path: Path | None = None) -> "Config":
        cfg = cls()
        p = path or CONFIG_PATH
        if not p.exists():
            return cfg
        with open(p, "rb") as f:
            data = tomllib.load(f)
        for name, comp in (("vad", cfg.vad), ("kws", cfg.kws),
                           ("asr", cfg.asr), ("llm", cfg.llm),
                           ("exec", cfg.exec), ("polish", cfg.polish),
                           ("turn", cfg.turn), ("dictation", cfg.dictation),
                           ("meeting", cfg.meeting),
                           ("ui", cfg.ui), ("hotkeys", cfg.hotkeys)):
            section = data.get(name, {})
            for k, v in section.items():
                if hasattr(comp, k):
                    setattr(comp, k, v)
        for name, value in (("dictation.session_max_s", cfg.dictation.session_max_s),
                            ("turn.max_sec", cfg.turn.max_sec)):
            if (isinstance(value, bool) or not isinstance(value, (int, float))
                    or not math.isfinite(value) or value <= 0):
                raise ValueError(f"{name} must be a finite positive number of seconds")
        _validate_meeting(cfg.meeting)
        _validate_preview_color(cfg.ui.preview_color)
        if "overlay" in data:
            cfg.overlay_enabled = bool(data["overlay"].get("enabled", True))
        return cfg
