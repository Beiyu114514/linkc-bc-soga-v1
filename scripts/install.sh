#!/bin/sh
set -eu

# Install linkc-node from the repository (expects ./linkc-node binary).
# Run as root: ./scripts/install.sh [config-path]
#
# Optional env overrides (used by scripts/quickstart.sh and for tests):
#   LINKC_BIN_DIR            default /usr/local/bin
#   LINKC_CONF_DIR           default /etc/linkc-node
#   LINKC_STATE_DIR          default /var/lib/linkc-node
#   LINKC_RUN_DIR            default /run/linkc-node
#   LINKC_UNIT_DIR           default /etc/systemd/system
#   LINKC_SKIP_EXAMPLE_CONFIG=1   do not drop the example config when missing
#   LINKC_SKIP_SYSTEMCTL=1        do not run systemctl daemon-reload/enable

# Remember the caller's cwd so a relative config argument keeps working.
ORIG_PWD="$(pwd)"

SCRIPT_DIR="$(cd -P -- "$(dirname -- "$0")" && pwd)"
cd "$(dirname -- "$SCRIPT_DIR")"

BIN_DIR="${LINKC_BIN_DIR:-/usr/local/bin}"
CONF_DIR="${LINKC_CONF_DIR:-/etc/linkc-node}"
CONF="${1:-$CONF_DIR/config.yaml}"
case "$CONF" in
  /*) ;;
  *) CONF="$ORIG_PWD/$CONF" ;;
esac
STATE_DIR="${LINKC_STATE_DIR:-/var/lib/linkc-node}"
RUN_DIR="${LINKC_RUN_DIR:-/run/linkc-node}"
UNIT_DIR="${LINKC_UNIT_DIR:-/etc/systemd/system}"
UNIT="$UNIT_DIR/linkc-node.service"
SKIP_EXAMPLE="${LINKC_SKIP_EXAMPLE_CONFIG:-0}"
SKIP_SYSTEMCTL="${LINKC_SKIP_SYSTEMCTL:-0}"

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
install -d -m 0700 -o linkc -g linkc "$CONF_DIR" "$STATE_DIR" "$RUN_DIR"
install -d -m 0755 -o root -g root "$BIN_DIR"
install -m 0755 -o root -g root ./linkc-node "$BIN_DIR/linkc-node"
install -d -m 0755 -o root -g root "$UNIT_DIR"
install -m 0644 deploy/linkc-node.service "$UNIT"
if [ ! -f "$CONF" ] && [ "$SKIP_EXAMPLE" != "1" ]; then
  install -m 0600 -o linkc -g linkc configs/linkc-node.example.yaml "$CONF"
  echo "installed example config at $CONF; edit it before starting"
fi
if [ "$SKIP_SYSTEMCTL" = "1" ]; then
  echo "installed. systemctl steps skipped (LINKC_SKIP_SYSTEMCTL=1)"
  exit 0
fi
systemctl daemon-reload
systemctl enable linkc-node.service
echo "installed. edit $CONF then: systemctl restart linkc-node"
