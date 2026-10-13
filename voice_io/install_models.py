"""Pinned, validated model installation; model weights are never packaged.

    install_models(*, meeting=False, downloader=None) -> dict[str, Path]
    verify_models(*, meeting=False) -> dict[str, Path]

`downloader(url, destination)` is injectable for an offline transport; it must
write the entire response to destination or raise. Default transport streams to
a temporary file. GitHub releases are used for KWS, x-ASR, and optional
meeting models because their sherpa-onnx exports are not hosted as usable files
on Hugging Face.
"""

from __future__ import annotations

import hashlib
import os
import shutil
import tarfile
import tempfile
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from voice_io import model_store


@dataclass(frozen=True)
class Model:
    name: str
    relative: str
    required: tuple[str, ...]
    repo: str
    revision: str
    sha256: str = ""
    archive: bool = False
    release: bool = False

    @property
    def directory(self) -> bool:
        return not self.relative.endswith(".onnx")


_KWS = "sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20"
_STREAM = "sherpa-onnx-x-asr-480ms-streaming-zipformer-transducer-zh-en-int8-2026-06-05"
_FINAL = "sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8"
MODELS = (
    Model("vad", "silero_vad_v5.onnx", ("silero_vad_v5.onnx",), "csukuangfj/vad",
          "fba88cd2e921609e7675c3aaf51e0b9b295da4bc"),
    Model("kws", _KWS, ("tokens.txt", "en.phone",
          "encoder-epoch-13-avg-2-chunk-16-left-64.int8.onnx",
          "decoder-epoch-13-avg-2-chunk-16-left-64.onnx",
          "joiner-epoch-13-avg-2-chunk-16-left-64.int8.onnx",
          "encoder-epoch-13-avg-2-chunk-8-left-64.int8.onnx",
          "decoder-epoch-13-avg-2-chunk-8-left-64.onnx",
          "joiner-epoch-13-avg-2-chunk-8-left-64.int8.onnx"),
          "k2-fsa/sherpa-onnx", "kws-models",
          "68447f4fbc67e70eee3a93961f36e81e98f47aef73ce7e7ca00885c6cd3616a6", True),
    Model("streaming", _STREAM,
          ("tokens.txt", "bpe.model", "encoder.int8.onnx", "decoder.onnx", "joiner.int8.onnx"),
          "k2-fsa/sherpa-onnx", "asr-models",
          "14919f03b74d9001fc8f2689c2ab1ea42c436db269e8c809144492f41612bfff", True),
    Model("final_en", _FINAL,
          ("tokens.txt", "encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx"),
          "csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8",
          "1ab9323565ddb038682214b292f588070a538ce2"),
)

MEETING_MODELS = (
    Model("segmentation", "sherpa-onnx-pyannote-segmentation-3-0",
          ("model.int8.onnx", "LICENSE"), "k2-fsa/sherpa-onnx",
          "speaker-segmentation-models",
          "24615ee884c897d9d2ba09bb4d30da6bb1b15e685065962db5b02e76e4996488",
          archive=True, release=True),
    Model("speaker_embedding", "nemo_en_titanet_small.onnx",
          ("nemo_en_titanet_small.onnx",), "k2-fsa/sherpa-onnx",
          "speaker-recongition-models",
          "ad4a1802485d8b34c722d2a9d04249662f2ece5d28a7a039063ca22f515a789e",
          release=True),
)


class ModelInstallError(RuntimeError):
    """Download, license, or integrity problem preventing a working model."""


def missing_files(model: Model, path: Path) -> tuple[str, ...]:
    """Report required files absent/empty at a candidate model path."""
    base = path if model.directory else path.parent
    return tuple(name for name in model.required
                 if not (base / name).is_file() or (base / name).stat().st_size == 0)


def _locations(model: Model) -> tuple[Path, Path]:
    return (model_store._REPO_MODELS / model.relative,
            model_store.model_root() / model.relative)


def verify_models(*, meeting: bool = False) -> dict[str, Path]:
    """Return validated installed/repo paths or raise with every missing file."""
    found = {}
    errors = []
    for model in MODELS + (MEETING_MODELS if meeting else ()):
        local, installed = _locations(model)
        if not missing_files(model, local):
            found[model.name] = local
        elif not missing_files(model, installed):
            found[model.name] = installed
        else:
            errors.append(f"{model.name}: missing {', '.join(missing_files(model, installed))} in {installed}")
    if errors:
        command = "voice-io models install --meeting" if meeting else "voice-io models install"
        raise ModelInstallError(f"Models incomplete; run `{command}`:\n" + "\n".join(errors))
    return found


def _download(url: str, destination: Path) -> None:
    request = urllib.request.Request(url, headers={"User-Agent": "voice-io-model-installer/0.1"})
    with urllib.request.urlopen(request, timeout=60) as response, destination.open("wb") as output:
        shutil.copyfileobj(response, output, length=1024 * 1024)


def _url(model: Model, filename: str = "") -> str:
    if model.archive or model.release:
        asset = model.relative + (".tar.bz2" if model.archive else "")
        return (f"https://github.com/{model.repo}/releases/download/"
                f"{model.revision}/{asset}")
    return f"https://huggingface.co/{model.repo}/resolve/{model.revision}/{filename}"


def _safe_unpack(archive: Path, model: Model, dest: Path) -> None:
    # Extract ONLY the explicitly required files. tarfile.extractall permits
    # traversal, symlink targets and device files in an untrusted archive.
    with tarfile.open(archive, "r:bz2") as tar:
        for member in tar:
            parts = member.name.split("/")
            if len(parts) != 2 or parts[0] != model.relative or parts[1] not in model.required:
                continue
            if not member.isfile():
                raise ModelInstallError(f"Unsafe archive entry {member.name!r} in {_url(model)}")
            source = tar.extractfile(member)
            if source is None:
                raise ModelInstallError(f"Cannot read {member.name!r} in {_url(model)}")
            with source, (dest / parts[1]).open("xb") as output:
                shutil.copyfileobj(source, output, length=1024 * 1024)


def _verify_sha256(path: Path, model: Model) -> None:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    if digest.hexdigest() != model.sha256:
        raise ModelInstallError(f"SHA-256 mismatch for {_url(model)}; download may be incomplete")

def _fetch(model: Model, staging: Path, downloader: Callable[[str, Path], None]) -> None:
    if model.archive:
        archive = staging.parent / (model.relative + ".tar.bz2")
        downloader(_url(model), archive)
        if not archive.is_file():
            raise ModelInstallError(f"Downloader did not create {archive}")
        _verify_sha256(archive, model)
        _safe_unpack(archive, model, staging)
    else:
        for filename in model.required:
            target = staging / filename if model.directory else staging
            downloader(_url(model, filename), target)
            if model.sha256:
                _verify_sha256(target, model)
    missing = missing_files(model, staging)
    if missing:
        raise ModelInstallError(f"{model.name}: download incomplete; missing/empty {', '.join(missing)}")


def install_models(*, meeting: bool = False,
                   downloader: Callable[[str, Path], None] | None = None) -> dict[str, Path]:
    """Install missing models into user data, atomically replacing incomplete installs.

    Complete repo checkout models remain untouched and work offline. Already
    complete user installs are left untouched. A failed transfer never publishes
    staged bytes as a valid model. Re-running safely retries an interrupted run.
    Meeting models are only included when explicitly requested.
    """
    fetch = downloader or _download
    found = {}
    for model in MODELS + (MEETING_MODELS if meeting else ()):
        local, target = _locations(model)
        if not missing_files(model, local):
            found[model.name] = local
            continue
        if not missing_files(model, target):
            found[model.name] = target
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix=".voice-io-model-", dir=target.parent) as temp:
            staging = Path(temp) / model.relative
            if model.directory:
                staging.mkdir()
            try:
                _fetch(model, staging, fetch)
            except urllib.error.HTTPError as exc:
                message = ("authorization/license acceptance required on the model page"
                           if exc.code in (401, 403) else "model file not found at the pinned revision"
                           if exc.code == 404 else "HTTP service error")
                raise ModelInstallError(f"{model.name}: {message}: {_url(model)} (HTTP {exc.code})") from exc
            except (urllib.error.URLError, TimeoutError, OSError) as exc:
                raise ModelInstallError(f"{model.name}: network/disk failure fetching {_url(model)}: {exc}") from exc
            except (tarfile.TarError, EOFError) as exc:
                raise ModelInstallError(f"{model.name}: corrupt model archive {_url(model)}: {exc}") from exc
            if target.exists() or target.is_symlink():
                # Preserve an existing incomplete target if the replacement fails.
                backup = Path(temp) / "previous"
                os.replace(target, backup)
                try:
                    os.replace(staging, target)
                except OSError:
                    os.replace(backup, target)
                    raise
            else:
                os.replace(staging, target)
        found[model.name] = target
    # No success report unless every required artifact can be located now.
    return verify_models(meeting=meeting)
