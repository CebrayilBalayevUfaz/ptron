#!/usr/bin/env bash
# Build and install Puppetron as a systemd service on a Debian/Ubuntu/RHEL-like host.
# Run from the repository root as root:  sudo ./deploy/install.sh
set -euo pipefail

PREFIX=/opt/puppetron
NOVNC_VERSION=${NOVNC_VERSION:-v1.5.0}

if [[ $EUID -ne 0 ]]; then echo "run as root (sudo)"; exit 1; fi
cd "$(dirname "$0")/.."

echo "==> checking dependencies"
command -v virsh >/dev/null || echo "WARNING: virsh not found. Install libvirt-clients (Debian) / libvirt-client (RHEL)."
command -v virt-install >/dev/null || echo "NOTE: virt-install not found; VM creation will be disabled (package: virtinst / virt-install)."
command -v go >/dev/null || { echo "Go is required to build. Install from https://go.dev/dl/"; exit 1; }

echo "==> building"
go mod tidy
CGO_ENABLED=0 go build -ldflags="-s -w" -o puppetron .

echo "==> installing to $PREFIX"
install -d "$PREFIX"
install -m 0755 puppetron "$PREFIX/puppetron"

if [[ -d /usr/share/novnc/core ]]; then
  echo "==> using system noVNC at /usr/share/novnc"
  NOVNC_DIR=/usr/share/novnc
else
  if [[ ! -d "$PREFIX/novnc/core" ]]; then
    echo "==> cloning noVNC $NOVNC_VERSION"
    command -v git >/dev/null || { echo "git is required to fetch noVNC"; exit 1; }
    git clone --depth 1 --branch "$NOVNC_VERSION" https://github.com/novnc/noVNC "$PREFIX/novnc"
  fi
  NOVNC_DIR="$PREFIX/novnc"
fi

if ! id puppetron >/dev/null 2>&1; then
  echo "==> creating user puppetron"
  useradd --system --home "$PREFIX" --shell /usr/sbin/nologin puppetron
fi
getent group libvirt >/dev/null && usermod -aG libvirt puppetron || echo "WARNING: group 'libvirt' not found; make sure user puppetron may access libvirt"
install -d -o puppetron -g libvirt /var/run/puppetron-tokens

if [[ ! -f /etc/puppetron.env ]]; then
  echo "==> writing /etc/puppetron.env (random password)"
  PASS=$(head -c 12 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 16)
  sed -e "s|^PUPPETRON_PASSWORD=.*|PUPPETRON_PASSWORD=$PASS|" -e "s|^PUPPETRON_NOVNC=.*|PUPPETRON_NOVNC=$NOVNC_DIR|" deploy/puppetron.env.example > /etc/puppetron.env
  chmod 600 /etc/puppetron.env
  echo "    user: admin   password: $PASS"
fi

install -m 0644 deploy/puppetron.service /etc/systemd/system/puppetron.service
systemctl daemon-reload
systemctl enable --now puppetron
sleep 1
systemctl --no-pager status puppetron | head -n 12
echo
echo "Puppetron is running. Open http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080"
