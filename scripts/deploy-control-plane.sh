#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTEXT="kind-ariadne"

kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/control-plane/00-namespace.yaml"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/control-plane/01-sensor-rbac.yaml"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/control-plane/02-sensor-deployment.yaml"

echo "==> waiting for sensor rollout"
kubectl --context "$CONTEXT" rollout status deployment/sensor -n ariadne-system --timeout=120s

kubectl --context "$CONTEXT" get pods -n ariadne-system -o wide
