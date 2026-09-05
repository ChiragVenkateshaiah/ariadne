"""Ariadne dashboard: a read-only window over the world model and the
adjudication ledger. Runs as its own process, reading the same SQLite file
the ingester (graph/ingest.py) and adjudicator write to -- WAL mode (set in
graph/store.py) means this can read live while those write, with no IPC of
our own. This is deliberately NOT where any decision gets made; it exists
only to make decisions already recorded elsewhere visible.
"""

from __future__ import annotations

import json
import os
import sqlite3
from pathlib import Path

from fastapi import FastAPI, Request
from fastapi.responses import HTMLResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates

from ariadne.graph import store

DB_PATH = os.environ.get("ARIADNE_DB_PATH", "ariadne.db")
HERE = Path(__file__).parent

app = FastAPI(title="Ariadne")
templates = Jinja2Templates(directory=str(HERE / "templates"))
app.mount("/static", StaticFiles(directory=str(HERE / "static")), name="static")

# Minutes of manual triage a single heal is assumed to avoid -- a stated,
# documented assumption behind the "maintenance saved" headline number,
# not a number we pretend to have measured precisely for a demo system.
MINUTES_SAVED_PER_HEAL = 15


def db() -> sqlite3.Connection:
    return store.connect(DB_PATH)


@app.get("/", response_class=HTMLResponse)
def index(request: Request):
    return templates.TemplateResponse(request, "index.html", {})


@app.get("/api/graph")
def graph_json(all: bool = False):
    """Defaults to the demo-relevant subgraph: the SUT's own namespace plus
    business-layer nodes (Workflow/WorkflowStep have no namespace). The
    sensor also watches cluster-wide RBAC (by design -- see
    docs/ARCHITECTURE.md), which at cluster scale is 100+ system Roles/
    ServiceAccounts that would otherwise swamp the one story this panel
    exists to tell. Pass ?all=true for the unfiltered graph.
    """
    conn = db()
    if all:
        where = "WHERE active = 1"
    else:
        where = "WHERE active = 1 AND (namespace = 'travel' OR kind IN ('WORKFLOW', 'WORKFLOW_STEP'))"
    nodes = [dict(r) for r in conn.execute(f"SELECT id, kind, name, namespace, discovery, confidence FROM nodes {where}")]
    node_ids = {n["id"] for n in nodes}
    edges = [dict(r) for r in conn.execute("SELECT id, src_id, dst_id, kind FROM edges WHERE active = 1")
             if r["src_id"] in node_ids and r["dst_id"] in node_ids]
    conn.close()
    return {"nodes": nodes, "edges": edges}


@app.get("/partials/metrics", response_class=HTMLResponse)
def metrics_partial(request: Request):
    conn = db()
    workflows_total = _scalar(conn, "SELECT COUNT(*) FROM workflows")
    workflows_covered = _scalar(conn, "SELECT COUNT(DISTINCT workflow_id) FROM test_specs WHERE active = 1")
    heals = _scalar(conn, "SELECT COUNT(*) FROM heals")
    blocked = _scalar(conn, "SELECT COUNT(*) FROM findings WHERE severity IN ('HIGH','CRITICAL')")
    changes_total = _scalar(conn, "SELECT COUNT(*) FROM change_events")
    conn.close()
    return templates.TemplateResponse(request, "_metrics.html", {
        "workflows_total": workflows_total,
        "workflows_covered": workflows_covered,
        "coverage_pct": round(100 * workflows_covered / workflows_total) if workflows_total else 0,
        "heals": heals,
        "blocked": blocked,
        "changes_total": changes_total,
        "minutes_saved": heals * MINUTES_SAVED_PER_HEAL,
    })


@app.get("/partials/changes", response_class=HTMLResponse)
def changes_partial(request: Request):
    conn = db()
    rows = conn.execute(
        """SELECT id, observed_at, change_class, operation, object_kind, object_ns, object_name, hints_json
           FROM change_events ORDER BY observed_at DESC LIMIT 20"""
    ).fetchall()
    conn.close()
    changes = []
    for r in rows:
        hints = json.loads(r["hints_json"])
        changes.append({
            **{k: r[k] for k in r.keys()},
            "is_noise": hints.get("is_noise", False),
            "touched_workloads": hints.get("touched_workload_names", []),
        })
    return templates.TemplateResponse(request, "_changes.html", {"changes": changes})


@app.get("/partials/ledger", response_class=HTMLResponse)
def ledger_partial(request: Request):
    conn = db()
    heals = conn.execute(
        """SELECT id, healed_at AS at, adjudication, intent, old_binding, new_binding,
                  reasoning, confidence FROM heals ORDER BY healed_at DESC LIMIT 20"""
    ).fetchall()
    findings = conn.execute(
        """SELECT id, created_at AS at, category, severity, title, description,
                  root_cause, reasoning, confidence FROM findings ORDER BY created_at DESC LIMIT 20"""
    ).fetchall()
    conn.close()

    items = [{"row_kind": "heal", **{k: r[k] for k in r.keys()}} for r in heals]
    items += [{"row_kind": "finding", **{k: r[k] for k in r.keys()}} for r in findings]
    items.sort(key=lambda x: x["at"] or "", reverse=True)
    return templates.TemplateResponse(request, "_ledger.html", {"items": items[:20]})


@app.get("/partials/risk", response_class=HTMLResponse)
def risk_partial(request: Request):
    """Most recent impact analysis: which workflows were put at risk by the
    latest change, and why -- the risk-based-selection story made visible."""
    conn = db()
    latest = conn.execute("SELECT id, change_event_id, created_at FROM impact_analyses ORDER BY created_at DESC LIMIT 1").fetchone()
    rows = []
    change = None
    if latest is not None:
        change = conn.execute(
            "SELECT change_class, object_kind, object_ns, object_name FROM change_events WHERE id = ?",
            (latest["change_event_id"],),
        ).fetchone()
        rows = conn.execute(
            """SELECT wr.risk_score, wr.hop_distance, wr.criticality, wr.coverage_gap, wr.reason,
                      w.slug, w.title
               FROM workflow_risk wr JOIN workflows w ON w.node_id = wr.workflow_id
               WHERE wr.impact_id = ? ORDER BY wr.risk_score DESC""",
            (latest["id"],),
        ).fetchall()
    conn.close()
    return templates.TemplateResponse(request, "_risk.html", {
        "change": dict(change) if change else None,
        "risks": [dict(r) for r in rows],
    })


def _scalar(conn: sqlite3.Connection, sql: str) -> int:
    row = conn.execute(sql).fetchone()
    return row[0] if row and row[0] is not None else 0
