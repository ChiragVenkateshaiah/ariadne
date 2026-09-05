#!/usr/bin/env bash
# Applies the SUT manifests in order. Safe to re-run (idempotent apply).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTEXT="kind-ariadne"

kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/sut/00-namespace.yaml"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/sut/01-postgres.yaml"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/sut/02-pricing-flags.yaml"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/sut/10-pricing-svc.yaml"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/sut/11-search-api.yaml"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/sut/12-payment-svc.yaml"

echo "==> waiting for postgres before booking-api (which migrates on startup)"
kubectl --context "$CONTEXT" rollout status deployment/postgres -n travel --timeout=120s

kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/sut/13-booking-api.yaml"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/sut/14-web-ui.yaml"

echo "==> waiting for all SUT deployments"
for d in pricing-svc search-api payment-svc booking-api web-ui; do
  kubectl --context "$CONTEXT" rollout status deployment/"$d" -n travel --timeout=120s
done

echo "==> travel namespace status:"
kubectl --context "$CONTEXT" get pods -n travel -o wide
echo
echo "==> web-ui reachable at http://localhost:8080 (via kind extraPortMappings)"
