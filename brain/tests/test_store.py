"""store.py's confidence-aware merge is the one invariant the whole World
Model's trust model depends on: a later, weaker observation must never
downgrade a fact the graph already holds with higher confidence. See
store.py's upsert_node docstring.
"""

from ariadne.graph import store
from ariadne.graph.model import Discovery, NodeKind


def make_conn():
    return store.connect(":memory:")


def test_upsert_node_creates_new_node():
    conn = make_conn()
    store.upsert_node(conn, "service:travel/web-ui", NodeKind.SERVICE, "web-ui", namespace="travel")
    node = store.get_node(conn, "service:travel/web-ui")
    assert node is not None
    assert node.name == "web-ui"
    assert node.namespace == "travel"
    assert node.discovery == Discovery.K8S_API  # the default


def test_upsert_node_stronger_confidence_overwrites_weaker():
    conn = make_conn()
    store.upsert_node(conn, "n1", NodeKind.SERVICE, "svc", discovery=Discovery.LLM_INFERRED, confidence=0.5)
    store.upsert_node(conn, "n1", NodeKind.SERVICE, "svc", discovery=Discovery.K8S_API, confidence=1.0)
    node = store.get_node(conn, "n1")
    assert node.discovery == Discovery.K8S_API
    assert node.confidence == 1.0


def test_upsert_node_weaker_confidence_never_downgrades_stronger():
    conn = make_conn()
    store.upsert_node(conn, "n1", NodeKind.SERVICE, "svc", discovery=Discovery.K8S_API, confidence=1.0)
    # A later placeholder-style upsert (e.g. a reference discovered before its
    # own ADDED event arrives) must not clobber the fact we already trust more.
    store.upsert_node(conn, "n1", NodeKind.SERVICE, "svc", discovery=Discovery.LLM_INFERRED, confidence=0.5)
    node = store.get_node(conn, "n1")
    assert node.discovery == Discovery.K8S_API
    assert node.confidence == 1.0


def test_upsert_node_merges_attrs_rather_than_replacing():
    conn = make_conn()
    store.upsert_node(conn, "n1", NodeKind.SERVICE, "svc", attrs={"selector": {"app": "svc"}})
    store.upsert_node(conn, "n1", NodeKind.SERVICE, "svc", attrs={"type": "ClusterIP"})
    node = store.get_node(conn, "n1")
    assert node.attrs == {"selector": {"app": "svc"}, "type": "ClusterIP"}


def test_deactivate_node_is_soft_delete():
    conn = make_conn()
    store.upsert_node(conn, "n1", NodeKind.SERVICE, "svc")
    store.deactivate_node(conn, "n1")
    assert store.get_node(conn, "n1") is None  # get_node only returns active=1
    row = conn.execute("SELECT active FROM nodes WHERE id = ?", ("n1",)).fetchone()
    assert row is not None and row["active"] == 0  # but the row itself survives as history
