"""Topology ingester: turns the sensor's ChangeStream into graph nodes/edges.

This module is deliberately mechanical -- everything it writes carries
Discovery.K8S_API (ground truth) and is a direct translation of a Kubernetes
object's own spec, never an inference. Business semantics (Workflow,
WorkflowStep, EXERCISES, RENDERS_ON) are a separate, LLM-assisted layer built
on top of this topology (see synth.py) -- keeping that line sharp is what
lets QualityPolicy safely trust topology facts while treating inferred
workflows as hypotheses.

Events can arrive in any order across resource kinds (each GVR's informer
syncs independently), so a reference to a not-yet-seen object (e.g. a
Deployment mounting a ConfigMap the sensor hasn't reported yet) is handled by
creating a low-confidence placeholder node up front. When the real event
for that object arrives, store.upsert_node's confidence-aware merge upgrades
it in place -- see store.py's docstring for why that direction is safe and
the reverse (letting a later, weaker observation downgrade a fact) is not.
"""

from __future__ import annotations

import json
import sqlite3
from datetime import timezone

import grpc
from ariadne.v1 import change_pb2, change_pb2_grpc, common_pb2

from ariadne.graph import model, store
from ariadne.graph.model import EdgeKind, NodeKind

PLACEHOLDER_CONFIDENCE = 0.5

_KIND_KEY_MAP: dict[str, NodeKind] = {
    "Service": NodeKind.SERVICE,
    "Deployment": NodeKind.WORKLOAD,
    "ConfigMap": NodeKind.CONFIG_RESOURCE,
    "Secret": NodeKind.SECRET,
    "ServiceAccount": NodeKind.SERVICE_ACCOUNT,
    "NetworkPolicy": NodeKind.NETWORK_POLICY,
    "Ingress": NodeKind.INGRESS,
    "Role": NodeKind.ROLE,
    "ClusterRole": NodeKind.ROLE,
    "RoleBinding": NodeKind.ROLE_BINDING,
    "ClusterRoleBinding": NodeKind.ROLE_BINDING,
}


def run(conn: sqlite3.Connection, sensor_addr: str, subscriber_id: str = "graph-builder") -> None:
    """Blocks forever, applying each ChangeEvent as it arrives. Call inside a
    thread/process the caller is happy to have block; reconnection on
    transport failure is the caller's responsibility (this raises)."""
    channel = grpc.insecure_channel(sensor_addr)
    stub = change_pb2_grpc.ChangeStreamServiceStub(channel)
    request = change_pb2.SubscribeRequest(subscriber_id=subscriber_id)
    for ev in stub.Subscribe(request):
        with store.transaction(conn):
            handle_change_event(conn, ev)


def handle_change_event(conn: sqlite3.Connection, ev: change_pb2.ChangeEvent) -> None:
    obj = ev.object
    kind, ns, name = obj.kind, obj.namespace, obj.name

    node_kind = _KIND_KEY_MAP.get(kind)
    object_node_id = model.node_id(node_kind, ns, name) if node_kind is not None else None

    # Topology ingestion (which creates the node, if any) MUST happen before
    # record_change_event: change_events.object_node_id has a foreign key
    # into nodes(id), and for a brand-new object this is the very event that
    # brings that node into existence. Recording the event first would fail
    # the very first time we ever see any object.
    if ev.operation == change_pb2.CHANGE_OPERATION_DELETED:
        if node_kind is not None:
            store.deactivate_node(conn, object_node_id)
        _record_event(conn, ev, object_node_id)
        return

    raw = json.loads(ev.raw_object_json) if ev.raw_object_json else {}

    if kind == "Service":
        _ingest_service(conn, ns, name, raw)
    elif kind == "Deployment":
        _ingest_workload(conn, ns, name, raw)
    elif kind == "ConfigMap":
        _ingest_simple(conn, NodeKind.CONFIG_RESOURCE, ns, name)
    elif kind == "Secret":
        _ingest_simple(conn, NodeKind.SECRET, ns, name)
    elif kind == "ServiceAccount":
        _ingest_simple(conn, NodeKind.SERVICE_ACCOUNT, ns, name)
    elif kind in ("Role", "ClusterRole"):
        _ingest_simple(conn, NodeKind.ROLE, ns if kind == "Role" else None, name)
    elif kind == "NetworkPolicy":
        _ingest_network_policy(conn, ns, name, raw)
    elif kind == "Ingress":
        _ingest_ingress(conn, ns, name, raw)
    elif kind in ("RoleBinding", "ClusterRoleBinding"):
        _ingest_role_binding(conn, ns if kind == "RoleBinding" else None, name, raw)
    # HorizontalPodAutoscaler: no dedicated NodeKind (scaling is tracked as a
    # change_events fact, not a topology node) -- intentionally not handled.

    _record_event(conn, ev, object_node_id)


def _record_event(conn: sqlite3.Connection, ev: change_pb2.ChangeEvent, object_node_id: str | None) -> None:
    obj = ev.object
    store.record_change_event(
        conn, ev.id, ev.observed_at.ToDatetime(tzinfo=timezone.utc).isoformat(),
        change_pb2.ChangeSource.Name(ev.source), change_pb2.ChangeClass.Name(ev.change_class),
        change_pb2.ChangeOperation.Name(ev.operation),
        object_node_id=object_node_id, object_kind=obj.kind, object_ns=obj.namespace, object_name=obj.name,
        hints=_hints_to_dict(ev.hints),
        diffs=[{"path": d.path, "before": d.before, "after": d.after, "op": common_pb2.DiffOp.Name(d.op)} for d in ev.diffs],
        provenance={"manager": ev.provenance.manager, "actor_kind": ev.provenance.actor_kind},
    )


def _hints_to_dict(hints: change_pb2.ChangeHints) -> dict:
    return {
        "affects_running_traffic": hints.affects_running_traffic,
        "affects_security_posture": hints.affects_security_posture,
        "affects_api_surface": hints.affects_api_surface,
        "affects_configuration": hints.affects_configuration,
        "is_scale_only": hints.is_scale_only,
        "is_noise": hints.is_noise,
        "changed_field_count": hints.changed_field_count,
        "touched_workload_names": list(hints.touched_workload_names),
    }


def _ingest_simple(conn: sqlite3.Connection, kind: NodeKind, ns: str | None, name: str,
                    confidence: float = 1.0) -> str:
    nid = model.node_id(kind, ns, name)
    store.upsert_node(conn, nid, kind, name, namespace=ns, confidence=confidence)
    return nid


def _ingest_service(conn: sqlite3.Connection, ns: str, name: str, raw: dict) -> None:
    nid = model.node_id(NodeKind.SERVICE, ns, name)
    spec = raw.get("spec", {}) or {}
    selector = spec.get("selector") or {}
    store.upsert_node(conn, nid, NodeKind.SERVICE, name, namespace=ns,
                       attrs={"selector": selector, "ports": spec.get("ports") or [],
                              "type": spec.get("type") or "ClusterIP"})
    _relink_service(conn, ns, nid, selector)


def _ingest_workload(conn: sqlite3.Connection, ns: str, name: str, raw: dict) -> None:
    nid = model.node_id(NodeKind.WORKLOAD, ns, name)
    spec = raw.get("spec", {}) or {}
    match_labels = ((spec.get("selector") or {}).get("matchLabels")) or {}
    pod_spec = ((spec.get("template") or {}).get("spec")) or {}
    containers = pod_spec.get("containers") or []
    image = containers[0].get("image") if containers else None
    service_account = pod_spec.get("serviceAccountName") or pod_spec.get("serviceAccount") or "default"

    store.upsert_node(conn, nid, NodeKind.WORKLOAD, name, namespace=ns,
                       attrs={"match_labels": match_labels, "image": image})

    sa_id = _ingest_simple(conn, NodeKind.SERVICE_ACCOUNT, ns, service_account, confidence=PLACEHOLDER_CONFIDENCE)
    store.upsert_edge(conn, model.edge_id(nid, EdgeKind.RUNS_AS, sa_id), nid, sa_id, EdgeKind.RUNS_AS)

    store.deactivate_edges_from(conn, nid, EdgeKind.MOUNTS)
    for ref_kind, ref_name in _extract_config_refs(pod_spec):
        ref_node_kind = NodeKind.CONFIG_RESOURCE if ref_kind == "ConfigMap" else NodeKind.SECRET
        ref_id = _ingest_simple(conn, ref_node_kind, ns, ref_name, confidence=PLACEHOLDER_CONFIDENCE)
        store.upsert_edge(conn, model.edge_id(nid, EdgeKind.MOUNTS, ref_id), nid, ref_id, EdgeKind.MOUNTS)

    _relink_workload(conn, ns, nid, match_labels)
    _relink_network_policies(conn, ns, nid, match_labels)


def _extract_config_refs(pod_spec: dict) -> list[tuple[str, str]]:
    refs: list[tuple[str, str]] = []
    for vol in pod_spec.get("volumes") or []:
        if "configMap" in vol:
            refs.append(("ConfigMap", vol["configMap"].get("name")))
        if "secret" in vol:
            refs.append(("Secret", vol["secret"].get("secretName")))
    for c in pod_spec.get("containers") or []:
        for ef in c.get("envFrom") or []:
            if "configMapRef" in ef:
                refs.append(("ConfigMap", ef["configMapRef"].get("name")))
            if "secretRef" in ef:
                refs.append(("Secret", ef["secretRef"].get("name")))
        for e in c.get("env") or []:
            vf = e.get("valueFrom") or {}
            if "configMapKeyRef" in vf:
                refs.append(("ConfigMap", vf["configMapKeyRef"].get("name")))
            if "secretKeyRef" in vf:
                refs.append(("Secret", vf["secretKeyRef"].get("name")))
    return [(k, n) for k, n in refs if n]


def _labels_match(selector: dict, candidate_labels: dict) -> bool:
    """True if every key/value in `selector` is present in `candidate_labels`
    -- an empty selector matches everything, which is correct Kubernetes
    NetworkPolicy/Service selector semantics, not a special case."""
    return selector.items() <= candidate_labels.items()


def _relink_service(conn: sqlite3.Connection, ns: str, service_id: str, selector: dict) -> None:
    for row in conn.execute(
        "SELECT id, attrs FROM nodes WHERE kind=? AND namespace=? AND active=1", (NodeKind.WORKLOAD.value, ns)
    ):
        match_labels = json.loads(row["attrs"]).get("match_labels", {})
        if _labels_match(selector, match_labels):
            store.upsert_edge(conn, model.edge_id(service_id, EdgeKind.BACKED_BY, row["id"]),
                               service_id, row["id"], EdgeKind.BACKED_BY)


def _relink_workload(conn: sqlite3.Connection, ns: str, workload_id: str, match_labels: dict) -> None:
    for row in conn.execute(
        "SELECT id, attrs FROM nodes WHERE kind=? AND namespace=? AND active=1", (NodeKind.SERVICE.value, ns)
    ):
        selector = json.loads(row["attrs"]).get("selector", {})
        if selector and _labels_match(selector, match_labels):
            store.upsert_edge(conn, model.edge_id(row["id"], EdgeKind.BACKED_BY, workload_id),
                               row["id"], workload_id, EdgeKind.BACKED_BY)


def _relink_network_policies(conn: sqlite3.Connection, ns: str, workload_id: str, match_labels: dict) -> None:
    for row in conn.execute(
        "SELECT id, attrs FROM nodes WHERE kind=? AND namespace=? AND active=1", (NodeKind.NETWORK_POLICY.value, ns)
    ):
        pod_selector = json.loads(row["attrs"]).get("pod_selector", {})
        if _labels_match(pod_selector, match_labels):
            store.upsert_edge(conn, model.edge_id(row["id"], EdgeKind.GOVERNS, workload_id),
                               row["id"], workload_id, EdgeKind.GOVERNS)


def _ingest_network_policy(conn: sqlite3.Connection, ns: str, name: str, raw: dict) -> None:
    nid = model.node_id(NodeKind.NETWORK_POLICY, ns, name)
    pod_selector = ((raw.get("spec") or {}).get("podSelector") or {}).get("matchLabels") or {}
    store.upsert_node(conn, nid, NodeKind.NETWORK_POLICY, name, namespace=ns, attrs={"pod_selector": pod_selector})

    store.deactivate_edges_from(conn, nid, EdgeKind.GOVERNS)
    for row in conn.execute(
        "SELECT id, attrs FROM nodes WHERE kind=? AND namespace=? AND active=1", (NodeKind.WORKLOAD.value, ns)
    ):
        match_labels = json.loads(row["attrs"]).get("match_labels", {})
        if _labels_match(pod_selector, match_labels):
            store.upsert_edge(conn, model.edge_id(nid, EdgeKind.GOVERNS, row["id"]), nid, row["id"], EdgeKind.GOVERNS)


def _ingest_ingress(conn: sqlite3.Connection, ns: str, name: str, raw: dict) -> None:
    nid = model.node_id(NodeKind.INGRESS, ns, name)
    store.upsert_node(conn, nid, NodeKind.INGRESS, name, namespace=ns)

    store.deactivate_edges_from(conn, nid, EdgeKind.ROUTES_TO)
    svc_names: set[str] = set()
    for rule in (raw.get("spec") or {}).get("rules") or []:
        for path in ((rule.get("http") or {}).get("paths")) or []:
            svc_name = ((path.get("backend") or {}).get("service") or {}).get("name")
            if svc_name:
                svc_names.add(svc_name)
    for svc_name in svc_names:
        svc_id = _ingest_simple(conn, NodeKind.SERVICE, ns, svc_name, confidence=PLACEHOLDER_CONFIDENCE)
        store.upsert_edge(conn, model.edge_id(nid, EdgeKind.ROUTES_TO, svc_id), nid, svc_id, EdgeKind.ROUTES_TO)


def _ingest_role_binding(conn: sqlite3.Connection, ns: str | None, name: str, raw: dict) -> None:
    nid = model.node_id(NodeKind.ROLE_BINDING, ns, name)
    store.upsert_node(conn, nid, NodeKind.ROLE_BINDING, name, namespace=ns)

    role_ref = raw.get("roleRef") or {}
    role_name = role_ref.get("name")
    if role_name:
        role_ns = ns if role_ref.get("kind") == "Role" else None
        role_id = _ingest_simple(conn, NodeKind.ROLE, role_ns, role_name, confidence=PLACEHOLDER_CONFIDENCE)
        store.upsert_edge(conn, model.edge_id(nid, EdgeKind.GRANTS, role_id), nid, role_id, EdgeKind.GRANTS)

    store.deactivate_edges_from(conn, nid, EdgeKind.BINDS_TO)
    for subj in raw.get("subjects") or []:
        if subj.get("kind") != "ServiceAccount":
            continue
        subj_ns = subj.get("namespace") or ns
        subj_name = subj.get("name")
        if not subj_name:
            continue
        sa_id = _ingest_simple(conn, NodeKind.SERVICE_ACCOUNT, subj_ns, subj_name, confidence=PLACEHOLDER_CONFIDENCE)
        store.upsert_edge(conn, model.edge_id(nid, EdgeKind.BINDS_TO, sa_id), nid, sa_id, EdgeKind.BINDS_TO)
