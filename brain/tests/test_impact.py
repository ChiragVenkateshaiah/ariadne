"""impact.py is the mechanism behind "risk-based test selection is a graph
traversal, not an LLM guess" (docs/ARCHITECTURE.md). These tests build a
small synthetic topology by hand -- no live cluster, no LLM -- covering
exactly the chain the real demo exercises: a ConfigMap change reaching a
Workflow through Workload/Service/CALLS/BACKED_BY/RENDERS_ON/HAS_STEP edges.
"""

from datetime import datetime, timezone

from ariadne.graph import impact, store
from ariadne.graph.model import EdgeKind, NodeKind


def build_chain(conn):
    """ConfigMap <-MOUNTS- Workload(pricing-svc) <-BACKED_BY- Service(pricing-svc)
    <-CALLS- Workload(search-api) <-BACKED_BY- Service(search-api) <-CALLS-
    Workload(web-ui) <-BACKED_BY- Service(web-ui) <-SERVED_BY- UiRoute
    <-RENDERS_ON- WorkflowStep <-HAS_STEP- Workflow. Edge direction is always
    "depends on" (src depends on dst) -- see impact.py's module docstring.
    """
    nodes = {
        "cm": ("config_resource:travel/pricing-flags", NodeKind.CONFIG_RESOURCE),
        "w_pricing": ("workload:travel/pricing-svc", NodeKind.WORKLOAD),
        "s_pricing": ("service:travel/pricing-svc", NodeKind.SERVICE),
        "w_search": ("workload:travel/search-api", NodeKind.WORKLOAD),
        "s_search": ("service:travel/search-api", NodeKind.SERVICE),
        "w_webui": ("workload:travel/web-ui", NodeKind.WORKLOAD),
        "s_webui": ("service:travel/web-ui", NodeKind.SERVICE),
        "route": ("ui_route:travel/GET /", NodeKind.UI_ROUTE),
        "step": ("workflow_step:-/wf#1", NodeKind.WORKFLOW_STEP),
        "wf": ("workflow:-/book_flight", NodeKind.WORKFLOW),
    }
    for node_id, kind in nodes.values():
        store.upsert_node(conn, node_id, kind, node_id)

    edges = [
        ("w_pricing", EdgeKind.MOUNTS, "cm"),
        ("s_pricing", EdgeKind.BACKED_BY, "w_pricing"),
        ("w_search", EdgeKind.CALLS, "s_pricing"),
        ("s_search", EdgeKind.BACKED_BY, "w_search"),
        ("w_webui", EdgeKind.CALLS, "s_search"),
        ("s_webui", EdgeKind.BACKED_BY, "w_webui"),
        ("route", EdgeKind.SERVED_BY, "s_webui"),
        ("step", EdgeKind.RENDERS_ON, "route"),
        ("wf", EdgeKind.HAS_STEP, "step"),
    ]
    for src, kind, dst in edges:
        src_id, dst_id = nodes[src][0], nodes[dst][0]
        store.upsert_edge(conn, f"{src_id}|{kind.value}|{dst_id}", src_id, dst_id, kind)

    store.upsert_workflow(conn, nodes["wf"][0], "book_flight", "Book a flight", criticality=0.9)
    return nodes


def test_build_graph_includes_all_active_nodes_and_edges():
    conn = store.connect(":memory:")
    build_chain(conn)
    g = impact.build_graph(conn)
    assert g.number_of_nodes() == 10
    assert g.number_of_edges() == 9


def test_compute_impact_finds_workflow_nine_hops_away():
    conn = store.connect(":memory:")
    nodes = build_chain(conn)
    cm_id = nodes["cm"][0]

    change_id = "change-1"
    store.record_change_event(
        conn, change_id, datetime.now(timezone.utc).isoformat(),
        "CHANGE_SOURCE_KUBERNETES", "CHANGE_CLASS_CONFIG", "CHANGE_OPERATION_MODIFIED",
        object_node_id=cm_id, object_kind="ConfigMap", object_ns="travel", object_name="pricing-flags",
        hints={}, diffs=[], provenance={},
    )

    result = impact.compute_impact(conn, change_id, cm_id, "CHANGE_CLASS_CONFIG")

    assert len(result.risks) == 1
    risk = result.risks[0]
    assert risk.workflow_id == nodes["wf"][0]
    assert risk.hop_distance == 9
    assert risk.criticality == 0.9
    assert risk.coverage_gap == 1.0  # no test_specs row exists for this workflow
    assert 0.0 < risk.risk_score <= 1.0


def test_compute_impact_unrelated_change_finds_nothing():
    conn = store.connect(":memory:")
    build_chain(conn)
    # A node with no path to any workflow at all.
    store.upsert_node(conn, "orphan", NodeKind.CONFIG_RESOURCE, "unrelated-config")

    change_id = "change-2"
    store.record_change_event(
        conn, change_id, datetime.now(timezone.utc).isoformat(),
        "CHANGE_SOURCE_KUBERNETES", "CHANGE_CLASS_CONFIG", "CHANGE_OPERATION_MODIFIED",
        object_node_id="orphan", object_kind="ConfigMap", object_ns="travel", object_name="unrelated-config",
        hints={}, diffs=[], provenance={},
    )
    result = impact.compute_impact(conn, change_id, "orphan", "CHANGE_CLASS_CONFIG")
    assert result.risks == []


def test_compute_impact_persists_to_impact_analyses_and_workflow_risk():
    conn = store.connect(":memory:")
    nodes = build_chain(conn)
    cm_id = nodes["cm"][0]
    change_id = "change-3"
    store.record_change_event(
        conn, change_id, datetime.now(timezone.utc).isoformat(),
        "CHANGE_SOURCE_KUBERNETES", "CHANGE_CLASS_CONFIG", "CHANGE_OPERATION_MODIFIED",
        object_node_id=cm_id, object_kind="ConfigMap", object_ns="travel", object_name="pricing-flags",
        hints={}, diffs=[], provenance={},
    )
    impact.compute_impact(conn, change_id, cm_id, "CHANGE_CLASS_CONFIG")

    analyses = conn.execute("SELECT * FROM impact_analyses WHERE change_event_id = ?", (change_id,)).fetchall()
    assert len(analyses) == 1
    risks = conn.execute("SELECT * FROM workflow_risk WHERE impact_id = ?", (analyses[0]["id"],)).fetchall()
    assert len(risks) == 1

    processed = conn.execute("SELECT processed FROM change_events WHERE id = ?", (change_id,)).fetchone()
    assert processed["processed"] == 1


def test_higher_criticality_workflow_ranks_above_lower_criticality():
    """Two workflows equidistant from the change; the revenue-path one
    (higher criticality) must score higher -- the actual "run the revenue
    path first" claim, not just a traversal existence check."""
    conn = store.connect(":memory:")
    nodes = build_chain(conn)

    # A second workflow at the same hop distance, lower criticality.
    store.upsert_node(conn, "workflow_step:-/wf2#1", NodeKind.WORKFLOW_STEP, "step2")
    store.upsert_node(conn, "workflow:-/search_only", NodeKind.WORKFLOW, "search_only")
    store.upsert_edge(conn, "e1", "workflow_step:-/wf2#1", nodes["route"][0], EdgeKind.RENDERS_ON)
    store.upsert_edge(conn, "e2", "workflow:-/search_only", "workflow_step:-/wf2#1", EdgeKind.HAS_STEP)
    store.upsert_workflow(conn, "workflow:-/search_only", "search_only", "Search only", criticality=0.3)

    cm_id = nodes["cm"][0]
    change_id = "change-4"
    store.record_change_event(
        conn, change_id, datetime.now(timezone.utc).isoformat(),
        "CHANGE_SOURCE_KUBERNETES", "CHANGE_CLASS_CONFIG", "CHANGE_OPERATION_MODIFIED",
        object_node_id=cm_id, object_kind="ConfigMap", object_ns="travel", object_name="pricing-flags",
        hints={}, diffs=[], provenance={},
    )
    result = impact.compute_impact(conn, change_id, cm_id, "CHANGE_CLASS_CONFIG")

    assert len(result.risks) == 2
    assert result.risks[0].workflow_id == nodes["wf"][0]  # sorted descending by risk_score
    assert result.risks[0].risk_score > result.risks[1].risk_score
