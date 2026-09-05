-- Ariadne world model.
--
-- One graph, auto-built, never hand-written. It is the thing that turns
-- "risk-based test selection" from an LLM guess into a graph traversal, and it
-- is what lets the evidence collector know exactly which pods to read logs from
-- when a business workflow fails.
--
-- Shape:
--   Workflow --HAS_STEP--> Step --EXERCISES--> Endpoint --SERVED_BY--> Service
--                            |                                           |
--                            +--RENDERS_ON--> UIRoute        Service --BACKED_BY--> Workload
--                                                                        |
--                                                            Workload --CALLS--> Service
--
-- Everything is nodes + edges so that traversal code stays uniform. Tables that
-- hang off the graph (bindings, runs, findings) are separate because they are
-- time-series, not topology.

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- ---------------------------------------------------------------------------
-- Topology
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS nodes (
    id            TEXT PRIMARY KEY,      -- "service:travel/pricing-svc" (stable, human-readable)
    kind          TEXT NOT NULL,         -- NodeKind
    name          TEXT NOT NULL,
    namespace     TEXT,                  -- NULL for non-k8s nodes (workflows, routes)
    k8s_uid       TEXT,                  -- survives renames; NULL for logical nodes
    display_name  TEXT,                  -- business-facing label

    -- How we learned this node exists. Provenance matters: a node inferred by an
    -- LLM must never be trusted the same way as one read from the K8s API.
    discovery     TEXT NOT NULL,         -- Discovery: K8S_API | OPENAPI | UI_CRAWL | LLM_INFERRED | MANUAL
    confidence    REAL NOT NULL DEFAULT 1.0,

    attrs         TEXT NOT NULL DEFAULT '{}',   -- JSON, kind-specific
    first_seen    TEXT NOT NULL,
    last_seen     TEXT NOT NULL,
    active        INTEGER NOT NULL DEFAULT 1    -- soft delete; history is evidence
);

CREATE INDEX IF NOT EXISTS idx_nodes_kind      ON nodes(kind, active);
CREATE INDEX IF NOT EXISTS idx_nodes_ns_name   ON nodes(namespace, name);
CREATE INDEX IF NOT EXISTS idx_nodes_uid       ON nodes(k8s_uid);

CREATE TABLE IF NOT EXISTS edges (
    id            TEXT PRIMARY KEY,      -- "src|kind|dst"
    src_id        TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    dst_id        TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,         -- EdgeKind

    discovery     TEXT NOT NULL,
    confidence    REAL NOT NULL DEFAULT 1.0,
    weight        REAL NOT NULL DEFAULT 1.0,   -- observed call volume, when known
    ordinal       INTEGER,                     -- step order within a workflow

    attrs         TEXT NOT NULL DEFAULT '{}',
    first_seen    TEXT NOT NULL,
    last_seen     TEXT NOT NULL,
    active        INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_edges_src  ON edges(src_id, kind, active);
CREATE INDEX IF NOT EXISTS idx_edges_dst  ON edges(dst_id, kind, active);
CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind, active);

-- ---------------------------------------------------------------------------
-- Business layer
-- ---------------------------------------------------------------------------

-- Workflows are also nodes (so traversal is uniform), but they carry enough
-- business metadata to deserve their own table. criticality is the single most
-- important input to risk ranking -- it is what makes "run the revenue path
-- first" possible.
CREATE TABLE IF NOT EXISTS workflows (
    node_id        TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    slug           TEXT NOT NULL UNIQUE,        -- "book_one_way_flight"
    title          TEXT NOT NULL,
    description    TEXT,
    business_goal  TEXT,                        -- "customer completes a purchase"
    criticality    REAL NOT NULL DEFAULT 0.5,   -- 0..1, LLM-proposed, human-overridable
    revenue_path   INTEGER NOT NULL DEFAULT 0,
    pii_involved   INTEGER NOT NULL DEFAULT 0,
    entry_route    TEXT,
    persona        TEXT,                        -- "anonymous shopper", "agent"
    derived_from   TEXT,                        -- how the LLM inferred it
    reviewed_by_human INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS workflow_steps (
    node_id      TEXT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    workflow_id  TEXT NOT NULL REFERENCES workflows(node_id) ON DELETE CASCADE,
    ordinal      INTEGER NOT NULL,
    intent       TEXT NOT NULL,     -- "enter the origin airport"  <- semantic, not a selector
    action       TEXT NOT NULL,     -- fill | click | select | navigate | assert | wait
    target_hint  TEXT,              -- natural-language description of the target
    value_expr   TEXT,              -- literal or template, e.g. "{{origin}}"
    assertion    TEXT,              -- for action=assert
    optional     INTEGER NOT NULL DEFAULT 0,
    UNIQUE(workflow_id, ordinal)
);

CREATE INDEX IF NOT EXISTS idx_steps_workflow ON workflow_steps(workflow_id, ordinal);

-- ---------------------------------------------------------------------------
-- Tests
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS test_specs (
    id            TEXT PRIMARY KEY,
    workflow_id   TEXT NOT NULL REFERENCES workflows(node_id) ON DELETE CASCADE,
    kind          TEXT NOT NULL,        -- ValidatorKind
    version       INTEGER NOT NULL DEFAULT 1,
    spec_json     TEXT NOT NULL,        -- the Intent Spec itself
    generated_by  TEXT NOT NULL,        -- model id or "human"
    generated_at  TEXT NOT NULL,
    stale         INTEGER NOT NULL DEFAULT 0,   -- a change invalidated it
    stale_reason  TEXT,
    active        INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_specs_workflow ON test_specs(workflow_id, kind, active);

-- The binding cache is the mechanism behind self-healing. Intent is permanent;
-- the binding from intent to a concrete locator is disposable and re-derivable.
-- Selector drift therefore costs one cheap resolve, not a test rewrite -- this
-- is the structural reason maintenance drops rather than a claim we assert.
CREATE TABLE IF NOT EXISTS intent_bindings (
    id             TEXT PRIMARY KEY,
    spec_id        TEXT NOT NULL REFERENCES test_specs(id) ON DELETE CASCADE,
    step_ordinal   INTEGER NOT NULL,
    intent         TEXT NOT NULL,
    ui_route       TEXT,

    strategy       TEXT NOT NULL,   -- role_name | test_id | label | text | css | xpath
    locator        TEXT NOT NULL,
    strategy_rank  INTEGER NOT NULL DEFAULT 0,  -- prefer semantic over positional

    resolved_at    TEXT NOT NULL,
    resolved_by    TEXT NOT NULL,   -- "cache" | "heuristic" | model id
    hit_count      INTEGER NOT NULL DEFAULT 0,
    miss_count     INTEGER NOT NULL DEFAULT 0,
    last_success   TEXT,
    healed_from    TEXT,            -- previous locator this replaced
    active         INTEGER NOT NULL DEFAULT 1,
    UNIQUE(spec_id, step_ordinal, active)
);

-- ---------------------------------------------------------------------------
-- Time series: changes, runs, verdicts
-- ---------------------------------------------------------------------------

-- Local mirror of ChangeEvents. Kept so the adjudicator can ask "what changed
-- just before this failed?" without a round trip, and so provenance survives a
-- sensor restart.
CREATE TABLE IF NOT EXISTS change_events (
    id             TEXT PRIMARY KEY,
    observed_at    TEXT NOT NULL,
    source         TEXT NOT NULL,
    change_class   TEXT NOT NULL,
    operation      TEXT NOT NULL,
    object_node_id TEXT REFERENCES nodes(id),
    object_kind    TEXT,
    object_ns      TEXT,
    object_name    TEXT,
    hints_json     TEXT NOT NULL DEFAULT '{}',
    diffs_json     TEXT NOT NULL DEFAULT '[]',
    provenance_json TEXT NOT NULL DEFAULT '{}',
    processed      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_changes_time  ON change_events(observed_at DESC);
CREATE INDEX IF NOT EXISTS idx_changes_node  ON change_events(object_node_id, observed_at DESC);

-- One impact analysis: a change, the workflows it put at risk, and why.
CREATE TABLE IF NOT EXISTS impact_analyses (
    id              TEXT PRIMARY KEY,
    change_event_id TEXT NOT NULL REFERENCES change_events(id) ON DELETE CASCADE,
    created_at      TEXT NOT NULL,
    blast_radius_json TEXT NOT NULL,   -- node ids by hop distance
    rationale       TEXT,              -- LLM explanation, business language
    model_id        TEXT
);

CREATE TABLE IF NOT EXISTS workflow_risk (
    impact_id     TEXT NOT NULL REFERENCES impact_analyses(id) ON DELETE CASCADE,
    workflow_id   TEXT NOT NULL REFERENCES workflows(node_id) ON DELETE CASCADE,
    risk_score    REAL NOT NULL,       -- 0..1, drives execution order
    hop_distance  INTEGER NOT NULL,
    criticality   REAL NOT NULL,
    coverage_gap  REAL NOT NULL,       -- 1.0 = no test exists at all
    flakiness     REAL NOT NULL DEFAULT 0.0,
    reason        TEXT,
    selected      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (impact_id, workflow_id)
);

CREATE TABLE IF NOT EXISTS runs (
    id                TEXT PRIMARY KEY,
    started_at        TEXT NOT NULL,
    finished_at       TEXT,
    trigger           TEXT NOT NULL,    -- change_event | schedule | manual | gate
    change_event_ids  TEXT NOT NULL DEFAULT '[]',
    verdict           TEXT,
    confidence        REAL,
    summary           TEXT,
    metrics_json      TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS task_results (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    spec_id        TEXT REFERENCES test_specs(id),
    workflow_id    TEXT REFERENCES workflows(node_id),
    kind           TEXT NOT NULL,
    phase          TEXT NOT NULL,
    started_at     TEXT,
    finished_at    TEXT,
    exit_code      INTEGER,
    payload_json   TEXT NOT NULL DEFAULT '{}',
    involved_pods_json TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS idx_task_run ON task_results(run_id);
CREATE INDEX IF NOT EXISTS idx_task_wf  ON task_results(workflow_id, finished_at DESC);

CREATE TABLE IF NOT EXISTS findings (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    category       TEXT NOT NULL,
    severity       TEXT NOT NULL,
    title          TEXT NOT NULL,
    description    TEXT,
    root_cause     TEXT,
    reasoning      TEXT,
    confidence     REAL NOT NULL DEFAULT 0.0,
    evidence_json  TEXT NOT NULL DEFAULT '[]',
    workflows_json TEXT NOT NULL DEFAULT '[]',
    remediation_json TEXT NOT NULL DEFAULT '{}',
    owasp_refs     TEXT NOT NULL DEFAULT '[]',
    first_seen_change_id TEXT REFERENCES change_events(id),
    created_at     TEXT NOT NULL
);

-- Every repair is auditable, and no repair happens without an adjudication.
-- A heal recorded against ADJUDICATION_APP_REGRESSION is a bug in Ariadne
-- itself; enforce that invariant in code.
CREATE TABLE IF NOT EXISTS heals (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    spec_id        TEXT NOT NULL REFERENCES test_specs(id),
    step_ordinal   INTEGER,
    adjudication   TEXT NOT NULL,
    intent         TEXT,
    old_binding    TEXT,
    new_binding    TEXT,
    strategy       TEXT,
    reasoning      TEXT NOT NULL,
    confidence     REAL NOT NULL,
    applied        INTEGER NOT NULL DEFAULT 0,
    requires_review INTEGER NOT NULL DEFAULT 0,
    supporting_change_ids TEXT NOT NULL DEFAULT '[]',
    healed_at      TEXT NOT NULL
);

-- Flakiness input for risk scoring. Cheap to maintain, high value: a workflow
-- that fails intermittently should not keep triggering regression alarms.
CREATE TABLE IF NOT EXISTS test_history (
    spec_id     TEXT NOT NULL REFERENCES test_specs(id) ON DELETE CASCADE,
    run_id      TEXT NOT NULL,
    passed      INTEGER NOT NULL,
    healed      INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER,
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (spec_id, run_id)
);

-- ---------------------------------------------------------------------------
-- Views
-- ---------------------------------------------------------------------------

-- Which services does each workflow actually depend on? This is the join that
-- answers "pricing-svc changed -- what do I run?" and also "this workflow
-- failed -- whose logs do I read?".
CREATE VIEW IF NOT EXISTS v_workflow_services AS
SELECT DISTINCT
    w.node_id   AS workflow_id,
    w.slug      AS workflow_slug,
    w.criticality,
    svc.id      AS service_id,
    svc.namespace,
    svc.name    AS service_name
FROM workflows w
JOIN edges e_step  ON e_step.src_id = w.node_id AND e_step.kind = 'HAS_STEP'   AND e_step.active = 1
JOIN edges e_ex    ON e_ex.src_id   = e_step.dst_id AND e_ex.kind = 'EXERCISES' AND e_ex.active = 1
JOIN edges e_srv   ON e_srv.src_id  = e_ex.dst_id   AND e_srv.kind = 'SERVED_BY' AND e_srv.active = 1
JOIN nodes svc     ON svc.id = e_srv.dst_id AND svc.active = 1;

-- Workflows with no active test spec: the coverage gaps that should be
-- generated first when a new surface appears.
CREATE VIEW IF NOT EXISTS v_coverage_gaps AS
SELECT w.node_id AS workflow_id, w.slug, w.title, w.criticality
FROM workflows w
LEFT JOIN test_specs t ON t.workflow_id = w.node_id AND t.active = 1
WHERE t.id IS NULL;
