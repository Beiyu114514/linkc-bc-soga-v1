#!/bin/sh
set -eu

# Install linkc-node from the repository (expects ./linkc-node binary).
# Run as root: ./scripts/install.sh [config-path]

BIN_DIR=/usr/local/bin
CONF_DIR=/etc/linkc-node
CONF="${1:-$CONF_DIR/config.yaml}"
STATE_DIR=/var/lib/linkc-node
UNIT=/etc/systemd/system/linkc-node.service

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi
if [ ! -x ./linkc-node ]; then
  echo "build first: make linux-amd64 or make build" >&2
  exit 1
fi

if ! id linkc >/dev/null 2>&1; then
  useradd --system --home "$STATE_DIR" --shell /usr/sbin/nologin linkc
fi
install -d -m 0700 -o linkc -g linkc "$CONF_DIR" "$STATE_DIR"
install -m 0755 -o root -g root ./linkc-node "$BIN_DIR/linkc-node"
install -m 0644 deploy/linkc-node.service "$UNIT"
if [ ! -f "$CONF" ]; then
  install -m 0600 -o linkc -g linkc configs/linkc-node.example.yaml "$CONF"
  echo "installed example config at $CONF; edit it before starting"
fi
systemctl daemon-reload
systemctl enable linkc-node.service
echo "installed. edit $CONF then: systemctl restart linkc-node"
