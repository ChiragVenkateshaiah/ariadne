"""Mock LLM fixtures, keyed by the same `purpose` tags client.py's cache and
callers use. Each fixture is written as the answer a real Claude call should
converge on for THIS SUT (see client.py's module docstring) -- these are not
placeholder stubs, they encode actual knowledge of the demo application's
business logic so the rest of the pipeline can be built and demoed against
them before a real API key exists.
"""

from __future__ import annotations

from ariadne.llm.client import MockLLMClient

SYNTH_WORKFLOWS_FIXTURE = {
    "workflows": [
        {
            "slug": "book_one_way_flight",
            "title": "Book a one-way flight",
            "description": "A shopper searches for flights between two airports, "
                            "picks an offer, and completes payment to receive a "
                            "confirmed booking reference.",
            "business_goal": "Convert a search into a paid, confirmed booking.",
            "criticality": 0.9,
            "revenue_path": True,
            "pii_involved": True,
            "entry_route": "/",
            "persona": "anonymous shopper",
            "derived_from": "web-ui routes (/, /search, /book) and the "
                             "search-api -> pricing-svc, booking-api -> payment-svc "
                             "call chains observed in the topology graph",
            "steps": [
                {"ordinal": 1, "intent": "open the flight search page", "action": "navigate",
                 "target_hint": None, "value_expr": "/", "assertion": None, "ui_route": "GET /"},
                {"ordinal": 2, "intent": "enter the origin airport", "action": "fill",
                 "target_hint": "origin airport input", "value_expr": "{{origin}}", "assertion": None, "ui_route": "GET /"},
                {"ordinal": 3, "intent": "enter the destination airport", "action": "fill",
                 "target_hint": "destination airport input", "value_expr": "{{destination}}", "assertion": None, "ui_route": "GET /"},
                {"ordinal": 4, "intent": "enter the departure date", "action": "fill",
                 "target_hint": "departure date input", "value_expr": "{{date}}", "assertion": None, "ui_route": "GET /"},
                {"ordinal": 5, "intent": "submit the search", "action": "click",
                 "target_hint": "search submit button", "value_expr": None, "assertion": None, "ui_route": "GET /"},
                {"ordinal": 6, "intent": "results show at least one priced offer", "action": "assert",
                 "target_hint": None, "value_expr": None,
                 "assertion": "the results page lists one or more flight offers, each with a price",
                 "ui_route": "GET /search"},
                {"ordinal": 7, "intent": "select the first offer to book", "action": "click",
                 "target_hint": "first offer's book-this-flight link", "value_expr": None, "assertion": None, "ui_route": "GET /search"},
                {"ordinal": 8, "intent": "enter the passenger name", "action": "fill",
                 "target_hint": "passenger name input", "value_expr": "{{passenger_name}}", "assertion": None, "ui_route": "GET /book"},
                {"ordinal": 9, "intent": "enter payment card details", "action": "fill",
                 "target_hint": "card number input", "value_expr": "{{card_last4}}", "assertion": None, "ui_route": "GET /book"},
                {"ordinal": 10, "intent": "confirm the booking", "action": "click",
                 "target_hint": "confirm booking button", "value_expr": None, "assertion": None, "ui_route": "POST /book"},
                {"ordinal": 11, "intent": "booking is confirmed with a reference", "action": "assert",
                 "target_hint": None, "value_expr": None,
                 "assertion": "the confirmation page shows a CONFIRMED status and a non-empty booking reference",
                 "ui_route": "POST /book"},
            ],
        },
        {
            "slug": "search_flights",
            "title": "Search for flights without booking",
            "description": "A shopper searches for flights and reviews priced "
                            "offers, without proceeding to payment.",
            "business_goal": "Surface accurate, priced search results -- the "
                              "top-of-funnel step that precedes every booking.",
            "criticality": 0.4,
            "revenue_path": False,
            "pii_involved": False,
            "entry_route": "/",
            "persona": "anonymous shopper",
            "derived_from": "web-ui / and /search routes, search-api -> "
                             "pricing-svc call chain",
            "steps": [
                {"ordinal": 1, "intent": "open the flight search page", "action": "navigate",
                 "target_hint": None, "value_expr": "/", "assertion": None, "ui_route": "GET /"},
                {"ordinal": 2, "intent": "enter the origin airport", "action": "fill",
                 "target_hint": "origin airport input", "value_expr": "{{origin}}", "assertion": None, "ui_route": "GET /"},
                {"ordinal": 3, "intent": "enter the destination airport", "action": "fill",
                 "target_hint": "destination airport input", "value_expr": "{{destination}}", "assertion": None, "ui_route": "GET /"},
                {"ordinal": 4, "intent": "enter the departure date", "action": "fill",
                 "target_hint": "departure date input", "value_expr": "{{date}}", "assertion": None, "ui_route": "GET /"},
                {"ordinal": 5, "intent": "submit the search", "action": "click",
                 "target_hint": "search submit button", "value_expr": None, "assertion": None, "ui_route": "GET /"},
                {"ordinal": 6, "intent": "results show at least one priced offer", "action": "assert",
                 "target_hint": None, "value_expr": None,
                 "assertion": "the results page lists one or more flight offers, each with a price",
                 "ui_route": "GET /search"},
            ],
        },
    ]
}


def default_mock_client() -> MockLLMClient:
    client = MockLLMClient()
    client.register("synth_workflows", SYNTH_WORKFLOWS_FIXTURE)
    return client
