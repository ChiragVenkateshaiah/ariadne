"""network_policy.py generates least-privilege NetworkPolicies mechanically
from CALLS edges -- "no rules hand-written" (docs/ARCHITECTURE.md). These
tests pin the two correctness bugs found while building it: a service with
no in-cluster callers but externally exposed via NodePort needs an
allow-all policy, not a lockout, and postgres must be reachable from its
actual caller (a regression test for the SERVICE_CALLS catalog gap that
would have silently broken real traffic).
"""

from ariadne.graph import model as gmodel
from ariadne.graph import store
from ariadne.security.network_policy import generate_network_policies


def seed_service_and_workload(conn, name, namespace, match_labels, service_type="ClusterIP"):
    svc_id = gmodel.node_id(gmodel.NodeKind.SERVICE, namespace, name)
    wl_id = gmodel.node_id(gmodel.NodeKind.WORKLOAD, namespace, name)
    store.upsert_node(conn, svc_id, gmodel.NodeKind.SERVICE, name, namespace=namespace,
                       attrs={"selector": match_labels, "type": service_type})
    store.upsert_node(conn, wl_id, gmodel.NodeKind.WORKLOAD, name, namespace=namespace,
                       attrs={"match_labels": match_labels})
    store.upsert_edge(conn, f"{svc_id}|BACKED_BY|{wl_id}", svc_id, wl_id, gmodel.EdgeKind.BACKED_BY)
    return svc_id, wl_id


def add_call(conn, caller_wl_id, callee_svc_id):
    store.upsert_edge(conn, f"{caller_wl_id}|CALLS|{callee_svc_id}", caller_wl_id, callee_svc_id, gmodel.EdgeKind.CALLS)


def test_default_deny_ingress_is_always_first():
    conn = store.connect(":memory:")
    policies = generate_network_policies(conn, "travel")
    assert policies[0].name == "default-deny-ingress"
    assert policies[0].manifest["spec"]["podSelector"] == {}


def test_service_with_one_caller_gets_a_scoped_allow_policy():
    conn = store.connect(":memory:")
    _, web_ui_wl = seed_service_and_workload(conn, "web-ui", "travel", {"app": "web-ui"}, "NodePort")
    search_svc, _ = seed_service_and_workload(conn, "search-api", "travel", {"app": "search-api"})
    add_call(conn, web_ui_wl, search_svc)

    policies = {p.name: p for p in generate_network_policies(conn, "travel")}
    p = policies["allow-search-api-from-callers"]
    assert p.manifest["spec"]["podSelector"] == {"matchLabels": {"app": "search-api"}}
    assert p.manifest["spec"]["ingress"] == [{"from": [{"podSelector": {"matchLabels": {"app": "web-ui"}}}]}]


def test_nodeport_service_gets_allow_all_not_a_lockout():
    """The exact bug found while building this: web-ui has no in-cluster
    callers (it's the external entry point), so a caller-restricted policy
    would silently block the browser. This must never regress."""
    conn = store.connect(":memory:")
    seed_service_and_workload(conn, "web-ui", "travel", {"app": "web-ui"}, service_type="NodePort")

    policies = {p.name: p for p in generate_network_policies(conn, "travel")}
    assert "allow-web-ui-from-callers" not in policies
    p = policies["allow-web-ui-external-ingress"]
    assert p.manifest["spec"]["ingress"] == [{}]  # {} with no `from` means allow from anywhere


def test_clusterip_service_with_no_callers_denies_entirely():
    """Contrast case: an internal-only service with no callers is NOT
    externally exposed, so it correctly gets a deny (empty ingress list),
    not an allow-all -- only NodePort/LoadBalancer earns the allow-all."""
    conn = store.connect(":memory:")
    seed_service_and_workload(conn, "internal-only", "travel", {"app": "internal-only"}, service_type="ClusterIP")

    policies = {p.name: p for p in generate_network_policies(conn, "travel")}
    p = policies["allow-internal-only-from-callers"]
    assert p.manifest["spec"]["ingress"] == []


def test_service_with_no_backing_workload_is_skipped():
    conn = store.connect(":memory:")
    svc_id = gmodel.node_id(gmodel.NodeKind.SERVICE, "travel", "orphan-svc")
    store.upsert_node(conn, svc_id, gmodel.NodeKind.SERVICE, "orphan-svc", namespace="travel",
                       attrs={"selector": {}, "type": "ClusterIP"})
    policies = generate_network_policies(conn, "travel")
    names = {p.name for p in policies}
    assert "allow-orphan-svc-from-callers" not in names
    assert "allow-orphan-svc-external-ingress" not in names


def test_postgres_is_reachable_from_its_real_caller():
    """Regression test for the SERVICE_CALLS catalog gap: booking-api's real
    call to postgres was originally missing from the catalog, which would
    have generated a policy blocking real traffic."""
    conn = store.connect(":memory:")
    _, booking_wl = seed_service_and_workload(conn, "booking-api", "travel", {"app": "booking-api"})
    postgres_svc, _ = seed_service_and_workload(conn, "postgres", "travel", {"app": "postgres"})
    add_call(conn, booking_wl, postgres_svc)

    policies = {p.name: p for p in generate_network_policies(conn, "travel")}
    p = policies["allow-postgres-from-callers"]
    assert p.manifest["spec"]["ingress"] == [{"from": [{"podSelector": {"matchLabels": {"app": "booking-api"}}}]}]
