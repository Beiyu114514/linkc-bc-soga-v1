#!/bin/sh
set -eu

is_remove=0
case "${1:-}" in
    remove|purge)
        is_remove=1
        ;;
    0)
        # RPM passes 0 on uninstall, >= 1 on upgrade.
        is_remove=1
        ;;
esac

if [ "$is_remove" -eq 1 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl stop linkc-node.service || true
    systemctl disable linkc-node.service || true
    for unit in $(systemctl list-units --type=service --all --no-legend 'linkc-node@*.service' 2>/dev/null | awk '{print $1}'); do
        systemctl stop "$unit" || true
        systemctl disable "$unit" || true
    done
fi
