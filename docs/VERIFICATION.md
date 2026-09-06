# Verification Guide — everything built so far

A hands-on walkthrough to manually confirm every claim in
[docs/ARCHITECTURE.md](ARCHITECTURE.md) actually holds, in the order things
were built. Each section says what you're checking and why it matters, gives
exact commands, and says what a correct result looks like. Run these against
the live cluster (already up if you're continuing a session; `## 0` covers
starting fresh).

Two terminals help: one for `kubectl`/scripts, one dedicated to whichever
`port-forward` a section needs (port-forwards are foreground processes —
`Ctrl+C` to stop one before starting the next, or just open a new terminal
tab per forward).

---

## 0. If starting fresh: bring the stack up

Skip this section if pods are already running (check with
`kubectl --context kind-ariadne get pods -A`).

```bash
export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin"
cd ~/projects/amadeus_hackathon

./scripts/create-cluster.sh          # kind cluster + audit logging
./scripts/install-calico.sh          # CNI; ends with a live NetworkPolicy smoke test
./scripts/build-and-load-sut.sh      # 5 SUT images
./scripts/deploy-sut.sh              # deploys them + Postgres
./scripts/build-and-load-control-plane.sh   # sensor, logcollector, orchestrator, netprobe
./scripts/deploy-control-plane.sh
./scripts/install-monitoring.sh      # Prometheus + Grafana (takes a few minutes first time)
```

Each script prints its own success/failure; `install-calico.sh` specifically
ends with `PASS: blocked as expected -- Calico is enforcing NetworkPolicy.`
— if you don't see that line, stop here, something's wrong before anything
else is worth checking.

---

## 1. The System Under Test — a real flight-booking app

**What this proves:** there's a real, working application for Ariadne to
watch — not a toy that only exists to be broken on cue.

Open **http://localhost:8080** in a browser. Search any route (e.g.
`LHR` → `JFK`, any date), pick an offer, fill in a fake name/card, confirm
the booking. You should land on a confirmation page with a reference ID.

Verify it actually persisted:

```bash
kubectl --context kind-ariadne exec -n travel deploy/postgres -- \
  psql -U ariadne -d ariadne -c "SELECT id, flight_id, passenger_name, amount, status FROM bookings ORDER BY created_at DESC LIMIT 3;"
```

You should see your booking, `status = CONFIRMED`.

---

## 2. The Cluster Sensor — change detection

**What this proves:** Ariadne learns about changes from the cluster itself,
in real time — not from polling or a CI hook.

```bash
kubectl --context kind-ariadne logs -f deployment/sensor -n ariadne-system
```

Leave that running. In a second terminal, make a real change:

```bash
kubectl --context kind-ariadne scale deployment/search-api -n travel --replicas=2
```

Within a second or two you should see a new JSON log line in the first
terminal with `"class":"CHANGE_CLASS_SCALING"` naming `search-api`. Revert:

```bash
kubectl --context kind-ariadne scale deployment/search-api -n travel --replicas=1
```

`Ctrl+C` the log-follow when done.

---

## 3. The World Model — topology + business workflows

**What this proves:** the graph builds itself from real cluster + code
topology; nothing is hand-typed.

```bash
cd brain && source .venv/bin/activate
export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin"

kubectl --context kind-ariadne port-forward -n ariadne-system svc/sensor 9090:9090 &
sleep 2
timeout 8 python scripts/run_ingester.py localhost:9090 /tmp/verify.db   # ingests real topology
python3 -c "
from ariadne.graph import store, synth
from ariadne.llm.fixtures import default_mock_client
conn = store.connect('/tmp/verify.db')
with store.transaction(conn):
    slugs = synth.synthesize_workflows(conn, default_mock_client())
print('workflows discovered:', slugs)
print()
for row in conn.execute('SELECT service_name, workflow_slug FROM v_workflow_services ORDER BY workflow_slug'):
    print(' ', row['workflow_slug'], '->', row['service_name'])
"
kill %1  # stop the port-forward
```

Expect `workflows discovered: ['book_one_way_flight', 'search_flights']` and
a service list under each — this is the graph, built from real Service/
Workload objects plus the declared `CALLS` edges, not typed by hand for this
demo.

---

## 4. Self-healing — the actual pitch, live

**What this proves:** a test survives a routine UI change, structurally —
not by an LLM being clever, but because intent and locator are decoupled.

```bash
# from brain/, venv active, as above
python3 - <<'PY'
from playwright.sync_api import sync_playwright
from ariadne.graph import store, synth, model as gmodel
from ariadne.resolve.intent_spec import build_from_workflow
from ariadne.resolve.runner import run_intent_spec
from ariadne.llm.fixtures import default_mock_client

conn = store.connect('/tmp/act1.db')
llm = default_mock_client()
with store.transaction(conn):
    synth.synthesize_workflows(conn, llm)
wf_id = gmodel.node_id(gmodel.NodeKind.WORKFLOW, None, 'book_one_way_flight')
with store.transaction(conn):
    conn.execute("INSERT INTO test_specs (id, workflow_id, kind, spec_json, generated_by, generated_at) VALUES ('s1',?,'UI','{}','verify','now')", (wf_id,))
spec = build_from_workflow(conn, wf_id, 's1', 'http://localhost:8080',
    {'origin':'LHR','destination':'JFK','date':'2026-10-01','passenger_name':'Verify Test','card_last4':'4242'})

with sync_playwright() as p:
    browser = p.chromium.launch(); page = browser.new_page()
    with store.transaction(conn):
        result = run_intent_spec(spec, page, conn, llm)
    browser.close()
print('RUN 1 (v1):', result.status)
PY
```

Now break the UI (a routine change, not a special demo mode):

```bash
kubectl --context kind-ariadne set env deployment/web-ui -n travel UI_VARIANT=v2
kubectl --context kind-ariadne rollout status deployment/web-ui -n travel
curl -s localhost:8080/ | grep -oE 'find-flights-btn|Find flights'   # confirms v2 is live
```

Rerun the **identical** script against the **same** `/tmp/act1.db` — every
input `id` changed, and the button's `id` *and* visible text changed
("Search" → "Find flights"):

```bash
python3 - <<'PY'
from playwright.sync_api import sync_playwright
from ariadne.graph import store, model as gmodel
from ariadne.resolve.intent_spec import build_from_workflow
from ariadne.resolve.runner import run_intent_spec
from ariadne.llm.fixtures import default_mock_client

conn = store.connect('/tmp/act1.db')
llm = default_mock_client()
wf_id = gmodel.node_id(gmodel.NodeKind.WORKFLOW, None, 'book_one_way_flight')
spec = build_from_workflow(conn, wf_id, 's1', 'http://localhost:8080',
    {'origin':'LHR','destination':'JFK','date':'2026-10-01','passenger_name':'Verify Test','card_last4':'4242'})
with sync_playwright() as p:
    browser = p.chromium.launch(); page = browser.new_page()
    with store.transaction(conn):
        result = run_intent_spec(spec, page, conn, llm)
    browser.close()
print('RUN 2 (v2, SAME db, NO code changes):', result.status)
for s in result.steps:
    print(f'  step {s.ordinal}: {s.intent}  healed={s.healed}  resolved_by={s.resolved_by}')
PY

kubectl --context kind-ariadne set env deployment/web-ui -n travel UI_VARIANT-   # revert to baseline
kubectl --context kind-ariadne rollout status deployment/web-ui -n travel
```

Expect `RUN 2: PASSED` with several steps showing `healed=True` — inputs
healed via `resolved_by=heuristic` (label text didn't change), the submit
button via `resolved_by=mock` (the LLM-fallback tier — the only way to catch
"Search" → "Find flights", since there's zero shared text).

---

## 5. The Evidence Correlator — explainability

**What this proves:** when something fails, Ariadne can already see pod
logs, K8s events, and the audit log — the raw material a root-cause
explanation is built from.

```bash
kubectl --context kind-ariadne port-forward -n ariadne-system svc/logcollector 9091:9091 &
sleep 2
POD=$(kubectl --context kind-ariadne get pods -n travel -l app=pricing-svc -o jsonpath='{.items[0].metadata.name}')
UID=$(kubectl --context kind-ariadne get pod $POD -n travel -o jsonpath='{.metadata.uid}')

grpcurl -plaintext -import-path proto -proto ariadne/v1/evidence.proto \
  -d "{\"correlation_id\":\"verify-1\",\"pods\":[{\"api_version\":\"v1\",\"kind\":\"Pod\",\"namespace\":\"travel\",\"name\":\"$POD\",\"uid\":\"$UID\"}],\"include_pod_logs\":true,\"include_k8s_events\":true,\"max_lines_per_pod\":5}" \
  localhost:9091 ariadne.v1.LogCollectorService/CollectEvidence
kill %1
```

Expect a JSON `EvidenceBundle` with real, structured `podLogs` entries
(parsed `level`/`message`/`trace_id` fields, not raw text) and `zero`
`collectionErrors`.

**Audit log / least-privilege check** (the K03 story):

```bash
kubectl --context kind-ariadne port-forward -n ariadne-system svc/logcollector 9091:9091 &
sleep 2
grpcurl -plaintext -import-path proto -proto ariadne/v1/evidence.proto \
  -d '{"service_accounts_only": true, "limit": 5}' \
  localhost:9091 ariadne.v1.LogCollectorService/QueryAuditLog
kill %1
```

Expect real `subjectActivity` entries showing exactly which verbs/resources
each ServiceAccount actually exercised — this is real audit data, not a
mock.

---

## 6. The Adjudicator — heal vs. block, the actual thesis

**What this proves:** Ariadne tells apart a cosmetic change from a real
regression, and structurally *cannot* heal a real bug.

```bash
python3 - <<'PY'
from ariadne.adjudicate.adjudicator import adjudicate, write_heal, write_finding
from ariadne.llm.fixtures import default_mock_client
llm = default_mock_client()

# Act 1 shape: a UI change, clean evidence
r1 = adjudicate(llm, "book_one_way_flight", "search button locator drift",
    [{"change_class":"CHANGE_CLASS_WORKLOAD_SPEC","object_name":"web-ui","diffs":[{"path":"spec...","before":"...","after":"..."}]}])
print("UI change  ->", r1.adjudication, f"(confidence {r1.confidence})")

# Act 2 shape: a business-logic config change, clean evidence
r2 = adjudicate(llm, "book_one_way_flight", "displayed price is wrong",
    [{"change_class":"CHANGE_CLASS_CONFIG","object_name":"pricing-flags",
      "diffs":[{"path":"data.flags.json","before":'{"rounding_mode": "HALF_UP"}',"after":'{"rounding_mode": "FLOOR"}'}]}])
print("Config change ->", r2.adjudication)
print("  root cause:", r2.root_cause)

try:
    write_heal(None, "run","spec",1,"i","old","new","TEXT", r2, [])
    print("BUG: a heal was written for a regression!")
except ValueError as e:
    print("Correctly refused to heal:", e)
PY
```

Expect: `UI change -> TEST_DEFECT`, `Config change -> APP_REGRESSION` with a
root cause naming the exact field (`rounding_mode ... HALF_UP -> FLOOR`),
and the heal attempt raising `ValueError` — that refusal is enforced in
code, not by convention (see `brain/tests/test_adjudicator.py` for the
same scenarios as permanent, automated tests).

---

## 7. Network conformance — proof, not assertion

**What this proves:** NetworkPolicies are generated from real topology
(zero hand-written rules), and a real Kubernetes Job then *attempts the
connection* to prove they hold — for both the allowed and blocked cases.

```bash
# Generate + inspect (uses the graph built in step 3)
python3 scripts/generate_network_policies.py /tmp/verify.db travel
cat ../deploy/security/generated/allow-postgres-from-callers.yaml   # note: only booking-api is allowed in

kubectl --context kind-ariadne apply -f ../deploy/security/generated/
```

Confirm the app still works end-to-end (every real call path should still
be allowed):

```bash
curl -s localhost:8080/health
curl -s "localhost:8080/search?origin=LHR&destination=JFK&date=2026-10-01" | grep -o 'offer-price'
```

Now the actual proof — dispatch two real probes via the Orchestrator:

```bash
kubectl --context kind-ariadne port-forward -n ariadne-system svc/orchestrator 9092:9092 &
sleep 2
cat > /tmp/probe.json <<'EOF'
{"run_id":"verify-net","namespace":"travel","tasks":[
  {"task_id":"web-ui-to-postgres","kind":"VALIDATOR_KIND_NETWORK_CONFORMANCE","image":"ariadne/netprobe:dev",
   "env":{"ARIADNE_PROBE_TARGET":"postgres:5432"},"pod_labels":{"app":"web-ui"},"timeout_seconds":20},
  {"task_id":"web-ui-to-search-api","kind":"VALIDATOR_KIND_NETWORK_CONFORMANCE","image":"ariadne/netprobe:dev",
   "env":{"ARIADNE_PROBE_TARGET":"search-api:8081"},"pod_labels":{"app":"web-ui"},"timeout_seconds":20}
]}
EOF
grpcurl -plaintext -import-path proto -proto ariadne/v1/validation.proto \
  -d @ localhost:9092 ariadne.v1.OrchestratorService/RunValidators < /tmp/probe.json
kill %1
```

Expect `web-ui-to-search-api` → `"reachable":true` (a real `CALLS` edge
exists) and `web-ui-to-postgres` → `"reachable":false` with an `i/o
timeout` (no `CALLS` edge — web-ui never legitimately talks to Postgres,
only `booking-api` does — so the generated policy correctly blocks it).

---

## 8. The Dashboard — the demo's visual surface

```bash
./scripts/run_dashboard.sh /tmp/verify.db
```

Open **http://localhost:8090**. You should see: the topology graph (color-
coded by node kind), a live change feed (polling every 2s — try step 2's
scale command again in another terminal and watch it appear), and an empty
heal/block ledger (populate it by re-running step 6's script against
`/tmp/verify.db` instead of `None`, or just trust step 6's direct output).

---

## 9. Prometheus + Grafana — QE visualization

```bash
kubectl --context kind-ariadne port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80 &
```

Open **http://localhost:3000**, log in `admin` / `ariadne` (see
`deploy/monitoring/values.yaml`), open **Dashboards → Ariadne → Ariadne /
Control Plane**. You should see real, live-updating panels: change events
by class/operation, informer sync status, per-pod memory/CPU for the
control plane. Trigger step 2's scale command again and watch the "Change
events / sec" panel move within ~10s.

Prometheus itself, if you want to run raw queries:

```bash
kubectl --context kind-ariadne port-forward -n monitoring svc/kube-prometheus-stack-prometheus 9091:9090 &
```

Open **http://localhost:9091**, query `ariadne_sensor_change_events_total`.

---

## 10. Tests + CI

Locally:

```bash
cd ~/projects/amadeus_hackathon/control-plane
go build ./... && go vet ./... && go test ./... -v   # expect 29 passed, 3 packages

cd ../brain && source .venv/bin/activate
ruff check .                                          # expect "All checks passed!"
python3 -m pytest tests/ -v                            # expect 27 passed
```

On GitHub: open the **Actions** tab at
https://github.com/ChiragVenkateshaiah/ariadne/actions — the latest run on
`main` should be green across all jobs (`proto`, `go` ×2, `python`, and 9
`docker build` jobs).

---

## Cleanup between runs

```bash
rm -f /tmp/verify.db /tmp/act1.db /tmp/probe.json
pkill -f "port-forward" 2>/dev/null   # kill any forwards you forgot to Ctrl+C
```

The cluster itself is safe to leave running between sessions — nothing here
tears it down.
