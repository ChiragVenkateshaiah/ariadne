"""Business-workflow synthesis: the one place an LLM proposes graph facts
rather than Kubernetes observation providing them. Every node/edge written
here carries Discovery.LLM_INFERRED (see store.upsert_workflow) -- a
hypothesis about business intent, never treated as ground truth the way
ingest.py's topology is. workflows.reviewed_by_human exists precisely so a
human can promote a proposal to trusted fact; nothing here sets it.

Two phases:
  1. ingest_catalog: writes the hand-authored API/UI surface (catalog.py) as
     plain topology (Discovery.MANUAL) -- endpoints, routes, and the
     service-to-service CALLS edges that let a UI workflow step reach every
     transitively affected backend service.
  2. synthesize_workflows: asks the LLM what business workflows this topology
     implies, then materializes its answer as Workflow/WorkflowStep nodes
     wired to that catalog via HAS_STEP and RENDERS_ON/EXERCISES.
"""

from __future__ import annotations

import json
import sqlite3

from ariadne.graph import catalog, model, store
from ariadne.graph.model import Discovery, EdgeKind, NodeKind
from ariadne.llm.client import LLMClient

SYNTH_SYSTEM_PROMPT = """You are a QA architect studying a Kubernetes-hosted \
application's topology to identify the business workflows it implements. \
Given a catalog of API endpoints, UI routes, and service-to-service calls, \
propose the end-user workflows that exercise this system, each broken into \
ordered steps a browser-automation tool could execute.

Respond with ONLY a JSON object of this exact shape, no prose:
{
  "workflows": [
    {
      "slug": "snake_case_id",
      "title": "...", "description": "...", "business_goal": "...",
      "criticality": 0.0-1.0, "revenue_path": bool, "pii_involved": bool,
      "entry_route": "/path", "persona": "...", "derived_from": "...",
      "steps": [
        {"ordinal": 1, "intent": "...", "action": "navigate|fill|click|select|wait|assert|api_call",
         "target_hint": "..."|null, "value_expr": "..."|null, "assertion": "..."|null,
         "ui_route": "METHOD /path"|null}
      ]
    }
  ]
}"""


def ingest_catalog(conn: sqlite3.Connection) -> None:
    for ep in catalog.API_ENDPOINTS:
        ep_id = model.node_id(NodeKind.API_ENDPOINT, ep.namespace, f"{ep.method} {ep.path}")
        store.upsert_node(conn, ep_id, NodeKind.API_ENDPOINT, f"{ep.method} {ep.path}",
                           namespace=ep.namespace, discovery=Discovery.MANUAL,
                           attrs={"method": ep.method, "path": ep.path, "description": ep.description})
        svc_id = model.node_id(NodeKind.SERVICE, ep.namespace, ep.service_name)
        store.upsert_node(conn, svc_id, NodeKind.SERVICE, ep.service_name, namespace=ep.namespace,
                           confidence=0.5)  # placeholder-safe if the sensor hasn't reported it yet
        store.upsert_edge(conn, model.edge_id(ep_id, EdgeKind.SERVED_BY, svc_id), ep_id, svc_id,
                           EdgeKind.SERVED_BY, discovery=Discovery.MANUAL)

    for route in catalog.UI_ROUTES:
        route_id = model.node_id(NodeKind.UI_ROUTE, route.namespace, f"{route.method} {route.path}")
        store.upsert_node(conn, route_id, NodeKind.UI_ROUTE, f"{route.method} {route.path}",
                           namespace=route.namespace, discovery=Discovery.MANUAL,
                           attrs={"method": route.method, "path": route.path, "description": route.description})
        svc_id = model.node_id(NodeKind.SERVICE, route.namespace, route.service_name)
        store.upsert_node(conn, svc_id, NodeKind.SERVICE, route.service_name, namespace=route.namespace,
                           confidence=0.5)
        # SERVED_BY is defined for APIEndpoint->Service, but nothing in the
        # schema restricts it by node kind -- reusing it for UiRoute->Service
        # means "this surface is served by this Service" in both cases, and
        # is what lets a UI workflow step's transitive backend dependencies
        # be found by following existing edges alone (see catalog.py).
        store.upsert_edge(conn, model.edge_id(route_id, EdgeKind.SERVED_BY, svc_id), route_id, svc_id,
                           EdgeKind.SERVED_BY, discovery=Discovery.MANUAL)

    for caller, callee, ns in catalog.SERVICE_CALLS:
        caller_id = model.node_id(NodeKind.WORKLOAD, ns, caller)
        callee_id = model.node_id(NodeKind.SERVICE, ns, callee)
        store.upsert_node(conn, caller_id, NodeKind.WORKLOAD, caller, namespace=ns, confidence=0.5)
        store.upsert_node(conn, callee_id, NodeKind.SERVICE, callee, namespace=ns, confidence=0.5)
        store.upsert_edge(conn, model.edge_id(caller_id, EdgeKind.CALLS, callee_id), caller_id, callee_id,
                           EdgeKind.CALLS, discovery=Discovery.MANUAL)


def _build_prompt(conn: sqlite3.Connection) -> str:
    endpoints = [{"service": ep.service_name, "method": ep.method, "path": ep.path, "description": ep.description}
                 for ep in catalog.API_ENDPOINTS]
    routes = [{"service": r.service_name, "method": r.method, "path": r.path, "description": r.description}
              for r in catalog.UI_ROUTES]
    calls = [{"caller": c, "callee": e} for c, e, _ in catalog.SERVICE_CALLS]
    return json.dumps({"api_endpoints": endpoints, "ui_routes": routes, "service_calls": calls}, indent=2)


def synthesize_workflows(conn: sqlite3.Connection, llm: LLMClient) -> list[str]:
    """Returns the slugs of workflows written or updated."""
    ingest_catalog(conn)

    response = llm.complete_json("synth_workflows", SYNTH_SYSTEM_PROMPT, _build_prompt(conn))
    slugs: list[str] = []

    for wf in response.get("workflows", []):
        slug = wf["slug"]
        wf_id = model.node_id(NodeKind.WORKFLOW, None, slug)
        store.upsert_workflow(
            conn, wf_id, slug, wf["title"],
            description=wf.get("description"), business_goal=wf.get("business_goal"),
            criticality=float(wf.get("criticality", 0.5)), revenue_path=bool(wf.get("revenue_path", False)),
            pii_involved=bool(wf.get("pii_involved", False)), entry_route=wf.get("entry_route"),
            persona=wf.get("persona"), derived_from=wf.get("derived_from"),
        )

        for step in wf.get("steps", []):
            ordinal = int(step["ordinal"])
            step_id = model.node_id(NodeKind.WORKFLOW_STEP, None, f"{slug}#{ordinal}")
            store.upsert_workflow_step(
                conn, step_id, wf_id, ordinal, step["intent"], step["action"],
                target_hint=step.get("target_hint"), value_expr=step.get("value_expr"),
                assertion=step.get("assertion"), optional=bool(step.get("optional", False)),
            )
            store.upsert_edge(conn, model.edge_id(wf_id, EdgeKind.HAS_STEP, step_id), wf_id, step_id,
                               EdgeKind.HAS_STEP, discovery=Discovery.LLM_INFERRED, confidence=0.7, ordinal=ordinal)

            ui_route = step.get("ui_route")
            if ui_route:
                route_id = model.node_id(NodeKind.UI_ROUTE, "travel", ui_route)
                if conn.execute("SELECT 1 FROM nodes WHERE id=? AND active=1", (route_id,)).fetchone():
                    store.upsert_edge(conn, model.edge_id(step_id, EdgeKind.RENDERS_ON, route_id), step_id,
                                       route_id, EdgeKind.RENDERS_ON, discovery=Discovery.LLM_INFERRED, confidence=0.7)

            api_endpoint = step.get("api_endpoint")
            if api_endpoint:
                ep_id = model.node_id(NodeKind.API_ENDPOINT, "travel", api_endpoint)
                if conn.execute("SELECT 1 FROM nodes WHERE id=? AND active=1", (ep_id,)).fetchone():
                    store.upsert_edge(conn, model.edge_id(step_id, EdgeKind.EXERCISES, ep_id), step_id,
                                       ep_id, EdgeKind.EXERCISES, discovery=Discovery.LLM_INFERRED, confidence=0.7)

        slugs.append(slug)

    return slugs
