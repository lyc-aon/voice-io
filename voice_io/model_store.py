"""User-installed model paths with a local development checkout fallback."""

from __future__ import annotations

import os
import sys
from pathlib import Path, PurePosixPath

_REPO_MODELS = Path(__file__).resolve().parent.parent / "models"


def model_root() -> Path:
    """Return the per-user model directory; no directory is created by this call."""
    if sys.platform == "darwin":
        return Path.home() / "Library" / "Application Support" / "voice-io" / "models"
    return Path(os.environ.get("XDG_DATA_HOME", Path.home() / ".local" / "share")) / "voice-io" / "models"


def resolve_model_path(relative: str) -> Path:
    """Resolve a `models/...` path, preferring a *complete* local checkout."""
    if not isinstance(relative, str):
        raise ValueError(f"expected a relative models/... path, got {relative!r}")
    path = PurePosixPath(relative)
    if (path.is_absolute() or len(path.parts) < 2 or path.parts[0] != "models"
            or ".." in path.parts or "\\" in relative):
        raise ValueError(f"expected a relative models/... path, got {relative!r}")
    local = _REPO_MODELS.joinpath(*path.parts[1:])
    installed = model_root().joinpath(*path.parts[1:])
    from voice_io.install_models import MEETING_MODELS, MODELS, missing_files
    for model in MODELS + MEETING_MODELS:
        if path.parts[1] == model.relative:
            if not missing_files(model, _REPO_MODELS / model.relative):
                return local
            return installed
    return local if local.exists() else installed
