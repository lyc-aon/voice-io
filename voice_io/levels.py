"""Map mic RMS to a visible, bounded speech-meter amplitude."""

from __future__ import annotations

import math


def speech_level(rms: float) -> float:
    """A logarithmic display scale; input is already speech/noise gated."""
    if rms <= 0.0:
        return 0.0
    db = 20.0 * math.log10(rms)
    return min(1.0, max(0.0, (db + 70.0) / 42.0)) ** 0.8
