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

---

# Phase 2: Platform Engineering Depth (post-demo-core)

Everything above is Phase 1 — the demo core (items 1–6 of the build order,
plus the dashboard) — and it is done and live-verified: Sensor, World Model,
Resolver/heal, Evidence Correlator, Adjudicator, dashboard. Phase 2 is a
deliberate reprioritization once that core is solid, not a replacement for it.

**The reprioritization, stated plainly:** UI self-healing is the pitch's hook,
but it's also the thing every competing team will show — Playwright-plus-LLM
is common. What isn't common is treating Kubernetes networking and API
contracts as first-class, deeply-tested surfaces with the same rigor as the
UI, run the way an actual platform engineering team would run them. Phase 2
leans into that: **API testing and networking get the depth investment, not
more UI polish.** The differentiator moves from "the demo trick" to "this
could genuinely be an internal platform other teams onboard to."

## 1. API testing, as a first-class validator (not an afterthought)

UI has a full Intent Spec → Resolver → Runner pipeline. API has only
`API_ENDPOINT` graph nodes from `catalog.py` — no spec format, no runner.
Fix that symmetrically:

- **API Intent Spec**: method, path, request body/headers, and assertions on
  status code, response schema (shape, not just presence), and specific field
  values — the same "intent over implementation" philosophy as UI steps, so a
  response field reordering or an added optional field doesn't false-positive
  a break the way a brittle exact-match would.
- **API runner** (Python, `httpx`), dispatched as a Kubernetes Job through the
  same `OrchestratorService` the network prober already uses — no new
  orchestration mechanism needed, just a new runner image and
  `VALIDATOR_KIND_API` spec shape.
- **Contract drift detection**: snapshot each endpoint's observed
  response shape per deployment (from real traffic, via the same structured
  logs LogCollector already reads, or from live probe responses) and diff it
  across a `CHANGE_CLASS_WORKLOAD_IMAGE` event — "this endpoint's response
  shape changed the same day the image changed" is exactly the kind of
  correlated finding the Adjudicator is built to reason about.
- **Depth items**: boundary/fuzz testing on request inputs, idempotency
  checks on retried writes, per-endpoint latency SLO assertions (feeds
  directly into the Prometheus histograms in §3).

## 2. Networking — treated as a platform service, not a demo script

The reframe: don't build "one script that proves segmentation once." Build
the mechanism a platform team would actually run continuously.

- **NetworkPolicy as a reconciliation loop, not a one-shot generator.**
  `security/network_policy.py` already derives policy from `CALLS` edges
  correctly (including the NodePort external-ingress case). Extend it into a
  continuous loop: the Sensor already emits `CHANGE_CLASS_WORKLOAD_SPEC`
  when a new service-to-service call pattern would need a policy change: dry-run,
  and route consequential Applys through the Adjudicator (the same
  heal-vs-regression judgment call — a policy widening is a Finding
  candidate, not an auto-apply) rather than a script run.
- **Calico-native depth beyond vanilla `networking.k8s.io/v1`**: since Calico
  is already the CNI, use its own CRDs — `GlobalNetworkPolicy` and
  `NetworkSet` for L3–L7 and DNS-aware egress rules, and verify WireGuard
  pod-to-pod encryption is actually active (a real zero-trust checkbox, not
  just claimed).
- **Ingress/Gateway API conformance**: routing rules, TLS termination, rate
  limiting at the edge — tested the same way NetworkPolicy is: derive the
  expectation from the graph, probe to confirm reality matches it.
- **Chaos beyond toxiproxy latency injection**: Chaos Mesh or Litmus for
  pod-kill, network-partition, and node-drain scenarios, with Ariadne's own
  workflow-level assertions proving (or disproving) business continuity
  through the failure — the resilience story generalized from "one injected
  latency" to "arbitrary infrastructure failure modes."
  Reconsider a service mesh (Istio/Linkerd) here, now that "demo-day risk"
  isn't the constraint — Phase 1 correctly rejected it as a time sink, but
  Phase 2's whole point is Kubernetes-native depth, and mTLS enforcement +
  L7 traffic policy are real platform capabilities worth testing if time
  allows.
- **Self-service onboarding — the actual "PaaS" framing.** Generalize the
  `ariadne.dev/watched=true` namespace label into a lightweight CRD (e.g.
  `AriadneTarget`) a team applies to their own namespace to register it.
  Applying the CR is the entire onboarding flow: World Model discovery,
  workflow synthesis, and NetworkPolicy generation all key off it already —
  this just turns "I configured it for them" into "they asked the platform
  for it," which is the actual distinction between a demo and a platform.
- **Full OWASP Kubernetes Top 10, not just K03/K07.** Current strength is
  K03 (least privilege, via audit-log `SubjectActivity`) and K07 (network
  segmentation). Extend: K01/K09 (workload/cluster misconfig — Trivy or
  kube-bench, LLM-ranked rather than raw findings dumped), K02 (supply
  chain — image signing/provenance), K05 (audit coverage gaps — already
  self-referentially checked), K06 (broken authn/authz beyond RBAC), K08
  (secrets hygiene — plaintext env vars, unsealed secrets), K10 (known-CVE
  images via Trivy).

## 3. Prometheus + Grafana for QE visualization

The custom FastAPI/HTMX dashboard stays — it tells the *story* (the
heal/block ledger with reasoning, the topology graph) in a way a generic
Grafana panel can't. Prometheus/Grafana adds what that dashboard
deliberately doesn't attempt: time-series depth, trends, and alerting — the
signal that this is an observability-integrated platform, not only a demo
app.

- **Deploy** `kube-prometheus-stack` via Helm (Prometheus Operator + Grafana
  + Alertmanager + kube-state-metrics) into a `monitoring` namespace.
- **Instrument every control-plane Go service** (sensor, logcollector,
  orchestrator) with `/metrics` via `prometheus/client_golang`: change
  events processed by class, impact analyses computed, Job dispatch
  latency/success rate, evidence-collection query volume.
- **Instrument the Python brain** via `prometheus_client`: workflows
  synthesized, resolver outcomes by tier (cache/heuristic/LLM-fallback —
  a rising LLM-fallback rate is itself a UI-drift signal worth graphing),
  adjudication outcomes by verdict, LLM call latency.
- **Instrument the SUT services** (already structured-logging) with a
  lightweight `/metrics` endpoint: request rate, error rate, latency
  histograms — real data for both the API-testing SLO assertions above and
  the resilience dashboards below.
- **`ServiceMonitor`/`PodMonitor` CRs** for auto-discovery of all of the above.
- **QE-specific Grafana dashboards** (provisioned as code, committed to the
  repo — not hand-built in the UI):
  - *Ariadne Overview*: workflow coverage %, heal rate, block rate, mean
    time-to-adjudicate.
  - *Self-Healing Health*: resolver strategy-tier distribution over time.
  - *Network & Security Posture*: NetworkPolicy conformance pass rate,
    least-privilege score per ServiceAccount, blocked cross-namespace
    attempts.
  - *Resilience*: p95/p99 latency under fault injection, error-budget burn
    during the toxiproxy/chaos scenarios.
  - *Release Gate*: `QualityAssessment` verdict history correlated against
    real deploys (`CHANGE_CLASS_WORKLOAD_IMAGE` events).

## Phase 2 build order

Sequenced so each item stays demoable on its own if time runs out again:

1. API Intent Spec + runner (symmetric with UI; reuses the Orchestrator)
2. Prometheus + Grafana deployed, control-plane services instrumented
   (observability should exist *before* the deeper networking work, so its
   effects are visible while being built)
3. NetworkPolicy reconciliation loop + Calico-native policy depth
4. Full OWASP K8s coverage (K01/K02/K05/K06/K08/K09/K10)
5. Chaos Mesh/Litmus scenarios beyond toxiproxy
6. `AriadneTarget` CRD — the actual self-service onboarding flow
7. Service mesh evaluation (stretch, only if the above lands early)
