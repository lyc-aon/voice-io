"""Optional OpenAI-compatible endpoint for transcript polish."""

from __future__ import annotations

import json
import threading
import urllib.error
import urllib.request
from dataclasses import dataclass

from voice_io.config import Config

_TIMEOUT_PROBE = 1.5
_TIMEOUT_INFER = 12.0


@dataclass
class Endpoint:
    base: str  # explicit config endpoint
    model: str  # model id for chat completions


def _post_json(url: str, payload: dict, headers: dict | None = None,
               timeout: float = _TIMEOUT_INFER) -> dict:
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", **(headers or {})})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read())


def _get_json(url: str, headers: dict | None = None,
              timeout: float = _TIMEOUT_PROBE) -> dict | None:
    req = urllib.request.Request(url, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read())
    except (urllib.error.URLError, TimeoutError, OSError):
        return None


def _models(endpoint: str, headers: dict | None = None) -> list[dict]:
    data = _get_json(endpoint.rstrip("/") + "/v1/models", headers)
    return (data or {}).get("data", []) or []


def _pick_model(models: list[dict], loaded_only: bool = True) -> str | None:
    """Largest (by size/n_params if advertised, else first)."""
    if not models:
        return None
    cands = [m for m in models
             if (m.get("status") or "loaded") == "loaded"] if loaded_only \
        else models
    if not cands:
        cands = models
    def key(m: dict):
        return (m.get("size") or m.get("n_params") or m.get("params") or 0,
                len(m.get("id", "")))
    return sorted(cands, key=key)[-1].get("id")




class LLM:
    def __init__(self, config: Config):
        self._cfg = config.llm
        self._endpoint: Endpoint | None = None
        self._lock = threading.Lock()

    # ---- configured endpoint ----

    def resolve(self) -> Endpoint | None:
        if self._endpoint is not None:
            return self._endpoint
        with self._lock:
            if self._endpoint is None:
                self._endpoint = self._discover()
        return self._endpoint

    def _discover(self) -> Endpoint | None:
        if not self._cfg.endpoint:
            return None
        models = _models(self._cfg.endpoint)
        model = self._cfg.model or _pick_model(models)
        if models or self._cfg.model:
            return Endpoint(self._cfg.endpoint.rstrip("/"), model or "")
        return None


    @staticmethod
    def _chat(ep: Endpoint, payload: dict) -> dict | None:
        try:
            return _post_json(ep.base + "/v1/chat/completions", payload)
        except (urllib.error.URLError, TimeoutError, OSError,
                json.JSONDecodeError):
            return None

    def complete(self, system: str, user: str, max_tokens: int = 600) -> str | None:
        """One-shot polish completion; returns text or None on failure."""
        ep = self.resolve()
        if ep is None:
            return None
        payload = {
            "model": ep.model or "llama",
            "temperature": 0.2,
            "max_tokens": max_tokens,
            "messages": [
                {"role": "system", "content": system},
                {"role": "user", "content": user},
            ],
        }
        resp = self._chat(ep, payload)
        if resp is None:
            return None
        try:
            content = resp["choices"][0]["message"].get("content")
        except (KeyError, IndexError):
            return None
        return content if isinstance(content, str) and content else None

