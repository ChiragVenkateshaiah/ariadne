"""The Intent Spec: the artifact tests are actually stored as.

This is the structural reason self-healing works at all. A traditional test
stores a locator ("#search-btn") and treats it as the source of truth; when
the id changes, the test is broken and someone edits a script. An Intent
Spec stores the INTENT ("submit the search") as the source of truth, and
treats any particular locator as a cached, disposable, re-derivable binding
to that intent (see resolver.py and the intent_bindings table in
schema.sql). Selector drift then costs one cheap re-resolve, not a rewrite.

This module intentionally has zero dependency on Playwright, gRPC, or the
database -- it is a plain data format. Something should be able to read/
write these as JSON without importing half the codebase.
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from typing import Any

from ariadne.graph.model import StepAction


@dataclass(slots=True)
class IntentStep:
    ordinal: int
    intent: str                       # "enter the origin airport" -- permanent, never rewritten by a heal
    action: StepAction
    target_hint: str | None = None    # natural-language description the resolver binds from
    value_expr: str | None = None     # literal, or "{{var}}" template
    assertion: str | None = None      # for action=ASSERT
    optional: bool = False

    def to_dict(self) -> dict[str, Any]:
        d = asdict(self)
        d["action"] = self.action.value
        return d

    @staticmethod
    def from_dict(d: dict[str, Any]) -> "IntentStep":
        return IntentStep(
            ordinal=d["ordinal"], intent=d["intent"], action=StepAction(d["action"]),
            target_hint=d.get("target_hint"), value_expr=d.get("value_expr"),
            assertion=d.get("assertion"), optional=bool(d.get("optional", False)),
        )


@dataclass(slots=True)
class IntentSpec:
    """One executable test: a workflow's steps plus the runtime parameters
    that instantiate its {{templates}}. `spec_id` matches test_specs.id;
    `workflow_id` matches workflows.node_id -- this is the thing
    ValidatorTask.spec_json carries opaquely across the Go orchestrator (see
    proto/ariadne/v1/validation.proto's comment: Go never parses this)."""

    spec_id: str
    workflow_id: str
    workflow_slug: str
    base_url: str
    steps: list[IntentStep] = field(default_factory=list)
    params: dict[str, str] = field(default_factory=dict)

    def render(self, key: str) -> str:
        """Resolves a {{var}} value_expr against `params`. Raises on a
        missing key rather than silently substituting an empty string --
        booking a flight with a blank origin is a different bug than the one
        the workflow author meant to write."""
        value = key
        if value.startswith("{{") and value.endswith("}}"):
            name = value[2:-2].strip()
            if name not in self.params:
                raise KeyError(f"IntentSpec {self.spec_id}: no param bound for {{{{{name}}}}}")
            return self.params[name]
        return value

    def to_json(self) -> str:
        return json.dumps({
            "spec_id": self.spec_id, "workflow_id": self.workflow_id, "workflow_slug": self.workflow_slug,
            "base_url": self.base_url, "params": self.params,
            "steps": [s.to_dict() for s in self.steps],
        }, indent=2)

    @staticmethod
    def from_json(text: str) -> "IntentSpec":
        d = json.loads(text)
        return IntentSpec(
            spec_id=d["spec_id"], workflow_id=d["workflow_id"], workflow_slug=d["workflow_slug"],
            base_url=d["base_url"], params=d.get("params", {}),
            steps=[IntentStep.from_dict(s) for s in d["steps"]],
        )


def build_from_workflow(conn, workflow_node_id: str, spec_id: str, base_url: str,
                          params: dict[str, str]) -> IntentSpec:
    """Reads a synthesized Workflow's steps back out of the graph (written by
    synth.py) and turns them into an executable IntentSpec. This is the
    bridge from "the LLM proposed a workflow" to "there is a runnable test"."""
    row = conn.execute("SELECT slug FROM workflows WHERE node_id = ?", (workflow_node_id,)).fetchone()
    if row is None:
        raise ValueError(f"no workflow found for node_id={workflow_node_id}")

    steps = []
    for r in conn.execute(
        "SELECT ordinal, intent, action, target_hint, value_expr, assertion, optional "
        "FROM workflow_steps WHERE workflow_id = ? ORDER BY ordinal", (workflow_node_id,)
    ):
        steps.append(IntentStep(
            ordinal=r["ordinal"], intent=r["intent"], action=StepAction(r["action"]),
            target_hint=r["target_hint"], value_expr=r["value_expr"], assertion=r["assertion"],
            optional=bool(r["optional"]),
        ))

    return IntentSpec(spec_id=spec_id, workflow_id=workflow_node_id, workflow_slug=row["slug"],
                       base_url=base_url, steps=steps, params=params)
