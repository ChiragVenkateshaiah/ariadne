#!/usr/bin/env bash
# Creates the Ariadne kind cluster with audit logging wired in and the
# default CNI disabled (Calico goes in next, via install-calico.sh).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AUDIT_POLICY_DIR="${ROOT}/deploy/audit-policy"
AUDIT_LOG_DIR="${AUDIT_LOG_DIR:-/tmp/ariadne-audit-logs}"

mkdir -p "$AUDIT_LOG_DIR"

TMPL="${ROOT}/deploy/kind/cluster.yaml"
RENDERED="$(mktemp /tmp/ariadne-kind-cluster.XXXXXX.yaml)"
trap 'rm -f "$RENDERED"' EXIT

sed \
  -e "s|__AUDIT_POLICY_DIR__|${AUDIT_POLICY_DIR}|g" \
  -e "s|__AUDIT_LOG_DIR__|${AUDIT_LOG_DIR}|g" \
  "$TMPL" > "$RENDERED"

echo "==> rendered kind config: $RENDERED"
echo "==> audit policy dir:     $AUDIT_POLICY_DIR"
echo "==> audit log dir:        $AUDIT_LOG_DIR"

if kind get clusters 2>/dev/null | grep -qx ariadne; then
  echo "==> cluster 'ariadne' already exists, skipping create"
else
  kind create cluster --config "$RENDERED"
fi

kubectl cluster-info --context kind-ariadne
echo "==> nodes:"
kubectl get nodes -o wide
