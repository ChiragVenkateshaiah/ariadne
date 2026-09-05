#!/usr/bin/env bash
# Installs Docker Engine natively inside this WSL2 distro (no Docker Desktop
# needed). Run this AFTER uninstalling Docker Desktop on the Windows side.
# Needs sudo -- run interactively, not from an agent session.
set -euo pipefail

echo "==> installing Docker Engine via the official convenience script"
curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
sudo sh /tmp/get-docker.sh
rm -f /tmp/get-docker.sh

echo "==> adding $USER to the docker group (avoids needing sudo for every docker command)"
sudo usermod -aG docker "$USER"

echo "==> enabling and starting the docker service (systemd is active in this distro)"
sudo systemctl enable docker
sudo systemctl start docker

echo "==> verifying"
sudo docker version
sudo docker run --rm hello-world

cat <<'MSG'

------------------------------------------------------------------
Docker Engine is installed and running natively in WSL2.

IMPORTANT: your shell needs to pick up the new "docker" group membership.
Either:
  A. Close this terminal and open a new one, OR
  B. Run: newgrp docker

Then verify you can run docker WITHOUT sudo:
  docker version
  docker run --rm hello-world

Once that works, tell Claude -- it will rebuild the kind cluster and
redeploy everything from the existing scripts/ automation.
------------------------------------------------------------------
MSG
