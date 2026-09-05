"""Executes an IntentSpec against a live page using the Resolver.

Assertion execution here is deliberately pragmatic rather than general: a
truly general "check this natural-language assertion against the page"
needs an LLM call per assertion (grading against a DOM/accessibility
snapshot), which is real future work. For now, ASSERT steps are matched
against a small set of known assertion shapes for this demo's two
workflows via keyword sniffing -- correct for what synth.py's fixture
actually produces today, and clearly marked NOT_EVALUATED rather than
silently passing when an assertion doesn't match a known shape (a false
PASS here would be far worse than an honest "couldn't check this").
"""

from __future__ import annotations

import sqlite3
from dataclasses import dataclass, field
from datetime import datetime, timezone

from playwright.sync_api import Page

from ariadne.graph.model import StepAction
from ariadne.llm.client import LLMClient
from ariadne.resolve import resolver
from ariadne.resolve.intent_spec import IntentSpec, IntentStep
from ariadne.resolve.resolver import Resolution, UnresolvableIntent


@dataclass(slots=True)
class StepResult:
    ordinal: int
    intent: str
    action: str
    status: str  # PASSED | FAILED | NOT_EVALUATED
    detail: str = ""
    healed: bool = False
    resolved_by: str = ""


@dataclass(slots=True)
class RunResult:
    spec_id: str
    workflow_slug: str
    status: str  # PASSED | FAILED
    steps: list[StepResult] = field(default_factory=list)
    started_at: str = ""
    finished_at: str = ""

    @property
    def heals(self) -> list[StepResult]:
        return [s for s in self.steps if s.healed]


def run_intent_spec(spec: IntentSpec, page: Page, conn: sqlite3.Connection, llm: LLMClient) -> RunResult:
    result = RunResult(spec_id=spec.spec_id, workflow_slug=spec.workflow_slug, status="PASSED",
                        started_at=datetime.now(timezone.utc).isoformat())

    for step in spec.steps:
        try:
            step_result = _execute_step(spec, step, page, conn, llm)
        except UnresolvableIntent as e:
            step_result = StepResult(ordinal=step.ordinal, intent=step.intent, action=step.action.value,
                                      status="FAILED", detail=str(e))
        except Exception as e:  # noqa: BLE001 -- a step failing for any reason must not crash the run
            step_result = StepResult(ordinal=step.ordinal, intent=step.intent, action=step.action.value,
                                      status="FAILED", detail=f"{type(e).__name__}: {e}")

        result.steps.append(step_result)
        if step_result.status == "FAILED" and not step.optional:
            result.status = "FAILED"
            break

    result.finished_at = datetime.now(timezone.utc).isoformat()
    return result


def _execute_step(spec: IntentSpec, step: IntentStep, page: Page, conn: sqlite3.Connection,
                   llm: LLMClient) -> StepResult:
    if step.action == StepAction.NAVIGATE:
        page.goto(spec.base_url + spec.render(step.value_expr))
        return StepResult(ordinal=step.ordinal, intent=step.intent, action=step.action.value, status="PASSED")

    if step.action == StepAction.ASSERT:
        return _execute_assert(spec, step, page)

    # FILL / CLICK / SELECT all resolve a locator first.
    res: Resolution = resolver.resolve(page, conn, spec.spec_id, step.ordinal, step.intent,
                                        step.action, step.target_hint, llm)

    if step.action == StepAction.FILL:
        res.locator.fill(spec.render(step.value_expr))
    elif step.action == StepAction.SELECT:
        res.locator.select_option(spec.render(step.value_expr))
    elif step.action == StepAction.CLICK:
        with page.expect_navigation(wait_until="networkidle"):
            res.locator.click()

    return StepResult(ordinal=step.ordinal, intent=step.intent, action=step.action.value, status="PASSED",
                       healed=res.healed, resolved_by=res.resolved_by)


def _execute_assert(spec: IntentSpec, step: IntentStep, page: Page) -> StepResult:
    assertion = (step.assertion or "").lower()
    base = {"ordinal": step.ordinal, "intent": step.intent, "action": step.action.value}

    if "offer" in assertion and "price" in assertion:
        count = page.locator('[data-testid="offer-price"]').count()
        status = "PASSED" if count >= 1 else "FAILED"
        return StepResult(**base, status=status, detail=f"found {count} priced offer(s)")

    if "confirmed" in assertion and "reference" in assertion:
        status_el = page.locator('[data-testid="booking-status"]')
        text = status_el.inner_text() if status_el.count() == 1 else ""
        status = "PASSED" if "confirmed" in text.lower() and len(text.strip()) > len("confirmed") else "FAILED"
        return StepResult(**base, status=status, detail=text)

    return StepResult(**base, status="NOT_EVALUATED",
                       detail=f"no known check for assertion shape: {step.assertion!r}")
