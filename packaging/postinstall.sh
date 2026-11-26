#!/bin/sh
set -eu

# Create the system account used by linkc-node.service and hand the config and
# state directories to it. Runs for both deb and rpm; safe to re-run on upgrade.

if ! getent group linkc >/dev/null 2>&1; then
    groupadd --system linkc
fi
if ! getent passwd linkc >/dev/null 2>&1; then
    useradd --system \
        --gid linkc \
        --home-dir /var/lib/linkc-node \
        --shell /usr/sbin/nologin \
        linkc
fi

install -d -m 0750 -o root -g linkc /etc/linkc-node
install -d -m 0700 -o linkc -g linkc /var/lib/linkc-node
if [ -d /var/lib/linkc-node ] && [ ! -L /var/lib/linkc-node ]; then
    chown -hR linkc:linkc /var/lib/linkc-node
fi

# The service can read the config but cannot replace it.
if [ -f /etc/linkc-node/config.yaml ] && [ ! -L /etc/linkc-node/config.yaml ]; then
    chown root:linkc /etc/linkc-node/config.yaml
    chmod 0640 /etc/linkc-node/config.yaml
fi
if [ -d /etc/linkc-node ] && [ ! -L /etc/linkc-node ]; then
    chown root:linkc /etc/linkc-node
    chmod 0750 /etc/linkc-node
fi

is_upgrade=0
if [ "${1:-}" = "configure" ] && [ -n "${2:-}" ]; then
    # Debian/Ubuntu postinst configure <most-recently-configured-version>
    is_upgrade=1
elif [ -n "${1:-}" ]; then
    case "$1" in
        [0-9]*)
            if [ "$1" -ge 2 ] 2>/dev/null; then
                # RPM %post $1 >= 2 indicates upgrade.
                is_upgrade=1
            fi
            ;;
    esac
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    if [ "$is_upgrade" -eq 1 ]; then
        systemctl try-restart linkc-node.service || true
        for unit in $(systemctl list-units --type=service --state=active --no-legend 'linkc-node@*.service' 2>/dev/null | awk '{print $1}'); do
            systemctl try-restart "$unit" || true
        done
    fi
fi
