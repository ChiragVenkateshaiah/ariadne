"""Thin persistence layer over schema.sql.

Deliberately not an ORM: the schema is small and stable enough (it was
frozen before any implementation started -- see docs/ARCHITECTURE.md) that
hand-written upserts are less code and more legible than a mapping layer
would be. Every function here is a direct, obvious translation of one schema
table.
"""

from __future__ import annotations

import json
import sqlite3
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from ariadne.graph.model import Discovery, EdgeKind, Node, NodeKind

_SCHEMA_PATH = Path(__file__).parent / "schema.sql"


def connect(db_path: str) -> sqlite3.Connection:
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA foreign_keys = ON")
    conn.executescript(_SCHEMA_PATH.read_text())
    return conn


@contextmanager
def transaction(conn: sqlite3.Connection) -> Iterator[sqlite3.Connection]:
    try:
        yield conn
        conn.commit()
    except Exception:
        conn.rollback()
        raise


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def upsert_node(
    conn: sqlite3.Connection,
    node_id: str,
    kind: NodeKind,
    name: str,
    *,
    namespace: str | None = None,
    k8s_uid: str | None = None,
    display_name: str | None = None,
    discovery: Discovery = Discovery.K8S_API,
    confidence: float = 1.0,
    attrs: dict[str, Any] | None = None,
) -> None:
    """Insert a node, or refresh it if one with this id already exists.

    On conflict, `discovery`/`confidence` are only tightened (a later K8S_API
    observation may upgrade an earlier LLM_INFERRED guess), never loosened --
    the reverse would let a downgraded observation quietly weaken a fact the
    graph already trusted more strongly.
    """
    now = _now()
    existing = conn.execute("SELECT discovery, confidence, attrs FROM nodes WHERE id = ?", (node_id,)).fetchone()
    if existing is not None:
        merged_attrs = json.loads(existing["attrs"])
        merged_attrs.update(attrs or {})
        keep_discovery = existing["discovery"]
        keep_confidence = existing["confidence"]
        if confidence >= existing["confidence"]:
            keep_discovery, keep_confidence = discovery.value, confidence
        conn.execute(
            """UPDATE nodes SET name=?, namespace=?, k8s_uid=?, display_name=?,
               discovery=?, confidence=?, attrs=?, last_seen=?, active=1
               WHERE id=?""",
            (name, namespace, k8s_uid, display_name, keep_discovery, keep_confidence,
             json.dumps(merged_attrs), now, node_id),
        )
        return
    conn.execute(
        """INSERT INTO nodes (id, kind, name, namespace, k8s_uid, display_name,
           discovery, confidence, attrs, first_seen, last_seen, active)
           VALUES (?,?,?,?,?,?,?,?,?,?,?,1)""",
        (node_id, kind.value, name, namespace, k8s_uid, display_name,
         discovery.value, confidence, json.dumps(attrs or {}), now, now),
    )


def deactivate_node(conn: sqlite3.Connection, node_id: str) -> None:
    """Soft-delete: history stays in place as evidence (see schema.sql)."""
    conn.execute("UPDATE nodes SET active=0, last_seen=? WHERE id=?", (_now(), node_id))


def upsert_edge(
    conn: sqlite3.Connection,
    edge_id: str,
    src_id: str,
    dst_id: str,
    kind: EdgeKind,
    *,
    discovery: Discovery = Discovery.K8S_API,
    confidence: float = 1.0,
    weight: float = 1.0,
    ordinal: int | None = None,
    attrs: dict[str, Any] | None = None,
) -> None:
    now = _now()
    existing = conn.execute("SELECT id FROM edges WHERE id = ?", (edge_id,)).fetchone()
    if existing is not None:
        conn.execute(
            """UPDATE edges SET discovery=?, confidence=?, weight=?, ordinal=?,
               attrs=?, last_seen=?, active=1 WHERE id=?""",
            (discovery.value, confidence, weight, ordinal, json.dumps(attrs or {}), now, edge_id),
        )
        return
    conn.execute(
        """INSERT INTO edges (id, src_id, dst_id, kind, discovery, confidence,
           weight, ordinal, attrs, first_seen, last_seen, active)
           VALUES (?,?,?,?,?,?,?,?,?,?,?,1)""",
        (edge_id, src_id, dst_id, kind.value, discovery.value, confidence,
         weight, ordinal, json.dumps(attrs or {}), now, now),
    )


def deactivate_edge(conn: sqlite3.Connection, edge_id: str) -> None:
    conn.execute("UPDATE edges SET active=0, last_seen=? WHERE id=?", (_now(), edge_id))


def deactivate_edges_from(conn: sqlite3.Connection, src_id: str, kind: EdgeKind) -> None:
    """Used when a workload's references change (e.g. a Deployment's volume
    mounts changed) -- clear all its outgoing edges of one kind before
    re-adding the current set, so a removed reference doesn't linger."""
    conn.execute(
        "UPDATE edges SET active=0, last_seen=? WHERE src_id=? AND kind=? AND active=1",
        (_now(), src_id, kind.value),
    )


def record_change_event(
    conn: sqlite3.Connection,
    event_id: str,
    observed_at: str,
    source: str,
    change_class: str,
    operation: str,
    *,
    object_node_id: str | None,
    object_kind: str,
    object_ns: str,
    object_name: str,
    hints: dict[str, Any],
    diffs: list[dict[str, Any]],
    provenance: dict[str, Any],
) -> None:
    conn.execute(
        """INSERT OR IGNORE INTO change_events
           (id, observed_at, source, change_class, operation, object_node_id,
            object_kind, object_ns, object_name, hints_json, diffs_json, provenance_json, processed)
           VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0)""",
        (event_id, observed_at, source, change_class, operation, object_node_id,
         object_kind, object_ns, object_name, json.dumps(hints), json.dumps(diffs), json.dumps(provenance)),
    )


def get_node(conn: sqlite3.Connection, node_id: str) -> Node | None:
    row = conn.execute("SELECT * FROM nodes WHERE id = ? AND active = 1", (node_id,)).fetchone()
    if row is None:
        return None
    return Node(
        id=row["id"], kind=NodeKind(row["kind"]), name=row["name"], namespace=row["namespace"],
        k8s_uid=row["k8s_uid"], display_name=row["display_name"], discovery=Discovery(row["discovery"]),
        confidence=row["confidence"], attrs=json.loads(row["attrs"]),
    )


def upsert_workflow(
    conn: sqlite3.Connection,
    node_id: str,
    slug: str,
    title: str,
    *,
    description: str | None = None,
    business_goal: str | None = None,
    criticality: float = 0.5,
    revenue_path: bool = False,
    pii_involved: bool = False,
    entry_route: str | None = None,
    persona: str | None = None,
    derived_from: str | None = None,
) -> None:
    """Workflow nodes live in both `nodes` (for uniform graph traversal) and
    `workflows` (for the business metadata that drives risk scoring) -- the
    two inserts are kept together here so no caller can create one without
    the other."""
    upsert_node(conn, node_id, NodeKind.WORKFLOW, title, discovery=Discovery.LLM_INFERRED, confidence=0.7)
    exists = conn.execute("SELECT 1 FROM workflows WHERE node_id = ?", (node_id,)).fetchone()
    if exists:
        conn.execute(
            """UPDATE workflows SET title=?, description=?, business_goal=?, criticality=?,
               revenue_path=?, pii_involved=?, entry_route=?, persona=?, derived_from=?
               WHERE node_id=?""",
            (title, description, business_goal, criticality, int(revenue_path), int(pii_involved),
             entry_route, persona, derived_from, node_id),
        )
        return
    conn.execute(
        """INSERT INTO workflows (node_id, slug, title, description, business_goal, criticality,
           revenue_path, pii_involved, entry_route, persona, derived_from, reviewed_by_human)
           VALUES (?,?,?,?,?,?,?,?,?,?,?,0)""",
        (node_id, slug, title, description, business_goal, criticality,
         int(revenue_path), int(pii_involved), entry_route, persona, derived_from),
    )


def upsert_workflow_step(
    conn: sqlite3.Connection,
    node_id: str,
    workflow_id: str,
    ordinal: int,
    intent: str,
    action: str,
    *,
    target_hint: str | None = None,
    value_expr: str | None = None,
    assertion: str | None = None,
    optional: bool = False,
) -> None:
    upsert_node(conn, node_id, NodeKind.WORKFLOW_STEP, intent, discovery=Discovery.LLM_INFERRED, confidence=0.7)
    exists = conn.execute("SELECT 1 FROM workflow_steps WHERE node_id = ?", (node_id,)).fetchone()
    if exists:
        conn.execute(
            """UPDATE workflow_steps SET ordinal=?, intent=?, action=?, target_hint=?,
               value_expr=?, assertion=?, optional=? WHERE node_id=?""",
            (ordinal, intent, action, target_hint, value_expr, assertion, int(optional), node_id),
        )
        return
    conn.execute(
        """INSERT INTO workflow_steps (node_id, workflow_id, ordinal, intent, action,
           target_hint, value_expr, assertion, optional)
           VALUES (?,?,?,?,?,?,?,?,?)""",
        (node_id, workflow_id, ordinal, intent, action, target_hint, value_expr, assertion, int(optional)),
    )
