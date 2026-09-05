"""Resolver tests run against static HTML via page.set_content() -- no live
cluster, no network. Covers the two real bugs found while building this:
the OR-vs-AND keyword regex bug (origin/destination disambiguation) and the
positional nth() fallback for elements with no unique attribute.
"""

import pytest
from playwright.sync_api import sync_playwright

from ariadne.graph import store
from ariadne.graph.model import NodeKind, StepAction
from ariadne.llm.fixtures import default_mock_client
from ariadne.resolve import resolver
from ariadne.resolve.resolver import UnresolvableIntent


@pytest.fixture(scope="module")
def browser():
    with sync_playwright() as p:
        b = p.chromium.launch()
        yield b
        b.close()


@pytest.fixture
def page(browser):
    pg = browser.new_page()
    yield pg
    pg.close()


@pytest.fixture
def conn():
    c = store.connect(":memory:")
    with store.transaction(c):
        store.upsert_node(c, "wf", NodeKind.WORKFLOW, "wf")
        store.upsert_workflow(c, "wf", "wf", "wf")
        c.execute(
            "INSERT INTO test_specs (id, workflow_id, kind, spec_json, generated_by, generated_at) "
            "VALUES ('spec-1', 'wf', 'UI', '{}', 'test', '2026-01-01')"
        )
    return c


@pytest.fixture
def llm():
    return default_mock_client()


TWO_SIMILAR_LABELS_HTML = """
<form>
  <label for="origin">Origin airport</label>
  <input id="origin" name="origin">
  <label for="destination">Destination airport</label>
  <input id="destination" name="destination">
</form>
"""


def test_label_heuristic_disambiguates_similar_fields(page, conn, llm):
    """Regression test for the OR-vs-AND regex bug: an alternation of
    "origin|airport" would match BOTH fields (they share "airport"); the
    AND-lookahead must pick exactly the one containing both keywords."""
    page.set_content(TWO_SIMILAR_LABELS_HTML)

    res = resolver.resolve(page, conn, "spec-1", 1, "enter the origin airport",
                            StepAction.FILL, "origin airport input", llm)
    assert res.resolved_by == "heuristic"
    res.locator.fill("LHR")
    assert page.locator("#origin").input_value() == "LHR"
    assert page.locator("#destination").input_value() == ""


DUPLICATE_LINKS_HTML = """
<a href="/book?id=1">Book this flight</a>
<a href="/book?id=2">Book this flight</a>
<a href="/book?id=3">Book this flight</a>
"""


def test_llm_fallback_positional_nth_for_identical_elements(page, conn, llm):
    """Regression test for the positional-nth fallback: three links with
    identical text can't be told apart by any attribute, so a chosen index
    must resolve via its exact position in the same DOM-order snapshot the
    model reasoned over."""
    page.set_content(DUPLICATE_LINKS_HTML)

    res = resolver.resolve(page, conn, "spec-1", 1, "select the first offer to book",
                            StepAction.CLICK, "first offer's book-this-flight link", llm)
    assert res.resolved_by == "mock"
    href = res.locator.get_attribute("href")
    assert href == "/book?id=1"  # the FIRST of the three, not an arbitrary one


def test_cache_hit_skips_heuristics_and_llm_on_second_resolve(page, conn, llm):
    page.set_content(TWO_SIMILAR_LABELS_HTML)
    first = resolver.resolve(page, conn, "spec-1", 1, "enter the origin airport",
                              StepAction.FILL, "origin airport input", llm)
    assert first.resolved_by == "heuristic"

    second = resolver.resolve(page, conn, "spec-1", 1, "enter the origin airport",
                               StepAction.FILL, "origin airport input", llm)
    assert second.resolved_by == "cache"
    assert not second.healed


def test_unresolvable_intent_raises_rather_than_guessing(page, conn):
    page.set_content("<p>nothing interactive here</p>")
    with pytest.raises(UnresolvableIntent):
        resolver.resolve(page, conn, "spec-1", 1, "click a button that does not exist",
                          StepAction.CLICK, "nonexistent button", llm=None)


def test_heal_is_recorded_when_a_binding_changes(page, conn, llm):
    """First resolve binds to the v1 markup; after the page changes (the
    Act 1 scenario in miniature), re-resolving must record a heal with the
    old binding preserved for the audit trail."""
    page.set_content('<button type="submit">Search</button>')
    first = resolver.resolve(page, conn, "spec-1", 5, "submit the search",
                              StepAction.CLICK, "search submit button", llm)
    assert not first.healed

    page.set_content('<button type="submit">Find flights</button>')
    second = resolver.resolve(page, conn, "spec-1", 5, "submit the search",
                               StepAction.CLICK, "search submit button", llm)
    assert second.healed

    row = conn.execute(
        "SELECT healed_from FROM intent_bindings WHERE spec_id='spec-1' AND step_ordinal=5 AND active=1"
    ).fetchone()
    assert row["healed_from"] is not None
