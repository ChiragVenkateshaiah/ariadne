#!/usr/bin/env bash
# Installs kube-prometheus-stack (Prometheus + Grafana + kube-state-metrics
# + node-exporter) plus Ariadne's own ServiceMonitors and dashboards.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONTEXT="kind-ariadne"
RELEASE="kube-prometheus-stack"

helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update >/dev/null

kubectl --context "$CONTEXT" create namespace monitoring --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f -

echo "==> installing/upgrading $RELEASE (this pulls several images on first install)"
helm upgrade --install "$RELEASE" prometheus-community/kube-prometheus-stack \
  --namespace monitoring \
  --values "$ROOT/deploy/monitoring/values.yaml" \
  --kube-context "$CONTEXT" \
  --wait --timeout 5m

echo "==> applying Ariadne ServiceMonitors + dashboards"
kubectl --context "$CONTEXT" apply -f "$ROOT/deploy/monitoring/servicemonitors.yaml"
if [ -d "$ROOT/deploy/monitoring/dashboards" ] && [ -n "$(ls -A "$ROOT/deploy/monitoring/dashboards" 2>/dev/null)" ]; then
  kubectl --context "$CONTEXT" create configmap ariadne-grafana-dashboards \
    --namespace monitoring \
    --from-file="$ROOT/deploy/monitoring/dashboards" \
    --dry-run=client -o yaml \
    | kubectl --context "$CONTEXT" label -f - --local -o yaml grafana_dashboard=1 \
    | kubectl --context "$CONTEXT" apply -f -
fi

echo "==> monitoring namespace status:"
kubectl --context "$CONTEXT" get pods -n monitoring -o wide

cat <<MSG

Grafana:    kubectl --context $CONTEXT port-forward -n monitoring svc/${RELEASE}-grafana 3000:80
            http://localhost:3000  (admin / ariadne -- see deploy/monitoring/values.yaml)
Prometheus: kubectl --context $CONTEXT port-forward -n monitoring svc/${RELEASE}-prometheus 9090:9090
            http://localhost:9090
MSG
