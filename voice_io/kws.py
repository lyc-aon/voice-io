"""English open-vocabulary KWS: compile user phrases with the model's en.phone.

Each keyword line is `<phonemes> @WAKE` or `<phonemes> @SEND`.
"""

from __future__ import annotations

import re
import tempfile
from pathlib import Path


_CHUNK = "chunk-16"  # 320 ms window


def keyword_lines(model_dir: Path, wake_phrase: str, end_phrase: str) -> str:
    """Compile only the requested words using the model's English lexicon."""
    lexicon = Path(model_dir) / "en.phone"

    def words(phrase: str, label: str) -> list[str]:
        if not isinstance(phrase, str) or not phrase.strip():
            raise ValueError(f"{label.lower()} phrase must contain English words")
        parts = phrase.strip().split()
        if any(not re.fullmatch(r"[A-Za-z]+(?:['-][A-Za-z]+)*", word)
               for word in parts):
            raise ValueError(f"{label.lower()} phrase {phrase!r} contains invalid words; "
                             "use space-separated English words from en.phone")
        return [word.upper() for word in parts]

    wake_words = words(wake_phrase, "WAKE")
    end_words = words(end_phrase, "SEND")
    wanted = set(wake_words + end_words)
    entries: dict[str, str] = {}
    with lexicon.open(encoding="utf-8") as f:
        for line in f:
            word, _, pronunciation = line.strip().partition(" ")
            if word in wanted and pronunciation and word not in entries:
                entries[word] = pronunciation
                if len(entries) == len(wanted):
                    break

    def phones(phrase_words: list[str], label: str) -> str:
        missing = [word.lower() for word in phrase_words if word not in entries]
        if missing:
            raise ValueError(f"{label.lower()} phrase has unsupported word(s): "
                             f"{', '.join(missing)}; choose words in {lexicon}")
        return " ".join(entries[word] for word in phrase_words) + f" @{label}"

    wake = phones(wake_words, "WAKE")
    end = phones(end_words, "SEND")
    if wake.rsplit(" @", 1)[0] == end.rsplit(" @", 1)[0]:
        raise ValueError("Wake and end phrases have the same pronunciation; "
                         "choose different phrases")
    return wake + "\n" + end + "\n"


class KWS:
    def __init__(self, model_dir: Path, wake_phrase: str, end_phrase: str, *,
                 chunk: str = _CHUNK, score: float = 1.0,
                 threshold: float = 0.25, threads: int = 4):
        m = Path(model_dir)
        keywords = keyword_lines(m, wake_phrase, end_phrase)
        import sherpa_onnx
        with tempfile.TemporaryDirectory(prefix="voice-io-kws-") as tmp:
            keyword_path = Path(tmp) / "keywords.txt"
            keyword_path.write_text(keywords, encoding="utf-8")
            self.spotter = sherpa_onnx.KeywordSpotter(
                tokens=str(m / "tokens.txt"),
                encoder=str(m / f"encoder-epoch-13-avg-2-{chunk}-left-64.int8.onnx"),
                decoder=str(m / f"decoder-epoch-13-avg-2-{chunk}-left-64.onnx"),
                joiner=str(m / f"joiner-epoch-13-avg-2-{chunk}-left-64.int8.onnx"),
                num_threads=threads,
                keywords_file=str(keyword_path),
                keywords_score=score,
                keywords_threshold=threshold,
                provider="cpu",
            )
            self._stream = self.spotter.create_stream()

    def accept(self, samples) -> list[str]:
        """Feed float32 samples; return keyword labels detected (0-1)."""
        self._stream.accept_waveform(16000, samples)
        dets = []
        while self.spotter.is_ready(self._stream):
            self.spotter.decode_stream(self._stream)
            r = self.spotter.get_result(self._stream)
            if r != "":
                label = r.removeprefix("@")
                if label not in ("WAKE", "SEND"):
                    raise ValueError(f"Unexpected KWS label {r!r}; expected WAKE or SEND")
                dets.append(label)
                self.spotter.reset_stream(self._stream)
        return dets
