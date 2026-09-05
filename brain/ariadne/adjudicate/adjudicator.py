"""The heart of the system, per docs/ARCHITECTURE.md's central claim: naive
self-healing is a liability, because a test that heals around a real
regression has converted a caught bug into an escaped one. Every failure or
heal lands in exactly one Adjudication bucket, with reasoning attached.

The one invariant enforced in CODE, not just by convention: write_heal()
refuses to persist a heal whose adjudication is APP_REGRESSION. If the model
concludes that, this is not a heal that happens to carry a scary label --
it must be raised as a Finding and the release gated, full stop.
"""

from __future__ import annotations

import json
import sqlite3
import uuid
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone

import networkx as nx

from ariadne.graph import impact
from ariadne.llm.client import LLMClient

ADJUDICATE_SYSTEM_PROMPT = """You are adjudicating why a test step failed or \
had to be re-bound to a new element. You are given: the workflow, what \
failed or was re-bound, recent Kubernetes changes to the services this \
workflow depends on (with diffs), and any relevant evidence (log errors, \
K8s events). Decide which ONE of these buckets applies:

  TEST_DEFECT     -- locator/selector drift from a cosmetic UI change; safe to heal
  INTENT_DRIFT     -- the application intentionally changed behavior; the test spec itself should be updated
  APP_REGRESSION   -- a real bug: NEVER heal this, it must block the release
  ENV_FLAKE        -- infrastructure noise (timeout, transient network); retry, don't touch the test
  UNDETERMINED     -- insufficient evidence; escalate to a human

Bias toward APP_REGRESSION or UNDETERMINED over a false TEST_DEFECT/INTENT_DRIFT --
converting a real bug into a healed test is the one mistake this system
exists to prevent. A ConfigMap/behavioral change with no clear cosmetic
(UI-only) signal should be treated with suspicion, not assumed benign.

Respond with ONLY a JSON object: {"adjudication": "...", "confidence": 0.0-1.0,
"root_cause": "...", "reasoning": "...", "summary": "..."}"""

_VALID_ADJUDICATIONS = {"TEST_DEFECT", "INTENT_DRIFT", "APP_REGRESSION", "ENV_FLAKE", "UNDETERMINED"}


@dataclass(slots=True)
class AdjudicationResult:
    adjudication: str
    confidence: float
    root_cause: str
    reasoning: str
    summary: str

    def __post_init__(self) -> None:
        if self.adjudication not in _VALID_ADJUDICATIONS:
            raise ValueError(f"model returned unrecognized adjudication: {self.adjudication!r}")


def gather_recent_changes(conn: sqlite3.Connection, workflow_node_id: str,
                           graph: nx.DiGraph | None = None, lookback_minutes: int = 15) -> list[dict]:
    """Finds everything this workflow transitively depends on (the forward,
    "descendants" direction -- the mirror image of impact.py's ancestors()
    blast-radius query) and pulls recent change_events for those specific
    nodes. This is what lets the adjudicator ask "did something this
    workflow relies on change recently?" using the same graph impact
    analysis already built, rather than a second traversal mechanism.
    """
    g = graph if graph is not None else impact.build_graph(conn)
    if workflow_node_id not in g:
        return []
    deps = nx.descendants(g, workflow_node_id)
    if not deps:
        return []

    since = (datetime.now(timezone.utc) - timedelta(minutes=lookback_minutes)).isoformat()
    placeholders = ",".join("?" for _ in deps)
    rows = conn.execute(
        f"""SELECT id, observed_at, change_class, operation, object_kind, object_ns, object_name,
                   diffs_json, provenance_json
            FROM change_events
            WHERE object_node_id IN ({placeholders}) AND observed_at >= ?
            ORDER BY observed_at DESC""",
        (*deps, since),
    ).fetchall()

    changes = []
    for r in rows:
        changes.append({
            "id": r["id"], "observed_at": r["observed_at"], "change_class": r["change_class"],
            "operation": r["operation"], "object_kind": r["object_kind"],
            "object_namespace": r["object_ns"], "object_name": r["object_name"],
            "diffs": json.loads(r["diffs_json"]), "provenance": json.loads(r["provenance_json"]),
        })
    return changes


def adjudicate(llm: LLMClient, workflow_slug: str, failure_summary: str,
                recent_changes: list[dict], evidence: dict | None = None) -> AdjudicationResult:
    prompt = json.dumps({
        "workflow": workflow_slug, "failure_summary": failure_summary,
        "recent_changes": recent_changes, "evidence": evidence or {},
    }, indent=2)
    response = llm.complete_json("adjudicate_failure", ADJUDICATE_SYSTEM_PROMPT, prompt)
    return AdjudicationResult(
        adjudication=response["adjudication"], confidence=float(response.get("confidence", 0.5)),
        root_cause=response.get("root_cause", ""), reasoning=response.get("reasoning", ""),
        summary=response.get("summary", ""),
    )


def write_heal(conn: sqlite3.Connection, run_id: str, spec_id: str, step_ordinal: int, intent: str,
               old_binding: str, new_binding: str, strategy: str, result: AdjudicationResult,
               supporting_change_ids: list[str], applied: bool = True) -> str:
    """Persists a heal -- but ONLY if adjudicated as safe to heal. This is
    the invariant from the module docstring, enforced here rather than left
    to caller discipline: calling this with adjudication=APP_REGRESSION is a
    bug in the caller, not a heal this function will ever write.
    """
    if result.adjudication == "APP_REGRESSION":
        raise ValueError(
            "refusing to write a heal with adjudication=APP_REGRESSION -- "
            "this must be raised as a finding via write_finding() instead, and the run FAILED"
        )
    heal_id = str(uuid.uuid4())
    conn.execute(
        """INSERT INTO heals (id, run_id, spec_id, step_ordinal, adjudication, intent, old_binding,
           new_binding, strategy, reasoning, confidence, applied, requires_review,
           supporting_change_ids, healed_at)
           VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
        (heal_id, run_id, spec_id, step_ordinal, result.adjudication, intent, old_binding, new_binding,
         strategy, result.reasoning, result.confidence, int(applied), int(result.confidence < 0.6),
         json.dumps(supporting_change_ids), datetime.now(timezone.utc).isoformat()),
    )
    return heal_id


def write_finding(conn: sqlite3.Connection, run_id: str, category: str, severity: str, title: str,
                   description: str, result: AdjudicationResult, affected_workflow_ids: list[str],
                   evidence_refs: list[str] | None = None,
                   first_seen_change_id: str | None = None) -> str:
    finding_id = str(uuid.uuid4())
    conn.execute(
        """INSERT INTO findings (id, run_id, category, severity, title, description, root_cause,
           reasoning, confidence, evidence_json, workflows_json, remediation_json, owasp_refs,
           first_seen_change_id, created_at)
           VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
        (finding_id, run_id, category, severity, title, description, result.root_cause, result.reasoning,
         result.confidence, json.dumps(evidence_refs or []), json.dumps(affected_workflow_ids),
         json.dumps({}), json.dumps([]), first_seen_change_id, datetime.now(timezone.utc).isoformat()),
    )
    return finding_id
