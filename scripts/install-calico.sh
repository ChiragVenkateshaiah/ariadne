#!/usr/bin/env bash
# Installs Calico as the cluster's CNI. Required because kind's default CNI
# (kindnet) does not enforce NetworkPolicy -- without this every segmentation
# conformance test would silently "pass" regardless of policy state.
set -euo pipefail

CALICO_VERSION="${CALICO_VERSION:-v3.28.0}"
CONTEXT="kind-ariadne"

echo "==> installing Calico ${CALICO_VERSION} (operator + custom-resources)"
kubectl --context "$CONTEXT" create -f \
  "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/tigera-operator.yaml" \
  2>&1 | grep -v 'already exists' || true

# Match the podSubnet set in deploy/kind/cluster.yaml.
cat <<'YAML' | kubectl --context "$CONTEXT" apply -f -
apiVersion: operator.tigera.io/v1
kind: Installation
metadata:
  name: default
spec:
  calicoNetwork:
    # VXLAN-only overlay: BGP must be explicitly disabled or calico-node's
    # readiness probe fails waiting on a BIRD socket that VXLAN never starts.
    bgp: Disabled
    ipPools:
      - blockSize: 26
        cidr: 192.168.0.0/16
        encapsulation: VXLANCrossSubnet
        natOutgoing: Enabled
        nodeSelector: all()
---
apiVersion: operator.tigera.io/v1
kind: APIServer
metadata:
  name: default
spec: {}
YAML

echo "==> waiting for the operator to create calico-system workloads"
# The Installation CR was just applied; the operator takes a few seconds to
# actually create the calico-node DaemonSet. `kubectl wait` on a resource that
# doesn't exist yet fails immediately with "no matching resources found"
# rather than waiting for it to appear -- so poll for existence first.
for i in $(seq 1 60); do
  if kubectl --context "$CONTEXT" get daemonset calico-node -n calico-system >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

echo "==> waiting for calico-node DaemonSet rollout (this can take a couple minutes on first pull)"
kubectl --context "$CONTEXT" rollout status daemonset/calico-node -n calico-system --timeout=300s

echo "==> waiting for remaining calico-system pods to become ready"
kubectl --context "$CONTEXT" wait --for=condition=Ready pods --all -n calico-system --timeout=300s || true
kubectl --context "$CONTEXT" wait --for=condition=Ready pods --all -n tigera-operator --timeout=120s || true

echo "==> calico-system status:"
kubectl --context "$CONTEXT" get pods -n calico-system -o wide
kubectl --context "$CONTEXT" get pods -n tigera-operator -o wide

echo "==> verifying NetworkPolicy enforcement actually works (smoke test)"
kubectl --context "$CONTEXT" create namespace ariadne-netpol-smoke --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f -
kubectl --context "$CONTEXT" run smoke-source --context "$CONTEXT" -n ariadne-netpol-smoke --image=busybox:1.36 --restart=Never --command -- sleep 3600 >/dev/null
kubectl --context "$CONTEXT" run smoke-target -n ariadne-netpol-smoke --image=nginx:alpine --restart=Never --labels=app=smoke-target >/dev/null
kubectl --context "$CONTEXT" wait --for=condition=Ready pod/smoke-source -n ariadne-netpol-smoke --timeout=60s
kubectl --context "$CONTEXT" wait --for=condition=Ready pod/smoke-target -n ariadne-netpol-smoke --timeout=60s
TARGET_IP=$(kubectl --context "$CONTEXT" get pod smoke-target -n ariadne-netpol-smoke -o jsonpath='{.status.podIP}')

echo "    baseline: source -> target with NO policy (expect success)"
if kubectl --context "$CONTEXT" exec -n ariadne-netpol-smoke smoke-source -- wget -q -T 3 -O- "http://${TARGET_IP}" >/dev/null 2>&1; then
  echo "    OK: reachable before policy, as expected"
else
  echo "    UNEXPECTED: unreachable even before any policy -- investigate before trusting later results"
fi

cat <<YAML | kubectl --context "$CONTEXT" apply -f -
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-all
  namespace: ariadne-netpol-smoke
spec:
  podSelector: {}
  policyTypes: ["Ingress"]
YAML
sleep 5

echo "    after deny-all: source -> target (expect BLOCKED)"
if kubectl --context "$CONTEXT" exec -n ariadne-netpol-smoke smoke-source -- wget -q -T 3 -O- "http://${TARGET_IP}" >/dev/null 2>&1; then
  echo "    FAIL: still reachable -- NetworkPolicy is NOT being enforced. Do not trust conformance results."
  exit 1
else
  echo "    PASS: blocked as expected -- Calico is enforcing NetworkPolicy."
fi

kubectl --context "$CONTEXT" delete namespace ariadne-netpol-smoke --wait=false
echo "==> Calico install verified."
