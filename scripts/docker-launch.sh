#!/usr/bin/env bash
# One-command entrypoint for the linkc-node Docker launcher.
#
#   sudo ./scripts/docker-launch.sh --username admin # default 0.0.0.0; auto password
#   # generated password: ~/.linkc-node/launcher-credentials.json (0600 JSON)
#   sudo ./scripts/docker-launch.sh --username admin --ask-password # type your own
#   sudo ./scripts/docker-launch.sh --hash-password --hash-password-output /root/.linkc-node/password.hash
#   sudo ./scripts/docker-launch.sh --username admin --password-hash-file /root/.linkc-node/password.hash
#   sudo ./scripts/docker-launch.sh --bind 127.0.0.1 # local-only, token-only allowed
#   ./scripts/docker-launch.sh --username admin      # non-root: ~/.linkc-node
#   ./scripts/docker-launch.sh --username admin --no-browser
#
# The actual UI and Docker orchestration live in docker_launcher.py and
# docker-launcher.html next to this file.  Only the Python standard library is
# required (Python 3.8+).

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
if [[ ! -f "$SCRIPT_DIR/docker-launcher.html" ]]; then
  echo "错误: 缺少 $SCRIPT_DIR/docker-launcher.html" >&2
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
  echo "警告: 未找到 docker 命令。页面仍可生成配置，但无法启动容器。" >&2
fi

exec "$PYTHON_BIN" "$SCRIPT_DIR/docker_launcher.py" \
  --repo-root "$REPO_ROOT" \
  --html "$SCRIPT_DIR/docker-launcher.html" \
  "$@"
