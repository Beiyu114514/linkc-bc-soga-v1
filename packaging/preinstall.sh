#!/bin/sh
set -eu

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
