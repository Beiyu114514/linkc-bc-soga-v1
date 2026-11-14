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

install -d -m 0700 -o linkc -g linkc /etc/linkc-node /var/lib/linkc-node

# The package owns config.yaml as root:root; the service runs as linkc.
if [ -f /etc/linkc-node/config.yaml ]; then
    chown linkc:linkc /etc/linkc-node/config.yaml
    chmod 0600 /etc/linkc-node/config.yaml
fi

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
fi
