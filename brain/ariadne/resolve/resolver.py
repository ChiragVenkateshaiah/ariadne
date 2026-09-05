"""The Resolver: binds an IntentStep's natural-language target to a live
Playwright Locator, at runtime, against whatever DOM actually exists.

This is the mechanism self-healing is built on. Order of attempts, cheapest
and most durable first:

  1. Cached binding (intent_bindings table) -- if it still resolves to
     exactly one visible element, reuse it. This is the common case and
     costs zero reasoning.
  2. Deterministic heuristics, in BindingStrategy rank order (see model.py):
     role+accessible-name, then label, then placeholder, then text. These
     survive id/class renames because they bind on WHAT the element means to
     an assistive technology, not on incidental markup.
  3. LLM fallback, only when every heuristic fails to find exactly one
     match -- e.g. the element's visible text itself changed. The model sees
     a flattened list of interactive elements and picks one by index; we
     then re-derive the most semantic locator we can from ITS attributes,
     rather than trusting the model's own guess at a selector string.

Every successful resolution is written back to intent_bindings, superseding
whatever was cached there before -- see _bind()'s "healed_from" handling.
Nothing here decides whether that supersession was a legitimate heal or
should have been a regression; that adjudication happens one layer up (see
docs/ARCHITECTURE.md's Adjudicator, not yet built) using the reasoning and
confidence this module reports plus separately-collected evidence.
"""

from __future__ import annotations

import json
import re
import sqlite3
from dataclasses import dataclass
from datetime import datetime, timezone

from playwright.sync_api import Locator, Page

from ariadne.graph.model import BindingStrategy, StepAction
from ariadne.llm.client import LLMClient

_STOPWORDS = {"the", "a", "an", "input", "field", "button", "link", "enter", "select",
              "click", "submit", "this", "to", "of", "on", "and"}

_ACTION_ROLES: dict[StepAction, list[str]] = {
    StepAction.FILL: ["textbox", "combobox"],
    StepAction.SELECT: ["combobox"],
    StepAction.CLICK: ["button", "link"],
}

_INTERACTIVE_SELECTOR = "input, button, a, select, textarea"

_INTERACTIVE_ELEMENTS_JS = """
() => Array.from(document.querySelectorAll('input, button, a, select, textarea')).map((el, i) => ({
    index: i,
    tag: el.tagName.toLowerCase(),
    type: el.getAttribute('type'),
    id: el.id || null,
    name: el.getAttribute('name'),
    role: el.getAttribute('role'),
    aria_label: el.getAttribute('aria-label'),
    placeholder: el.getAttribute('placeholder'),
    text: (el.innerText || el.value || '').trim().slice(0, 80),
    testid: el.getAttribute('data-testid'),
}))
"""

RESOLVE_SYSTEM_PROMPT = """You are resolving a test step's intent to a specific \
element on a live web page. You are given the step's intent, its action, a \
natural-language hint about the target, and a flattened list of the page's \
interactive elements (each with an index). Pick the ONE element that best \
matches the intent -- element text or attributes may have changed since the \
test was written, so reason about MEANING, not just literal word overlap.

Respond with ONLY a JSON object: {"element_index": <int or null>, "reasoning": "..."}.
Use null only if truly no element matches the intent."""


class UnresolvableIntent(Exception):
    def __init__(self, intent: str, hint: str | None, reasoning: str = ""):
        msg = f"could not resolve intent={intent!r} hint={hint!r}"
        if reasoning:
            msg += f" ({reasoning})"
        super().__init__(msg)
        self.intent = intent
        self.hint = hint


@dataclass(slots=True)
class Resolution:
    locator: Locator
    strategy: BindingStrategy
    resolved_by: str  # "cache" | "heuristic" | a model id (e.g. "mock", "claude-sonnet-5")
    cache_value: str  # the string _apply_cached_strategy needs to rebuild this exact locator later
    healed: bool = False


def _keywords(text: str) -> set[str]:
    # Strip apostrophes before splitting so "offer's" becomes one token
    # ("offers") rather than fragmenting into "offer" + a noise "s" token.
    cleaned = text.lower().replace("'", "")
    return {w for w in re.findall(r"[a-z]+", cleaned) if len(w) > 1 and w not in _STOPWORDS}


def _name_regex(hint: str) -> re.Pattern:
    """Builds a regex requiring ALL of the hint's significant keywords to be
    present (in any order/position) -- deliberately AND, not OR. An
    alternation ("origin|airport") would match "Destination airport" too,
    since both labels share the word "airport"; a lookahead chain
    ("(?=.*origin)(?=.*airport)") only matches text containing every
    keyword, which is what actually disambiguates between sibling fields.
    """
    kws = _keywords(hint)
    if not kws:
        return re.compile(re.escape(hint), re.IGNORECASE)
    lookaheads = "".join(f"(?=.*{re.escape(k)})" for k in kws)
    return re.compile(lookaheads, re.IGNORECASE)


def _unique_visible(locator: Locator) -> bool:
    try:
        return locator.count() == 1 and locator.first.is_visible()
    except Exception:  # noqa: BLE001 -- Playwright raises several exception
        # types here (detached element, navigation mid-check, timeout); all
        # of them mean the same thing to a caller: this locator isn't
        # resolvable right now, so treat it as a plain miss, not a crash.
        return False


def _apply_cached_strategy(page: Page, strategy: BindingStrategy, value: str) -> Locator | None:
    try:
        if strategy == BindingStrategy.ROLE_NAME:
            # `name` here is already a regex pattern (cached verbatim from
            # _try_heuristics' name_re.pattern, an AND-lookahead chain) --
            # re.escape()-ing it, as an earlier version of this branch did,
            # turns the pattern into a literal string match that can never
            # succeed. LABEL/PLACEHOLDER/TEXT below never made this mistake;
            # ROLE_NAME must be compiled the same way they are.
            role, _, name = value.partition("::")
            return page.get_by_role(role, name=re.compile(name, re.IGNORECASE))
        if strategy == BindingStrategy.LABEL:
            return page.get_by_label(re.compile(value, re.IGNORECASE))
        if strategy == BindingStrategy.PLACEHOLDER:
            return page.get_by_placeholder(re.compile(value, re.IGNORECASE))
        if strategy == BindingStrategy.TEXT:
            return page.get_by_text(re.compile(value, re.IGNORECASE))
        if strategy == BindingStrategy.TEST_ID:
            return page.get_by_test_id(value)
        if strategy == BindingStrategy.CSS:
            if value.startswith("nth::"):
                return page.locator(_INTERACTIVE_SELECTOR).nth(int(value.removeprefix("nth::")))
            return page.locator(value)
    except Exception:  # noqa: BLE001 -- a cached locator string can fail to
        # reconstruct for many reasons (invalid regex from a corrupted
        # cache row, a role Playwright rejects); any of them just means
        # "cache miss, fall through to heuristics," not a caller-visible error.
        return None
    return None


def resolve(
    page: Page,
    conn: sqlite3.Connection,
    spec_id: str,
    ordinal: int,
    intent: str,
    action: StepAction,
    target_hint: str,
    llm: LLMClient | None = None,
) -> Resolution:
    if target_hint is None:
        raise ValueError(f"resolve() called for step {ordinal} with no target_hint -- caller bug")

    cached = _try_cached(page, conn, spec_id, ordinal)
    if cached is not None:
        _record_success(conn, spec_id, ordinal)
        return cached

    resolution = _try_heuristics(page, action, target_hint)
    if resolution is not None:
        _bind(conn, spec_id, ordinal, intent, resolution)
        return resolution

    if llm is None:
        raise UnresolvableIntent(intent, target_hint, "no heuristic matched and no LLM fallback configured")

    resolution = _resolve_with_llm(page, intent, action, target_hint, llm)
    if resolution is None:
        raise UnresolvableIntent(intent, target_hint, "heuristics and LLM fallback both found no match")
    _bind(conn, spec_id, ordinal, intent, resolution)
    return resolution


def _try_cached(page: Page, conn: sqlite3.Connection, spec_id: str, ordinal: int) -> Resolution | None:
    row = conn.execute(
        "SELECT strategy, locator FROM intent_bindings WHERE spec_id=? AND step_ordinal=? AND active=1",
        (spec_id, ordinal),
    ).fetchone()
    if row is None:
        return None
    strategy = BindingStrategy(row["strategy"])
    locator = _apply_cached_strategy(page, strategy, row["locator"])
    if locator is not None and _unique_visible(locator):
        return Resolution(locator=locator, strategy=strategy, resolved_by="cache", cache_value=row["locator"])
    return None


def _try_heuristics(page: Page, action: StepAction, target_hint: str) -> Resolution | None:
    name_re = _name_regex(target_hint)

    for role in _ACTION_ROLES.get(action, ["button", "link", "textbox"]):
        locator = page.get_by_role(role, name=name_re)
        if _unique_visible(locator):
            return Resolution(locator=locator, strategy=BindingStrategy.ROLE_NAME, resolved_by="heuristic",
                               cache_value=f"{role}::{name_re.pattern}")

    locator = page.get_by_label(name_re)
    if _unique_visible(locator):
        return Resolution(locator=locator, strategy=BindingStrategy.LABEL, resolved_by="heuristic",
                           cache_value=name_re.pattern)

    locator = page.get_by_placeholder(name_re)
    if _unique_visible(locator):
        return Resolution(locator=locator, strategy=BindingStrategy.PLACEHOLDER, resolved_by="heuristic",
                           cache_value=name_re.pattern)

    locator = page.get_by_text(name_re)
    if _unique_visible(locator):
        return Resolution(locator=locator, strategy=BindingStrategy.TEXT, resolved_by="heuristic",
                           cache_value=name_re.pattern)

    return None


def _resolve_with_llm(page: Page, intent: str, action: StepAction, target_hint: str,
                       llm: LLMClient) -> Resolution | None:
    elements = page.evaluate(_INTERACTIVE_ELEMENTS_JS)
    prompt = json.dumps({"intent": intent, "action": action.value, "hint": target_hint, "elements": elements})
    response = llm.complete_json("resolve_intent", RESOLVE_SYSTEM_PROMPT, prompt)

    index = response.get("element_index")
    if index is None or not (0 <= index < len(elements)):
        return None
    chosen = elements[index]
    model_id = response.get("model", "unknown")

    candidates: list[tuple[Locator, BindingStrategy, str]] = []
    if chosen.get("testid"):
        candidates.append((page.get_by_test_id(chosen["testid"]), BindingStrategy.TEST_ID, chosen["testid"]))
    if chosen.get("text"):
        candidates.append((page.get_by_text(chosen["text"], exact=True), BindingStrategy.TEXT, chosen["text"]))
    if chosen.get("id"):
        css = f"#{chosen['id']}"
        candidates.append((page.locator(css), BindingStrategy.CSS, css))

    for locator, strategy, cache_value in candidates:
        if _unique_visible(locator):
            return Resolution(locator=locator, strategy=strategy, resolved_by=model_id, cache_value=cache_value)

    # Nothing about the chosen element uniquely identifies it (e.g. one of
    # several identically-worded "Book this flight" links) -- fall back to
    # its exact position in the same combined selector the snapshot itself
    # enumerated. This is less durable across a future DOM reshuffle than a
    # semantic locator (the cache's own hit/miss tracking will surface it if
    # it stops working), but it is the correct, unambiguous answer for the
    # page state being resolved right now.
    idx = chosen.get("index")
    if idx is not None:
        positional = page.locator(_INTERACTIVE_SELECTOR).nth(idx)
        if _unique_visible(positional):
            return Resolution(locator=positional, strategy=BindingStrategy.CSS, resolved_by=model_id,
                               cache_value=f"nth::{idx}")
    return None


def _bind(conn: sqlite3.Connection, spec_id: str, ordinal: int, intent: str, resolution: Resolution) -> None:
    """Persists a freshly resolved (non-cached) binding, deactivating
    whatever was there before -- if something was, this is a heal (recorded
    via healed_from), and the caller is expected to log it as such."""
    previous = conn.execute(
        "SELECT id, locator FROM intent_bindings WHERE spec_id=? AND step_ordinal=? AND active=1",
        (spec_id, ordinal),
    ).fetchone()
    healed_from = None
    if previous is not None:
        resolution.healed = True
        healed_from = previous["locator"]
        conn.execute("UPDATE intent_bindings SET active=0 WHERE id=?", (previous["id"],))

    conn.execute(
        """INSERT INTO intent_bindings
           (id, spec_id, step_ordinal, intent, strategy, locator, strategy_rank,
            resolved_at, resolved_by, hit_count, miss_count, last_success, healed_from, active)
           VALUES (?,?,?,?,?,?,?,?,?,1,0,?,?,1)""",
        (f"{spec_id}#{ordinal}#{datetime.now(timezone.utc).timestamp()}", spec_id, ordinal, intent,
         resolution.strategy.value, resolution.cache_value, resolution.strategy.rank,
         datetime.now(timezone.utc).isoformat(), resolution.resolved_by,
         datetime.now(timezone.utc).isoformat(), healed_from),
    )


def _record_success(conn: sqlite3.Connection, spec_id: str, ordinal: int) -> None:
    conn.execute(
        "UPDATE intent_bindings SET hit_count = hit_count + 1, last_success = ? "
        "WHERE spec_id=? AND step_ordinal=? AND active=1",
        (datetime.now(timezone.utc).isoformat(), spec_id, ordinal),
    )
