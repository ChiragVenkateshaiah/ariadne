"""Hand-authored API/UI surface catalog for the demo SUT.

Ariadne's design principle is that only Kubernetes-observed facts get
Discovery.K8S_API and only LLM output gets Discovery.LLM_INFERRED -- this
catalog is neither. It exists because our SUT has no OpenAPI documents to
discover endpoints from (a real target would supply those, see
docs/ARCHITECTURE.md's OPENAPI ChangeSource), so for this demo the routes are
declared once, by hand, tagged Discovery.MANUAL: an honest record of what we
ourselves built sut/services/*/main.go to expose, not a guess.
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class ApiEndpointDef:
    service_name: str  # matches the Kubernetes Service name -> SERVED_BY target
    namespace: str
    method: str
    path: str
    description: str


@dataclass(frozen=True, slots=True)
class UiRouteDef:
    service_name: str
    namespace: str
    method: str
    path: str
    description: str


API_ENDPOINTS: list[ApiEndpointDef] = [
    ApiEndpointDef("search-api", "travel", "GET", "/api/v1/search",
                    "Search flight offers for an origin/destination/date; calls pricing-svc per candidate flight."),
    ApiEndpointDef("pricing-svc", "travel", "POST", "/api/v1/price",
                    "Compute the final price for a fare (base fare + taxes, rounded per the mounted ConfigMap)."),
    ApiEndpointDef("booking-api", "travel", "POST", "/api/v1/bookings",
                    "Charge the card via payment-svc and persist a confirmed booking."),
    ApiEndpointDef("booking-api", "travel", "GET", "/api/v1/bookings/{id}",
                    "Look up a booking's status by id."),
    ApiEndpointDef("payment-svc", "travel", "POST", "/api/v1/charge",
                    "Simulated payment gateway charge."),
]

UI_ROUTES: list[UiRouteDef] = [
    UiRouteDef("web-ui", "travel", "GET", "/", "Flight search form."),
    UiRouteDef("web-ui", "travel", "GET", "/search", "Search results: priced flight offers."),
    UiRouteDef("web-ui", "travel", "GET", "/book", "Booking form for a selected offer."),
    UiRouteDef("web-ui", "travel", "POST", "/book", "Submits the booking; renders confirmation or failure."),
]

# Service-to-service calls this SUT's code makes. Like the routes above, this
# cannot be discovered from static K8s specs (there is no manifest field for
# "this Deployment calls that Service") -- it is declared because we wrote
# sut/services/*/main.go ourselves and know the call chain. A real target
# would need this observed at runtime (e.g. from a mesh or traced requests);
# for this demo it is honest, hand-authored MANUAL fact, not a guess.
#
# This is what lets a purely UI-driven workflow step reach every transitively
# affected backend service through existing graph edges alone: WorkflowStep
# --RENDERS_ON--> UiRoute --SERVED_BY--> Service(web-ui) --BACKED_BY-->
# Workload(web-ui) --CALLS--> Service(search-api) --BACKED_BY--> ... and so
# on -- without the LLM ever having to enumerate downstream services itself.
SERVICE_CALLS: list[tuple[str, str, str]] = [  # (caller_service, callee_service, namespace)
    ("web-ui", "search-api", "travel"),
    ("web-ui", "booking-api", "travel"),
    ("search-api", "pricing-svc", "travel"),
    ("booking-api", "payment-svc", "travel"),
]
