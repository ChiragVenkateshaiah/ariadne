"""Generates least-privilege NetworkPolicies from the World Model's CALLS
edges -- "auto-generated from topology, no rules hand-written" per
docs/ARCHITECTURE.md. Two kinds of policy come out:

  1. One default-deny-all-ingress policy for the namespace. Kubernetes'
     baseline with zero policies is "everything reachable"; this flips that.
  2. One allow-ingress policy per Service whose backing Workload receives
     traffic, with `from.podSelector` set to exactly the Workloads that have
     a CALLS edge pointing at that Service -- a direct, mechanical
     translation of the graph, never a guess about what SHOULD be allowed.

Anything NOT declared as a CALLS edge -- e.g. web-ui -> postgres, which the
real code never does -- is therefore correctly, automatically left
unreachable once these are applied. That's the whole Act 3 network story:
prove it by generating the policy from truth and then testing the gap.

This module only produces manifests (dicts); it does not apply them --
kubectl (or a Go component) does that, keeping with "Go/kubectl talks to
Kubernetes, Python reasons" (docs/ARCHITECTURE.md's stack split covers CLI
application of static manifests as an acceptable boundary crossing, same as
any human operator would use).
"""

from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass
from typing import Any

from ariadne.graph import model as gmodel


@dataclass(slots=True)
class GeneratedPolicy:
    name: str
    namespace: str
    manifest: dict[str, Any]
    rationale: str


def generate_network_policies(conn: sqlite3.Connection, namespace: str = "travel") -> list[GeneratedPolicy]:
    policies = [_deny_all(namespace)]

    rows = conn.execute(
        "SELECT id, name, attrs FROM nodes WHERE kind = ? AND namespace = ? AND active = 1",
        (gmodel.NodeKind.SERVICE.value, namespace),
    ).fetchall()
    services = [{"id": r["id"], "name": r["name"], "service_type": json.loads(r["attrs"]).get("type", "ClusterIP")}
                for r in rows]

    for svc in services:
        policy = _allow_policy_for_service(conn, namespace, svc["id"], svc["name"], svc["service_type"])
        if policy is not None:
            policies.append(policy)

    return policies


def _allow_policy_for_service(conn: sqlite3.Connection, namespace: str, service_id: str,
                                service_name: str, service_type: str) -> GeneratedPolicy | None:
    backing = conn.execute(
        """SELECT w.name, w.attrs FROM edges e JOIN nodes w ON w.id = e.dst_id
           WHERE e.src_id = ? AND e.kind = ? AND e.active = 1""",
        (service_id, gmodel.EdgeKind.BACKED_BY.value),
    ).fetchall()
    if not backing:
        return None  # nothing backs this Service yet -- nothing to write a policy for

    target_labels = json.loads(backing[0]["attrs"]).get("match_labels", {})
    if not target_labels:
        return None

    if service_type in ("NodePort", "LoadBalancer"):
        # Externally-exposed services receive traffic that never originates
        # from a pod matching any in-cluster podSelector (kube-proxy forwards
        # it directly), so a caller-restricted policy would silently lock out
        # every legitimate external user -- including, for this demo, the
        # browser hitting web-ui. Generating a caller-list here instead of an
        # explicit allow-all would be a correctness bug, not a stricter
        # policy: it isn't "more secure," it's "broken."
        name = f"allow-{service_name}-external-ingress"
        return GeneratedPolicy(
            name=name, namespace=namespace,
            rationale=f"{service_name} is a {service_type} Service (externally exposed) -- ingress "
                      f"is allowed from any source, since NodePort/LoadBalancer traffic does not "
                      f"originate from an in-cluster pod a podSelector could match.",
            manifest={
                "apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                "metadata": {"name": name, "namespace": namespace, "labels": {"ariadne.dev/generated": "true"}},
                "spec": {"podSelector": {"matchLabels": target_labels}, "policyTypes": ["Ingress"],
                         "ingress": [{}]},
            },
        )

    callers = conn.execute(
        """SELECT w.name, w.attrs FROM edges e JOIN nodes w ON w.id = e.src_id
           WHERE e.dst_id = ? AND e.kind = ? AND e.active = 1 AND w.kind = ?""",
        (service_id, gmodel.EdgeKind.CALLS.value, gmodel.NodeKind.WORKLOAD.value),
    ).fetchall()

    ingress_from = []
    caller_names = []
    for c in callers:
        caller_labels = json.loads(c["attrs"]).get("match_labels", {})
        if caller_labels:
            ingress_from.append({"podSelector": {"matchLabels": caller_labels}})
            caller_names.append(c["name"])

    name = f"allow-{service_name}-from-callers"
    rationale = (
        f"{service_name} is reachable from {', '.join(caller_names)} (each has a CALLS edge "
        f"to it in the World Model) and from nothing else."
        if caller_names else
        f"{service_name} has no known callers in the World Model -- ingress is denied entirely."
    )

    return GeneratedPolicy(
        name=name, namespace=namespace, rationale=rationale,
        manifest={
            "apiVersion": "networking.k8s.io/v1",
            "kind": "NetworkPolicy",
            "metadata": {"name": name, "namespace": namespace, "labels": {"ariadne.dev/generated": "true"}},
            "spec": {
                "podSelector": {"matchLabels": target_labels},
                "policyTypes": ["Ingress"],
                "ingress": [{"from": ingress_from}] if ingress_from else [],
            },
        },
    )


def _deny_all(namespace: str) -> GeneratedPolicy:
    name = "default-deny-ingress"
    return GeneratedPolicy(
        name=name, namespace=namespace,
        rationale="Kubernetes' baseline with no policies is unrestricted ingress; this establishes "
                  "deny-by-default so every allow below is an explicit, graph-derived grant.",
        manifest={
            "apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
            "metadata": {"name": name, "namespace": namespace, "labels": {"ariadne.dev/generated": "true"}},
            "spec": {"podSelector": {}, "policyTypes": ["Ingress"]},
        },
    )
