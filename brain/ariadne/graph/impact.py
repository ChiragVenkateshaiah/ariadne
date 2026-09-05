"""Impact analysis: turns "this object changed" into "these workflows are at
risk, ranked, with reasons" -- a graph traversal, not an LLM guess.

Edge direction throughout the graph is consistently "depends on": Workload
--CALLS--> Service means the workload depends on that service; Service
--BACKED_BY--> Workload means the service depends on (is realized by) that
workload; WorkflowStep --RENDERS_ON--> UiRoute --SERVED_BY--> Service means
the step depends on that route being served by that service. Because of that
consistency, "what depends on the thing that changed" is exactly
networkx.ancestors() on the changed node -- no bespoke traversal needed for
each edge kind, and the LLM never touches this arithmetic. The LLM's only
job (see adjudicate.py, not yet built) is writing the English `reason`
alongside a score this module already computed deterministically.
"""

from __future__ import annotations

import json
import sqlite3
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone

import networkx as nx

from ariadne.graph.model import CHANGE_CLASS_RISK, RISK_WEIGHTS, NodeKind, WorkflowRisk


def build_graph(conn: sqlite3.Connection) -> nx.DiGraph:
    g = nx.DiGraph()
    for row in conn.execute("SELECT id, kind, namespace, name FROM nodes WHERE active = 1"):
        g.add_node(row["id"], kind=row["kind"], namespace=row["namespace"], name=row["name"])
    for row in conn.execute("SELECT id, src_id, dst_id, kind FROM edges WHERE active = 1"):
        # Both endpoints must already be nodes; a dangling edge (stale data,
        # a bug upstream) is dropped rather than crashing analysis for the
        # whole graph -- impact analysis degrading gracefully beats it not
        # running at all.
        if row["src_id"] in g and row["dst_id"] in g:
            g.add_edge(row["src_id"], row["dst_id"], kind=row["kind"])
    return g


@dataclass(slots=True)
class ImpactResult:
    change_event_id: str
    changed_node_id: str
    risks: list[WorkflowRisk]


def _hop_distance(reversed_graph: nx.DiGraph, changed_node_id: str, workflow_id: str) -> int:
    try:
        return nx.shortest_path_length(reversed_graph, source=changed_node_id, target=workflow_id)
    except nx.NetworkXNoPath:
        return -1  # should not happen if workflow_id came from ancestors(), but never crash on it


def _coverage_gap(conn: sqlite3.Connection, workflow_node_id: str) -> float:
    row = conn.execute(
        "SELECT COUNT(*) c FROM test_specs WHERE workflow_id = ? AND active = 1", (workflow_node_id,)
    ).fetchone()
    return 0.0 if row["c"] > 0 else 1.0


def _flakiness(conn: sqlite3.Connection, workflow_node_id: str) -> float:
    row = conn.execute(
        """SELECT AVG(CASE WHEN passed = 0 THEN 1.0 ELSE 0.0 END) AS flake_rate
           FROM test_history th JOIN test_specs ts ON ts.id = th.spec_id
           WHERE ts.workflow_id = ?""",
        (workflow_node_id,),
    ).fetchone()
    return float(row["flake_rate"] or 0.0)


def compute_impact(
    conn: sqlite3.Connection,
    change_event_id: str,
    changed_node_id: str,
    change_class: str,
    *,
    graph: nx.DiGraph | None = None,
    reason_by_slug: dict[str, str] | None = None,
) -> ImpactResult:
    """Computes and PERSISTS (impact_analyses + workflow_risk) the blast
    radius of one change. `reason_by_slug` lets a caller (the adjudicator,
    once built) attach an LLM-written explanation per workflow without this
    module importing an LLM client itself -- the score and the prose are
    deliberately produced by different layers.
    """
    g = graph if graph is not None else build_graph(conn)
    reason_by_slug = reason_by_slug or {}

    risks: list[WorkflowRisk] = []
    if changed_node_id in g:
        ancestors = nx.ancestors(g, changed_node_id)
        reversed_g = g.reverse(copy=False)
        change_multiplier = CHANGE_CLASS_RISK.get(change_class, 0.5)

        for node_id in ancestors:
            if g.nodes[node_id].get("kind") != NodeKind.WORKFLOW.value:
                continue
            row = conn.execute(
                "SELECT slug, criticality FROM workflows WHERE node_id = ?", (node_id,)
            ).fetchone()
            if row is None:
                continue  # a WORKFLOW node with no workflows row is a data bug elsewhere; skip defensively

            hop_distance = _hop_distance(reversed_g, changed_node_id, node_id)
            proximity = 1.0 / (1.0 + max(hop_distance, 0))
            coverage_gap = _coverage_gap(conn, node_id)
            flakiness = _flakiness(conn, node_id)

            risk_score = (
                row["criticality"] * RISK_WEIGHTS["criticality"]
                + proximity * RISK_WEIGHTS["proximity"]
                + coverage_gap * RISK_WEIGHTS["coverage_gap"]
                + change_multiplier * RISK_WEIGHTS["change_class"]
                - flakiness * RISK_WEIGHTS["flakiness_penalty"]
            )

            risks.append(WorkflowRisk(
                workflow_id=node_id, risk_score=round(risk_score, 4), hop_distance=hop_distance,
                criticality=row["criticality"], coverage_gap=coverage_gap, flakiness=flakiness,
                reason=reason_by_slug.get(row["slug"], ""),
            ))

    risks.sort(key=lambda r: r.risk_score, reverse=True)
    _persist(conn, change_event_id, risks)
    return ImpactResult(change_event_id=change_event_id, changed_node_id=changed_node_id, risks=risks)


def _persist(conn: sqlite3.Connection, change_event_id: str, risks: list[WorkflowRisk]) -> None:
    impact_id = str(uuid.uuid4())
    now = datetime.now(timezone.utc).isoformat()
    conn.execute(
        "INSERT INTO impact_analyses (id, change_event_id, created_at, blast_radius_json, rationale, model_id) "
        "VALUES (?,?,?,?,?,?)",
        (impact_id, change_event_id, now, json.dumps([r.workflow_id for r in risks]), None, None),
    )
    for r in risks:
        conn.execute(
            """INSERT INTO workflow_risk (impact_id, workflow_id, risk_score, hop_distance, criticality,
               coverage_gap, flakiness, reason, selected)
               VALUES (?,?,?,?,?,?,?,?,0)""",
            (impact_id, r.workflow_id, r.risk_score, r.hop_distance, r.criticality,
             r.coverage_gap, r.flakiness, r.reason),
        )
    conn.execute("UPDATE change_events SET processed = 1 WHERE id = ?", (change_event_id,))
