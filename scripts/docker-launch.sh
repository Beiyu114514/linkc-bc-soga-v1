#!/usr/bin/env bash
# One-command CLI entrypoint for deploying and managing linkc-node in Docker.
#
#   sudo ./scripts/docker-launch.sh wizard            # interactive setup
#   sudo ./scripts/docker-launch.sh deploy --panel-url https://panel.example.com --node-id 12 --api-key-stdin
#   sudo ./scripts/docker-launch.sh preview --panel-url https://panel.example.com --node-id 12
#   sudo ./scripts/docker-launch.sh status            # inspect container and staged config
#   sudo ./scripts/docker-launch.sh logs -f           # follow container logs
#   sudo ./scripts/docker-launch.sh start|stop|restart|remove
#   ./scripts/docker-launch.sh --help
#
# The CLI and Docker orchestration live in docker_launcher.py next to this
# file.  Only the Python standard library is required (Python 3.8+).

set -euo pipefail

# Resolve symlinks so the script can be linked into /usr/local/bin.
SOURCE="${BASH_SOURCE[0]}"
while [[ -L "$SOURCE" ]]; do
  LINK_DIR="$(cd -P -- "$(dirname -- "$SOURCE")" && pwd)"
  SOURCE="$(readlink -- "$SOURCE")"
  [[ "$SOURCE" != /* ]] && SOURCE="$LINK_DIR/$SOURCE"
done
SCRIPT_DIR="$(cd -P -- "$(dirname -- "$SOURCE")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
PYTHON_BIN="${PYTHON_BIN:-python3}"

if [[ ! -f "$SCRIPT_DIR/docker_launcher.py" ]]; then
  echo "错误: 缺少 $SCRIPT_DIR/docker_launcher.py" >&2
  exit 1
fi

if ! command -v "$PYTHON_BIN" >/dev/null 2>&1; then
  echo "错误: 找不到 $PYTHON_BIN。请安装 Python 3.8+，或用 PYTHON_BIN=/path/to/python3 指定。" >&2
  exit 1
fi

if ! "$PYTHON_BIN" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)'; then
  echo "错误: Python 版本过低，需要 3.8+。" >&2
  "$PYTHON_BIN" --version >&2 || true
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "警告: 未找到 docker 命令。仍可使用 preview/stage 生成配置，但无法校验或启动容器。" >&2
fi

exec "$PYTHON_BIN" "$SCRIPT_DIR/docker_launcher.py" \
  --repo-root "$REPO_ROOT" \
  "$@"
