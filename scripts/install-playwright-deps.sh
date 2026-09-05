#!/usr/bin/env bash
# Installs the OS-level shared libraries headless Chromium needs to launch
# (libnspr4, libnss3, libatk, etc). Requires sudo -- run this yourself in an
# interactive terminal; it can't run unattended from an agent session.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../brain"
source .venv/bin/activate
playwright install-deps chromium
echo "==> verifying"
python3 -c "
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    b = p.chromium.launch()
    b.close()
print('Chromium launches OK')
"
