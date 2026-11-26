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
# shellcheck source=scripts/install_paths.sh
. "$SCRIPT_DIR/install_paths.sh"

BIN_DIR="${LINKC_BIN_DIR:-/usr/local/bin}"
CONF_DIR="${LINKC_CONF_DIR:-/etc/linkc-node}"
STATE_DIR="${LINKC_STATE_DIR:-/var/lib/linkc-node}"
RUN_DIR="${LINKC_RUN_DIR:-/run/linkc-node}"
UNIT_DIR="${LINKC_UNIT_DIR:-/etc/systemd/system}"
SKIP_EXAMPLE="${LINKC_SKIP_EXAMPLE_CONFIG:-0}"
SKIP_SYSTEMCTL="${LINKC_SKIP_SYSTEMCTL:-0}"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

BIN_DIR="$(normalize_install_path LINKC_BIN_DIR "$BIN_DIR" /usr/local/bin)" || exit 1
CONF_DIR="$(normalize_install_path LINKC_CONF_DIR "$CONF_DIR" /etc/linkc-node)" || exit 1
STATE_DIR="$(normalize_install_path LINKC_STATE_DIR "$STATE_DIR" /var/lib/linkc-node)" || exit 1
RUN_DIR="$(normalize_install_path LINKC_RUN_DIR "$RUN_DIR" /run/linkc-node)" || exit 1
UNIT_DIR="$(normalize_install_path LINKC_UNIT_DIR "$UNIT_DIR" /etc/systemd/system)" || exit 1
ensure_disjoint_install_paths "$BIN_DIR" "$CONF_DIR" "$STATE_DIR" "$RUN_DIR" "$UNIT_DIR" || exit 1

CONF_DEFAULT="$CONF_DIR/config.yaml"
CONF="${1:-$CONF_DEFAULT}"
case "$CONF" in
  /*) ;;
  *) CONF="$ORIG_PWD/$CONF" ;;
esac
CONF="$(normalize_install_path "config file" "$CONF" "$CONF_DEFAULT")" || exit 1
ensure_managed_file_slot "$CONF" "config file" || exit 1
UNIT="$UNIT_DIR/linkc-node.service"

if [ ! -x ./linkc-node ]; then
  echo "build first: make linux-amd64 or make build" >&2
  exit 1
fi

if ! id linkc >/dev/null 2>&1; then
  useradd --system --home "$STATE_DIR" --shell /usr/sbin/nologin linkc
fi
install -d -m 0750 -o root -g linkc "$CONF_DIR"
install -d -m 0700 -o linkc -g linkc "$STATE_DIR" "$RUN_DIR"
install -d -m 0755 -o root -g root "$BIN_DIR"
install -m 0755 -o root -g root ./linkc-node "$BIN_DIR/linkc-node"
install -d -m 0755 -o root -g root "$UNIT_DIR"
install -m 0644 deploy/linkc-node.service "$UNIT"
if [ ! -f "$CONF" ] && [ "$SKIP_EXAMPLE" != "1" ]; then
  ensure_managed_file_slot "$CONF" "config file" || exit 1
  write_managed_file "$CONF" 0640 root:linkc < configs/linkc-node.example.yaml
  echo "installed example config at $CONF; edit it before starting"
fi
if [ "$SKIP_SYSTEMCTL" = "1" ]; then
  echo "installed. systemctl steps skipped (LINKC_SKIP_SYSTEMCTL=1)"
  exit 0
fi
systemctl daemon-reload
systemctl enable linkc-node.service
echo "installed. edit $CONF then: systemctl restart linkc-node"
