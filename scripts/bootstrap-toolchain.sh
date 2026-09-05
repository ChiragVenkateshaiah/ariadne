#!/usr/bin/env bash
# Installs the Ariadne build toolchain into WSL. Idempotent.
# Does NOT install Docker -- see the note at the end.
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.24.0}"
KIND_VERSION="${KIND_VERSION:-v0.27.0}"
BIN="${HOME}/.local/bin"
mkdir -p "$BIN"

have() { command -v "$1" >/dev/null 2>&1; }
note() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

note "apt prerequisites"
sudo apt-get update -qq
sudo apt-get install -y -qq curl wget git make jq unzip ca-certificates \
    python3-venv python3-pip build-essential

if have go; then
  note "go already present: $(go version)"
else
  note "installing Go ${GO_VERSION}"
  wget -qO /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf /tmp/go.tgz
  rm /tmp/go.tgz
fi

if have kubectl; then
  note "kubectl already present"
else
  note "installing kubectl"
  KV=$(curl -sL https://dl.k8s.io/release/stable.txt)
  curl -sLo "$BIN/kubectl" "https://dl.k8s.io/release/${KV}/bin/linux/amd64/kubectl"
  chmod +x "$BIN/kubectl"
fi

if have kind; then
  note "kind already present"
else
  note "installing kind ${KIND_VERSION}"
  curl -sLo "$BIN/kind" "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
  chmod +x "$BIN/kind"
fi

if have buf; then
  note "buf already present"
else
  note "installing buf"
  curl -sSL "https://github.com/bufbuild/buf/releases/latest/download/buf-Linux-x86_64" -o "$BIN/buf"
  chmod +x "$BIN/buf"
fi

if have helm; then
  note "helm already present"
else
  note "installing helm (for Calico / toxiproxy charts)"
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
fi

# PATH wiring
PROFILE="${HOME}/.bashrc"
if ! grep -q 'ariadne-toolchain' "$PROFILE" 2>/dev/null; then
  note "adding PATH entries to ${PROFILE}"
  cat >> "$PROFILE" <<'RC'

# ariadne-toolchain
export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin"
RC
fi
export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin:$BIN"

note "versions"
go version        || echo "go MISSING"
kubectl version --client --output=yaml 2>/dev/null | head -3 || echo "kubectl MISSING"
kind --version    || echo "kind MISSING"
buf --version     || echo "buf MISSING"
python3 --version

cat <<'MSG'

------------------------------------------------------------------
Docker is NOT installed by this script.

Docker Desktop is running on Windows but WSL integration is OFF, so
`docker` is unreachable from this distro. kind needs a working Docker
socket here.

Fix (one of):
  A. Docker Desktop -> Settings -> Resources -> WSL Integration
     -> enable for this distro -> Apply & Restart.   [recommended]
  B. Install Docker Engine natively in WSL:
       curl -fsSL https://get.docker.com | sudo sh
       sudo usermod -aG docker "$USER"     # then reopen the shell
     and disable Docker Desktop's WSL integration to avoid a clash.

Verify with:  docker run --rm hello-world
------------------------------------------------------------------
MSG
