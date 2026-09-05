#!/usr/bin/env bash
# Builds and loads Go control-plane component images into kind, mirroring
# scripts/build-and-load-sut.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="ariadne"
COMPONENTS=(sensor)

cd "$ROOT"
for c in "${COMPONENTS[@]}"; do
  echo "==> building ariadne/${c}:dev"
  docker build -q -t "ariadne/${c}:dev" -f "control-plane/Dockerfile.${c}" .
  echo "==> loading ariadne/${c}:dev into kind"
  kind load docker-image "ariadne/${c}:dev" --name "$CLUSTER"
done
