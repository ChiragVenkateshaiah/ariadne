#!/usr/bin/env bash
# Runs the Ariadne dashboard against a local SQLite DB (default ariadne.db,
# same file scripts/run_ingester.py writes to). Usage:
#   scripts/run_dashboard.sh [db_path] [port]
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
source .venv/bin/activate
export ARIADNE_DB_PATH="${1:-ariadne.db}"
PORT="${2:-8090}"
echo "dashboard reading ${ARIADNE_DB_PATH}, serving on http://localhost:${PORT}"
uvicorn dashboard.app:app --host 0.0.0.0 --port "$PORT"
