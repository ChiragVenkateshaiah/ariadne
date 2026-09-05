"""The one seam between Ariadne's deterministic graph/gRPC plumbing and an
actual language model. Every LLM-touching component (workflow synthesis,
evidence correlation, adjudication, the intent resolver) calls through this
interface and never imports `anthropic` directly -- that is what makes the
mock swap-in below possible with zero changes anywhere else.

Two implementations:
  - AnthropicClient: the real thing, lazily constructed so importing this
    module never requires an API key to be present.
  - MockLLMClient: returns fixture responses keyed by `purpose`. Not a
    generic stub -- each fixture is the answer a competent engineer (or a
    real Claude call) would actually give for THIS SUT, because we know its
    business logic. This lets every downstream component (synth.py first,
    then correlate.py and adjudicate.py) be built and demoed end-to-end
    before a real API key exists, and swapping in AnthropicClient later
    should change answers only in polish, not in shape.

Every real call is cached to disk keyed by (purpose, prompt-hash) -- the
record/replay cache docs/ARCHITECTURE.md calls non-negotiable, because live
LLM latency during a demo is a self-inflicted wound.
"""

from __future__ import annotations

import hashlib
import json
import os
from abc import ABC, abstractmethod
from pathlib import Path
from typing import Any

DEFAULT_MODEL = os.environ.get("ARIADNE_LLM_MODEL", "claude-sonnet-5")
CACHE_DIR = Path(os.environ.get("ARIADNE_LLM_CACHE_DIR", "/tmp/ariadne-llm-cache"))


class LLMClient(ABC):
    @abstractmethod
    def complete_json(self, purpose: str, system: str, prompt: str) -> dict[str, Any]:
        """Sends `prompt` (with `system` as the system prompt) and returns the
        response parsed as JSON. `purpose` is a short stable tag (e.g.
        "synth_workflows", "adjudicate_failure") used for cache keys and, in
        the mock, fixture lookup -- it is never sent to the model."""


class AnthropicClient(LLMClient):
    def __init__(self, model: str = DEFAULT_MODEL, api_key: str | None = None):
        self._model = model
        self._api_key = api_key or os.environ.get("ANTHROPIC_API_KEY")
        if not self._api_key:
            raise RuntimeError(
                "AnthropicClient requires ANTHROPIC_API_KEY. Use get_llm_client() "
                "instead of constructing this directly -- it falls back to "
                "MockLLMClient automatically when no key is set."
            )
        self._client = None  # constructed lazily on first use

    def _sdk(self):
        if self._client is None:
            import anthropic
            self._client = anthropic.Anthropic(api_key=self._api_key)
        return self._client

    def complete_json(self, purpose: str, system: str, prompt: str) -> dict[str, Any]:
        cached = _cache_get(purpose, system, prompt)
        if cached is not None:
            return cached

        response = self._sdk().messages.create(
            model=self._model,
            max_tokens=4096,
            system=system,
            messages=[{"role": "user", "content": prompt}],
        )
        text = "".join(block.text for block in response.content if block.type == "text")
        parsed = _parse_json_response(text)
        _cache_put(purpose, system, prompt, parsed)
        return parsed


class MockLLMClient(LLMClient):
    """Returns a fixture for `purpose`. A fixture is either a static dict
    (used verbatim, ignoring the actual prompt -- appropriate when there is
    one fixed correct answer, e.g. synth_workflows) or a callable
    `(system, prompt) -> dict` (appropriate when the correct answer genuinely
    depends on live, per-call content the model would have to read -- e.g.
    resolve_intent, where the right element index depends on whatever DOM
    snapshot this particular page load produced). Raises loudly for an
    unknown purpose rather than returning something plausible-looking -- a
    silent wrong answer here would be far more confusing to debug than an
    explicit "no fixture for this yet"."""

    def __init__(self, fixtures: dict[str, Any] | None = None):
        self._fixtures = fixtures or {}

    def register(self, purpose: str, response: Any) -> None:
        self._fixtures[purpose] = response

    def complete_json(self, purpose: str, system: str, prompt: str) -> dict[str, Any]:
        if purpose not in self._fixtures:
            raise KeyError(
                f"MockLLMClient has no fixture registered for purpose={purpose!r}. "
                "Register one (see ariadne.llm.fixtures) or set ANTHROPIC_API_KEY "
                "to use the real client."
            )
        fixture = self._fixtures[purpose]
        return fixture(system, prompt) if callable(fixture) else fixture


def get_llm_client() -> LLMClient:
    """The one place callers should construct a client. Prefers the real
    Anthropic client; falls back to the mock (with registered fixtures) when
    no API key is configured, so the rest of the codebase never has to
    branch on whether a key is present."""
    if os.environ.get("ANTHROPIC_API_KEY"):
        return AnthropicClient()
    from ariadne.llm.fixtures import default_mock_client
    return default_mock_client()


def _parse_json_response(text: str) -> dict[str, Any]:
    text = text.strip()
    if text.startswith("```"):
        text = text.split("\n", 1)[1] if "\n" in text else text
        if text.endswith("```"):
            text = text.rsplit("```", 1)[0]
    return json.loads(text.strip())


def _cache_key(purpose: str, system: str, prompt: str) -> str:
    h = hashlib.sha256(f"{purpose}\0{system}\0{prompt}".encode()).hexdigest()[:24]
    return f"{purpose}-{h}"


def _cache_get(purpose: str, system: str, prompt: str) -> dict[str, Any] | None:
    path = CACHE_DIR / f"{_cache_key(purpose, system, prompt)}.json"
    if not path.exists():
        return None
    return json.loads(path.read_text())


def _cache_put(purpose: str, system: str, prompt: str, value: dict[str, Any]) -> None:
    CACHE_DIR.mkdir(parents=True, exist_ok=True)
    path = CACHE_DIR / f"{_cache_key(purpose, system, prompt)}.json"
    path.write_text(json.dumps(value, indent=2))
