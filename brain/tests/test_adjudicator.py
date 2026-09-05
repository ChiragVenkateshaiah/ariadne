"""The actual thesis of the project, pinned as tests: a failure following a
cosmetic UI change heals; a failure following an unreviewed business-logic
config change blocks; and write_heal() refuses, in code, to ever persist a
heal for an APP_REGRESSION verdict. These are the exact Act 1 / Act 2
scenarios verified live against the real cluster earlier in the project --
now permanent regression coverage instead of a one-off manual run.
"""

import pytest

from ariadne.adjudicate.adjudicator import (
    AdjudicationResult,
    adjudicate,
    write_finding,
    write_heal,
)
from ariadne.graph import model as gmodel
from ariadne.graph import store
from ariadne.llm.fixtures import default_mock_client


@pytest.fixture
def conn():
    c = store.connect(":memory:")
    wf_id = "workflow:-/book_one_way_flight"
    store.upsert_node(c, wf_id, gmodel.NodeKind.WORKFLOW, "book_one_way_flight")
    store.upsert_workflow(c, wf_id, "book_one_way_flight", "Book a one-way flight", criticality=0.9)
    with store.transaction(c):
        c.execute("INSERT INTO runs (id, started_at, trigger) VALUES ('run-1','2026-01-01','test')")
        c.execute(
            "INSERT INTO test_specs (id, workflow_id, kind, spec_json, generated_by, generated_at) "
            "VALUES ('spec-1', ?, 'UI', '{}', 'test', '2026-01-01')", (wf_id,),
        )
    return c


@pytest.fixture
def llm():
    return default_mock_client()


def test_act1_ui_workload_change_with_clean_evidence_heals(conn, llm):
    changes = [{
        "change_class": "CHANGE_CLASS_WORKLOAD_SPEC", "object_name": "web-ui",
        "diffs": [{"path": "spec.template.spec.containers", "before": "...", "after": "..."}],
    }]
    result = adjudicate(llm, "book_one_way_flight", "search submit button locator drift", changes)

    assert result.adjudication == "TEST_DEFECT"
    assert result.confidence > 0.5

    with store.transaction(conn):
        heal_id = write_heal(conn, "run-1", "spec-1", 5, "submit the search",
                              "button::search", "text::Find flights", "TEXT", result, [])
    row = conn.execute("SELECT adjudication FROM heals WHERE id = ?", (heal_id,)).fetchone()
    assert row["adjudication"] == "TEST_DEFECT"


def test_act2_business_logic_config_change_blocks_not_heals(conn, llm):
    changes = [{
        "change_class": "CHANGE_CLASS_CONFIG", "object_name": "pricing-flags",
        "diffs": [{"path": "data.flags.json",
                   "before": '{"rounding_mode": "HALF_UP"}', "after": '{"rounding_mode": "FLOOR"}'}],
    }]
    result = adjudicate(llm, "book_one_way_flight",
                         "displayed price does not match expected computed price", changes)

    assert result.adjudication == "APP_REGRESSION"
    assert "pricing-flags" in result.root_cause
    assert "FLOOR" in result.root_cause

    with store.transaction(conn):
        finding_id = write_finding(conn, "run-1", "FUNCTIONAL", "HIGH",
                                    "Pricing regression: FLOOR rounding undercharges customers",
                                    "rounding_mode changed from HALF_UP to FLOOR", result,
                                    ["workflow:-/book_one_way_flight"])
    row = conn.execute("SELECT category, severity FROM findings WHERE id = ?", (finding_id,)).fetchone()
    assert row["category"] == "FUNCTIONAL" and row["severity"] == "HIGH"


def test_write_heal_refuses_app_regression_verdict(conn):
    """The one invariant enforced in code, not just by convention (see
    adjudicator.py's module docstring): this must raise, never insert."""
    regression = AdjudicationResult(
        adjudication="APP_REGRESSION", confidence=0.9,
        root_cause="a real bug", reasoning="...", summary="blocked",
    )
    with pytest.raises(ValueError, match="APP_REGRESSION"), store.transaction(conn):
        write_heal(conn, "run-1", "spec-1", 1, "intent", "old", "new", "TEXT", regression, [])

    assert conn.execute("SELECT COUNT(*) c FROM heals").fetchone()["c"] == 0


def test_no_evidence_and_no_relevant_change_is_undetermined_not_a_guess(llm):
    result = adjudicate(llm, "book_one_way_flight", "unexpected timeout", [], evidence={})
    assert result.adjudication == "UNDETERMINED"
    assert result.confidence < 0.5


def test_application_log_error_outranks_a_cosmetic_change_signal(llm):
    """If BOTH a cosmetic change AND a real log error are present, the log
    error must win -- never silently prefer the more comfortable verdict."""
    changes = [{"change_class": "CHANGE_CLASS_WORKLOAD_SPEC", "object_name": "web-ui", "diffs": []}]
    evidence = {"error_log_lines": ["panic: nil pointer dereference in PriceEngine"]}
    result = adjudicate(llm, "book_one_way_flight", "booking failed", changes, evidence=evidence)
    assert result.adjudication == "APP_REGRESSION"


def test_adjudication_result_rejects_unrecognized_verdict():
    with pytest.raises(ValueError):
        AdjudicationResult(adjudication="MAYBE_FINE_WHO_KNOWS", confidence=0.5,
                            root_cause="", reasoning="", summary="")
