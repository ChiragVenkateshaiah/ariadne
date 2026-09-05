"""Typed vocabulary for the Ariadne world model.

These enums are the schema. `schema.sql` stores their *values* as TEXT, so any
string that is not defined here is a bug -- validate on write, never on read.

The one rule that matters when extending this: node and edge kinds describe
either observed topology (from the Kubernetes API, an OpenAPI document, or a UI
crawl) or inferred business semantics (from an LLM). `Discovery` records which,
and nothing that was merely inferred is ever allowed to block a release on its
own.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum
from typing import Any


class NodeKind(str, Enum):
    # --- observed from the cluster -----------------------------------------
    SERVICE = "SERVICE"
    WORKLOAD = "WORKLOAD"                    # Deployment / StatefulSet / DaemonSet
    INGRESS = "INGRESS"
    CONFIG_RESOURCE = "CONFIG_RESOURCE"      # ConfigMap
    SECRET = "SECRET"                        # metadata only, never contents
    NETWORK_POLICY = "NETWORK_POLICY"
    SERVICE_ACCOUNT = "SERVICE_ACCOUNT"
    ROLE = "ROLE"                            # Role or ClusterRole
    ROLE_BINDING = "ROLE_BINDING"
    DATASTORE = "DATASTORE"                  # postgres, redis, ...
    EXTERNAL_DEPENDENCY = "EXTERNAL_DEPENDENCY"  # third-party API outside the mesh

    # --- observed from contracts and the UI --------------------------------
    API_ENDPOINT = "API_ENDPOINT"            # one OpenAPI operation
    UI_ROUTE = "UI_ROUTE"

    # --- inferred business semantics ---------------------------------------
    WORKFLOW = "WORKFLOW"
    WORKFLOW_STEP = "WORKFLOW_STEP"


class EdgeKind(str, Enum):
    HAS_STEP = "HAS_STEP"          # Workflow -> WorkflowStep (ordinal-ordered)
    EXERCISES = "EXERCISES"        # WorkflowStep -> APIEndpoint
    RENDERS_ON = "RENDERS_ON"      # WorkflowStep -> UIRoute
    SERVED_BY = "SERVED_BY"        # APIEndpoint -> Service
    BACKED_BY = "BACKED_BY"        # Service -> Workload
    CALLS = "CALLS"                # Workload -> Service (service-to-service)
    RUNS_AS = "RUNS_AS"            # Workload -> ServiceAccount
    MOUNTS = "MOUNTS"              # Workload -> ConfigResource | Secret
    ROUTES_TO = "ROUTES_TO"        # Ingress -> Service
    GOVERNS = "GOVERNS"            # NetworkPolicy -> Workload
    GRANTS = "GRANTS"              # RoleBinding -> Role
    BINDS_TO = "BINDS_TO"          # RoleBinding -> ServiceAccount
    READS_FROM = "READS_FROM"      # Workload -> Datastore
    WRITES_TO = "WRITES_TO"        # Workload -> Datastore
    COVERS = "COVERS"              # TestSpec -> Workflow
    DEPENDS_ON = "DEPENDS_ON"      # materialised transitive closure (cache)


class Discovery(str, Enum):
    """How we learned a fact. Confidence in a conclusion can never exceed the
    confidence of the weakest link that produced it."""

    K8S_API = "K8S_API"            # ground truth
    OPENAPI = "OPENAPI"            # ground truth, if the spec is honest
    UI_CRAWL = "UI_CRAWL"          # observed
    LOG_OBSERVED = "LOG_OBSERVED"  # inferred from real traffic
    LLM_INFERRED = "LLM_INFERRED"  # a hypothesis, not a fact
    MANUAL = "MANUAL"              # a human said so; outranks everything


class ValidatorKind(str, Enum):
    UI = "UI"
    API = "API"
    ACCESSIBILITY = "ACCESSIBILITY"
    PERFORMANCE = "PERFORMANCE"
    SECURITY_APP = "SECURITY_APP"
    SECURITY_CLUSTER = "SECURITY_CLUSTER"
    SECURITY_AUDIT = "SECURITY_AUDIT"
    NETWORK_CONFORMANCE = "NETWORK_CONFORMANCE"
    RESILIENCE = "RESILIENCE"


class Adjudication(str, Enum):
    """Why a test failed. The whole system exists to answer this correctly.

    Healing a failure that is really APP_REGRESSION is worse than not healing at
    all: it converts a caught bug into an escaped one. Enforce in code that no
    heal is ever written against APP_REGRESSION.
    """

    TEST_DEFECT = "TEST_DEFECT"          # locator drift -> safe to heal
    INTENT_DRIFT = "INTENT_DRIFT"        # app changed on purpose -> update spec
    APP_REGRESSION = "APP_REGRESSION"    # real bug -> never heal; gate the release
    ENV_FLAKE = "ENV_FLAKE"              # infra noise -> retry, touch nothing
    UNDETERMINED = "UNDETERMINED"        # escalate to a human, with evidence


class Verdict(str, Enum):
    PASS = "PASS"
    PASS_HEALED = "PASS_HEALED"
    WARN = "WARN"
    FAIL = "FAIL"
    BLOCKED = "BLOCKED"
    INCONCLUSIVE = "INCONCLUSIVE"


class Severity(str, Enum):
    INFO = "INFO"
    LOW = "LOW"
    MEDIUM = "MEDIUM"
    HIGH = "HIGH"
    CRITICAL = "CRITICAL"


class FindingCategory(str, Enum):
    FUNCTIONAL = "FUNCTIONAL"
    SECURITY_APP = "SECURITY_APP"
    SECURITY_K8S = "SECURITY_K8S"
    PERFORMANCE = "PERFORMANCE"
    ACCESSIBILITY = "ACCESSIBILITY"
    RESILIENCE = "RESILIENCE"
    COVERAGE_GAP = "COVERAGE_GAP"
    CONFIG_DRIFT = "CONFIG_DRIFT"


class BindingStrategy(str, Enum):
    """Ordered best-to-worst. The resolver always tries to bind an intent using
    the most semantic strategy available, because semantic locators survive
    refactors that positional ones do not -- that is where the maintenance
    saving actually comes from.
    """

    ROLE_NAME = "ROLE_NAME"    # getByRole("button", name="Search")
    TEST_ID = "TEST_ID"
    LABEL = "LABEL"
    PLACEHOLDER = "PLACEHOLDER"
    TEXT = "TEXT"
    CSS = "CSS"
    XPATH = "XPATH"            # last resort; flag any spec that relies on it

    @property
    def rank(self) -> int:
        return list(BindingStrategy).index(self)


class StepAction(str, Enum):
    NAVIGATE = "navigate"
    FILL = "fill"
    CLICK = "click"
    SELECT = "select"
    WAIT = "wait"
    ASSERT = "assert"
    API_CALL = "api_call"


def node_id(kind: NodeKind, namespace: str | None, name: str) -> str:
    """Canonical node id: stable, human-readable, and safe to embed in logs.

    Deliberately not the Kubernetes UID -- a node must be addressable before it
    exists in the cluster (a workflow the LLM proposed, an endpoint read from an
    OpenAPI file). `nodes.k8s_uid` carries the UID when there is one.
    """
    ns = namespace or "-"
    return f"{kind.value.lower()}:{ns}/{name}"


def edge_id(src: str, kind: EdgeKind, dst: str) -> str:
    return f"{src}|{kind.value}|{dst}"


@dataclass(slots=True)
class Node:
    id: str
    kind: NodeKind
    name: str
    namespace: str | None = None
    k8s_uid: str | None = None
    display_name: str | None = None
    discovery: Discovery = Discovery.K8S_API
    confidence: float = 1.0
    attrs: dict[str, Any] = field(default_factory=dict)
    first_seen: datetime | None = None
    last_seen: datetime | None = None
    active: bool = True


@dataclass(slots=True)
class Edge:
    id: str
    src_id: str
    dst_id: str
    kind: EdgeKind
    discovery: Discovery = Discovery.K8S_API
    confidence: float = 1.0
    weight: float = 1.0
    ordinal: int | None = None
    attrs: dict[str, Any] = field(default_factory=dict)
    first_seen: datetime | None = None
    last_seen: datetime | None = None
    active: bool = True


@dataclass(slots=True)
class WorkflowRisk:
    """Output of impact analysis: why this workflow is worth running now.

    risk_score is deliberately a weighted blend rather than an LLM number, so it
    is reproducible and explainable. The LLM writes the `reason` prose; it does
    not invent the score.
    """

    workflow_id: str
    risk_score: float
    hop_distance: int
    criticality: float
    coverage_gap: float
    flakiness: float = 0.0
    reason: str = ""
    selected: bool = False


# Weights for the blend above. Tuned by hand, documented so a judge can ask
# "why did it pick that?" and get a real answer instead of "the model decided".
RISK_WEIGHTS: dict[str, float] = {
    "criticality": 0.35,       # revenue/PII paths dominate
    "proximity": 0.25,         # 1/(1+hops) from the changed object
    "coverage_gap": 0.20,      # untested surface is the riskiest surface
    "change_class": 0.15,      # config/security changes outrank scaling
    "flakiness_penalty": 0.05, # de-prioritise chronically noisy specs
}

# Per-ChangeClass multiplier feeding the "change_class" term. A ConfigMap edit
# can silently change business behaviour with no image change at all, which is
# exactly the case traditional CI-triggered QA misses entirely.
CHANGE_CLASS_RISK: dict[str, float] = {
    "CHANGE_CLASS_CONFIG": 1.00,
    "CHANGE_CLASS_RBAC": 0.95,
    "CHANGE_CLASS_NETWORK_POLICY": 0.95,
    "CHANGE_CLASS_API_CONTRACT": 0.90,
    "CHANGE_CLASS_WORKLOAD_IMAGE": 0.85,
    "CHANGE_CLASS_ROUTE": 0.80,
    "CHANGE_CLASS_WORKLOAD_SPEC": 0.70,
    "CHANGE_CLASS_UI_SURFACE": 0.70,
    "CHANGE_CLASS_SECRET": 0.60,
    "CHANGE_CLASS_SERVICE": 0.55,
    "CHANGE_CLASS_POLICY": 0.50,
    "CHANGE_CLASS_SOURCE_COMMIT": 0.50,
    "CHANGE_CLASS_CRD": 0.40,
    "CHANGE_CLASS_SCALING": 0.20,
}
