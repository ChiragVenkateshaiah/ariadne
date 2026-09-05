# Ariadne — Autonomous Quality Engineering for Kubernetes

*The thread that follows a business workflow through the service maze.*

## The claim

Everyone builds "LLM reads the DOM, rewrites the broken selector." That demo has
a fatal flaw worth attacking directly: **naive self-healing is a liability.** A
test that heals around a real regression has converted a caught bug into an
escaped one.

To decide whether a failing test is a **broken test** or a **broken
application**, you need evidence the test runner does not have. That evidence
lives in the cluster. That single sentence is the justification for every
Kubernetes component below.

## Two planes, one contract

**Go does everything that talks to Kubernetes. Python does everything that
thinks.** Nothing is duplicated; neither side does a job it is bad at.

| Control plane (Go) | Intelligence plane (Python) |
| --- | --- |
| Cluster Sensor — informers → `ChangeEvent` stream | World-model graph + impact traversal |
| Operator + CRDs (`QualityPolicy`, `QualityAssessment`) | Test synthesis (Intent Specs) |
| Job orchestration & lifecycle | Runtime Resolver + Playwright |
| Log + audit-log collection (concurrent, windowed) | Evidence correlation & root-cause narration |
| Network prober (`scratch` binary) | Adjudicator (heal vs. bug) |
| The system under test (5 services) | Dashboard |

Note the Go argument is *not* performance — the latency budget is dominated by
LLM calls. It is correctness at the K8s API layer (informer caches, workqueues,
resync, leader election), credibility with platform engineers, and two jobs Go
is genuinely better at: concurrent fan-out log collection, and a 5MB static
prober that can be injected into any namespace.

## Contract

`proto/ariadne/v1/` is the frozen boundary. Four services:

| Service | Direction | Purpose |
| --- | --- | --- |
| `ChangeStream` | Go → Py | server-streaming `ChangeEvent`; plus `Replay` for demo safety |
| `Orchestrator` | Py → Go | `RunValidators` → stream `ValidatorResult`; `FaultInjector` for resilience |
| `LogCollector` | Py → Go | `CollectEvidence(pods, window)` → `EvidenceBundle`; audit-log queries |
| `VerdictService` | Py → Go | `PublishAssessment` → writes CR status → gates the rollout |

Two deliberate decoupling decisions:

1. **`ValidatorTask.spec_json` is opaque to Go.** The orchestrator ships it to
   the runner as a mounted file and never parses it. The Intent Spec format can
   change hourly without recompiling the control plane.
2. **Runners report via a stdout sentinel** (`###ARIADNE-RESULT###` + one JSON
   line), which the orchestrator extracts into `result_payload_json`. No inbound
   gRPC server needed on the Python side.

The sensor is **dumb but rich**: it normalises and diffs, and computes cheap
deterministic `ChangeHints`, but never interprets. Interpretation is the brain's
job, and keeping that line clean is what lets semantics evolve without
redeploying Go.

## World model

`brain/ariadne/graph/schema.sql` + `model.py`.

```
Workflow --HAS_STEP--> Step --EXERCISES--> Endpoint --SERVED_BY--> Service
             |                                                        |
             +--RENDERS_ON--> UIRoute                Service --BACKED_BY--> Workload
                                                                        |
                                                          Workload --CALLS--> Service
```

This graph does two jobs no LLM-only approach can:

- **Risk-based selection becomes a traversal**, not a guess. A change maps to a
  node; reverse traversal yields the workflows at risk, ranked by a documented
  weighted blend (`RISK_WEIGHTS`) — the LLM writes the *prose*, not the score.
- **Evidence collection becomes precise.** When a workflow fails, the graph
  already knows exactly which pods served it, so log collection is targeted
  rather than a cluster-wide grep.

`Discovery` on every node and edge records whether a fact was observed
(`K8S_API`, `OPENAPI`, `UI_CRAWL`) or merely inferred (`LLM_INFERRED`). Nothing
inferred may block a release on its own.

## Self-healing, structurally

Tests are stored as **Intent Specs** (`enter the origin airport`), never as
scripts. A runtime **Resolver** binds each intent to a locator against the live
accessibility tree and caches it in `intent_bindings`, preferring semantic
strategies (`ROLE_NAME`, `TEST_ID`) over positional ones (`CSS`, `XPATH`).

Selector drift therefore costs **one cheap re-resolve**, not a test rewrite.
That is the structural reason maintenance drops — the 70–80% figure is measured
in `QualityMetrics`, not asserted.

## Adjudication — the heart

Every failure lands in exactly one bucket, with reasoning recorded:

| Adjudication | Action |
| --- | --- |
| `TEST_DEFECT` | locator drift → safe to heal |
| `INTENT_DRIFT` | app changed on purpose → update the spec |
| `APP_REGRESSION` | **never heal** → file it, gate the release |
| `ENV_FLAKE` | retry, touch nothing |
| `UNDETERMINED` | escalate to a human, with the evidence attached |

**Invariant to enforce in code: a `heals` row may never carry
`APP_REGRESSION`.** Violating it is a bug in Ariadne itself.

Inputs to the decision: the failure, change provenance from the sensor, and the
correlated `EvidenceBundle` (pod logs + K8s events + audit slice + recent
changes, all in one payload).

## Kubernetes-specific value

**Change detection at the control plane, not at commit time.** Deploy-time truth
beats commit-time guessing — especially when an AI wrote the commit message. A
ConfigMap edit can silently change business behaviour with no image change at
all; CI-triggered QA misses that case entirely.

**Networking.** NetworkPolicy conformance is proven by *reachability*, not by
reading YAML: the prober execs and actually attempts the connection (OWASP K8s
K07). Resilience is tested by injecting latency/errors via toxiproxy and
re-running the *business workflow* — catching "infinite spinner, no timeout
handling at p99 payment latency", a defect class nobody tests automatically.

**Security by behaviour, not posture alone.** Posture scanning is a commodity
(shell out to Trivy/kube-bench and let the LLM do the *ranking*). The
differentiator is the API-server audit log: *"this ServiceAccount is granted 47
permissions and exercised 3 — here is the tightened Role, ready to apply"*
(K03). And change-correlated authz regressions: a rollout followed by a 403
spike from the new pod's SA means the new code calls an API it is not permitted
to — a real pre-production bug, caught by correlation.

**The release decision becomes control-plane state.** The verdict lands as a
`QualityAssessment` CR status, so Argo Rollouts / Flux gate on it natively
instead of a human reading a dashboard.

## Demo (7 minutes)

Acts 1 and 2 are mandatory; Act 3 is the differentiator.

1. **Setup.** Booking app in kind: `web-ui`, `search-api`, `pricing-svc`,
   `booking-api`, `payment-svc`, `postgres`. Agent builds the graph live,
   generates 12 workflows. *"We wrote zero tests."*
2. **Act 1 — intentional change → heal.** Search button becomes "Find flights",
   input id changes. Sensor fires in ~2s → 4 workflows at risk → resolver
   re-binds → green. *"UI-layer change, clean logs → healed 3 specs, PR opened."*
3. **Act 2 — regression → refuse to heal.** ConfigMap breaks currency rounding.
   Correlator names the root cause and the ConfigMap that caused it. **Rollout
   visibly pauses.** *The heal / refuse-to-heal contrast is the entire pitch —
   rehearse it to 90 seconds.*
4. **Act 3 — security + networking.** Delete a NetworkPolicy and widen a
   RoleBinding → prove `web-ui → postgres` reachability, emit a tightened Role
   from audit evidence, inject payment latency, file the resilience defect.
5. **Close.** Coverage, heals vs. bugs, maintenance hours saved, verdict.

## Build order

Strictly sequential. Steps 1–6 are the prize-winning core; 7–10 are upside.

1. kind + Calico + audit logging + Go SUT + repo skeleton
2. Cluster Sensor → `ChangeEvent` stream
3. World-model graph + impact traversal
4. Intent Spec + Resolver + Playwright runner ← *first demoable value*
5. Evidence Correlator ← *the winning component; do not let this slip*
6. Adjudicator ← *completes Acts 1+2*
7. Trivy posture + audit-log RBAC analysis
8. Network prober + toxiproxy resilience
9. CRDs + operator + release gate
10. Dashboard + axe + k6

## Non-negotiables

- **LLM record/replay cache from day one.** Live LLM latency will ruin the demo.
- **Each plane runs standalone.** The Go sensor prints events with no Python
  alive; the Python brain consumes a recorded fixture with no cluster alive.
  Solo builder: never be blocked on the other half.
- **Scope gravity is toward infrastructure.** If at day 5 the correlator is not
  working, cut Act 3 rather than cutting the correlator.

## Gotchas paid for in advance

- **kind's default CNI (kindnet) does not enforce NetworkPolicy.** Install
  Calico or Cilium on day one or every policy test silently "passes".
- **API-server audit logging on kind** needs a flag + policy file via
  `kubeadmConfigPatches` + a hostPath mount. Fiddly. Timebox it to 45 minutes;
  fall back to synthesising equivalent events from the watch stream — the
  analysis layer is identical.
- **Istio is a trap** at this scale. Use toxiproxy for fault injection.
