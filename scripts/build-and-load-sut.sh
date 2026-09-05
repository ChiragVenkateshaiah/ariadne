#!/usr/bin/env bash
# Builds all five SUT service images and loads them straight into the kind
# nodes' containerd image store. No registry involved -- `kind load` copies
# the image directly, which is faster and avoids needing a local registry
# for a demo cluster that never leaves this machine.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="ariadne"
SERVICES=(web-ui search-api pricing-svc booking-api payment-svc)

cd "$ROOT"
for svc in "${SERVICES[@]}"; do
  echo "==> building ariadne/${svc}:dev"
  docker build -q -t "ariadne/${svc}:dev" -f "sut/services/${svc}/Dockerfile" .
done

echo "==> loading images into kind cluster '${CLUSTER}'"
for svc in "${SERVICES[@]}"; do
  kind load docker-image "ariadne/${svc}:dev" --name "$CLUSTER"
done

echo "==> done. Images present on kind nodes:"
for svc in "${SERVICES[@]}"; do
  docker exec "${CLUSTER}-control-plane" crictl images 2>/dev/null | grep "ariadne/${svc}" || true
done
