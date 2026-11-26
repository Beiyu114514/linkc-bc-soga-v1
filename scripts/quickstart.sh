#!/usr/bin/env bash
# 一键启动 linkc-node：生成配置 -> 本地校验 -> 安装并启动服务 -> 等待 ready。
#
#   sudo ./scripts/quickstart.sh --panel-url https://panel.example.com --node-id 12 \
#     --api-key-stdin < /etc/linkc-node/api-key
#   sudo ./scripts/quickstart.sh wizard                 # 交互式（逐步询问）
#   ./scripts/quickstart.sh --dry-run                    # 只打印配置与将执行的命令
#   ./scripts/quickstart.sh --mode docker --panel-url ... --node-id 12 --api-key-env PANEL_API_KEY
#
# 模式:
#   systemd     构建/安装二进制、systemd unit，写入 /etc/linkc-node 并启动（默认）
#   docker      转调 scripts/docker-launch.sh deploy（容器方式，需要 Docker）
#   foreground  前台运行，不安装任何东西（调试用）
#
# 详细说明见 docs/quickstart.md。

set -euo pipefail

SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=scripts/install_paths.sh
. "$SCRIPT_DIR/install_paths.sh"

# 路径（测试/预发环境可用环境变量覆盖；正式环境保持默认值）
BIN_DIR_INPUT="${LINKC_BIN_DIR:-/usr/local/bin}"
CONF_DIR_INPUT="${LINKC_CONF_DIR:-/etc/linkc-node}"
STATE_DIR_INPUT="${LINKC_STATE_DIR:-/var/lib/linkc-node}"
RUN_DIR_INPUT="${LINKC_RUN_DIR:-/run/linkc-node}"
UNIT_DIR_INPUT="${LINKC_UNIT_DIR:-/etc/systemd/system}"
UNIT_NAME="linkc-node"
SKIP_SYSTEMCTL="${LINKC_SKIP_SYSTEMCTL:-0}"

CONF=""
API_KEY_PATH=""
WHITELIST_PATH=""
SOCKET_PATH=""
LOCAL_BIN="$REPO_ROOT/linkc-node"

DEFAULT_IMAGE="ghcr.io/beiyu114514/linkc-bc-soga-v1:latest"

# 参数默认值
MODE="auto"
PANEL_URL=""
NODE_ID=""
PROFILE="sspanel-uim-soga"
LISTEN="0.0.0.0"
API_KEY=""
API_KEY_ENV=""
API_KEY_FILE_SRC=""
READ_KEY_STDIN=0
KEY_GIVEN=0
CERT_FILE=""
KEY_FILE=""
LIMITS_MODE="enforce"
AUDIT_ENABLED="true"
AUDIT_ACTION="report"
GEODATA_DIR="/usr/share/xray"
LOG_LEVEL="info"
LOG_FORMAT="json"
ALLOW_HTTP="false"
IMAGE="${LINKC_IMAGE:-$DEFAULT_IMAGE}"
DRY_RUN=0
STAGE_ONLY=0
FORCE=0
REBUILD=0
WAIT_SECONDS=30

log() { printf '==> %s\n' "$*"; }
warn() { printf '警告: %s\n' "$*" >&2; }
die() { printf '错误: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
一键启动 linkc-node

用法:
  sudo ./scripts/quickstart.sh [选项]
  ./scripts/quickstart.sh wizard [选项]

必填（首次部署，或使用 wizard 交互输入）:
  --panel-url URL        面板地址，例如 https://panel.example.com/
  --node-id ID           面板上的节点 ID
  面板 API Key 四选一（也可用环境变量 PANEL_API_KEY）:
  --api-key-stdin        从标准输入读取一行
  --api-key-env NAME     从环境变量 NAME 读取
  --api-key-file PATH    从文件读取
  --api-key VALUE        直接写在命令行（会出现在 ps，不推荐）

模式与动作:
  --mode auto|systemd|docker|foreground   默认 auto（有 systemd 用 systemd，否则 docker）
  --stage-only           只写配置并本地校验，不启动服务
  --dry-run              只打印配置与将执行的命令，不写文件
  --force                覆盖已存在的 config.yaml
  --build                强制重新执行 make build
  --wait SECONDS         启动后等待 ready 的秒数，默认 30（0 = 不检查就绪）

节点选项:
  --profile NAME         面板协议，默认 sspanel-uim-soga
  --listen IP            监听地址，默认 0.0.0.0（端口由面板下发）
  --cert-file PATH       TLS 证书（与 --key-file 成对）
  --key-file PATH        TLS 私钥
  --limits-mode MODE     enforce|report|off，默认 enforce
  --audit-action ACTION  report|block，默认 report（建议先观察）
  --no-audit             关闭审计规则
  --allow-http           允许 http:// 面板（仅测试）
  --geodata-dir DIR      geoip.dat/geosite.dat 所在目录，默认 /usr/share/xray
  --log-level LEVEL      debug|info|warn|error，默认 info
  --log-format FORMAT    json|text，默认 json

Docker 模式:
  --image IMAGE          默认 ghcr.io/beiyu114514/linkc-bc-soga-v1:latest

其它:
  -h, --help             显示本帮助

示例:
  # 非交互一键启动（API Key 从标准输入，不出现在 ps）
  printf '%s\n' "$PANEL_API_KEY" | sudo ./scripts/quickstart.sh \
    --panel-url https://panel.example.com --node-id 12 --api-key-stdin

  # 交互式向导
  sudo ./scripts/quickstart.sh wizard

  # 先看要做什么
  ./scripts/quickstart.sh --dry-run \
    --panel-url https://panel.example.com --node-id 12 --api-key-env PANEL_API_KEY
EOF
}

# 支持 --flag=value 写法
normalize_args() {
  local -a out=()
  local a
  for a in "$@"; do
    if [[ "$a" == --*=* ]]; then
      out+=("${a%%=*}" "${a#*=}")
    else
      out+=("$a")
    fi
  done
  if [[ ${#out[@]} -gt 0 ]]; then
    printf '%s\n' "${out[@]}"
  fi
}
need_value() {
  [[ $# -ge 2 && -n "${2:-}" ]] || die "$1 需要一个取值"
}

WIZARD=0
SUBCMD=()
if [[ $# -gt 0 && ( "$1" == "wizard" || "$1" == "init" ) ]]; then
  WIZARD=1
  SUBCMD=("$1")
  shift
fi

# ARGS 用于解析（不含子命令），ORIG_ARGS 用于 sudo 重新执行时原样重放
ARGS_SRC=("$@")
ORIG_ARGS=()
if [[ ${#SUBCMD[@]} -gt 0 ]]; then
  ORIG_ARGS+=("${SUBCMD[@]}")
fi
ORIG_ARGS+=("$@")
mapfile -t ARGS < <(normalize_args "${ARGS_SRC[@]}")
if [[ ${#ARGS[@]} -gt 0 ]]; then
  set -- "${ARGS[@]}"
else
  set --
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --panel-url) need_value "$@"; PANEL_URL="$2"; shift 2 ;;
    --node-id) need_value "$@"; NODE_ID="$2"; shift 2 ;;
    --profile) need_value "$@"; PROFILE="$2"; shift 2 ;;
    --listen) need_value "$@"; LISTEN="$2"; shift 2 ;;
    --api-key) need_value "$@"; API_KEY="$2"; KEY_GIVEN=1; shift 2 ;;
    --api-key-env) need_value "$@"; API_KEY_ENV="$2"; KEY_GIVEN=1; shift 2 ;;
    --api-key-file) need_value "$@"; API_KEY_FILE_SRC="$2"; KEY_GIVEN=1; shift 2 ;;
    --api-key-stdin) READ_KEY_STDIN=1; KEY_GIVEN=1; shift ;;
    --cert-file) need_value "$@"; CERT_FILE="$2"; shift 2 ;;
    --key-file) need_value "$@"; KEY_FILE="$2"; shift 2 ;;
    --limits-mode) need_value "$@"; LIMITS_MODE="$2"; shift 2 ;;
    --audit-action) need_value "$@"; AUDIT_ACTION="$2"; shift 2 ;;
    --no-audit) AUDIT_ENABLED="false"; shift ;;
    --geodata-dir) need_value "$@"; GEODATA_DIR="$2"; shift 2 ;;
    --log-level) need_value "$@"; LOG_LEVEL="$2"; shift 2 ;;
    --log-format) need_value "$@"; LOG_FORMAT="$2"; shift 2 ;;
    --allow-http) ALLOW_HTTP="true"; shift ;;
    --image) need_value "$@"; IMAGE="$2"; shift 2 ;;
    --mode) need_value "$@"; MODE="$2"; shift 2 ;;
    --stage-only) STAGE_ONLY=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --force) FORCE=1; shift ;;
    --build) REBUILD=1; shift ;;
    --wait) need_value "$@"; WAIT_SECONDS="$2"; shift 2 ;;
    *) die "未知参数: $1（--help 查看用法）" ;;
  esac
done

case "$MODE" in
  auto|systemd|docker|foreground) ;;
  *) die "--mode 只能是 auto|systemd|docker|foreground" ;;
esac
case "$LIMITS_MODE" in
  enforce|report|off) ;;
  *) die "--limits-mode 只能是 enforce|report|off" ;;
esac
case "$AUDIT_ACTION" in
  report|block) ;;
  *) die "--audit-action 只能是 report|block" ;;
esac
case "$LOG_LEVEL" in
  debug|info|warn|error) ;;
  *) die "--log-level 只能是 debug|info|warn|error" ;;
esac
case "$LOG_FORMAT" in
  json|text) ;;
  *) die "--log-format 只能是 json|text" ;;
esac
[[ "$WAIT_SECONDS" =~ ^[0-9]+$ ]] || die "--wait 必须是非负整数秒"
WAIT_SECONDS=$((10#$WAIT_SECONDS))
[[ "$WAIT_SECONDS" -ge 0 ]] || die "--wait 超出有效范围"
if [[ -n "$API_KEY_ENV" && ! "$API_KEY_ENV" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
  die "--api-key-env 必须是有效的环境变量名"
fi

BIN_DIR="$(normalize_install_path LINKC_BIN_DIR "$BIN_DIR_INPUT" /usr/local/bin)" || exit 1
CONF_DIR="$(normalize_install_path LINKC_CONF_DIR "$CONF_DIR_INPUT" /etc/linkc-node)" || exit 1
STATE_DIR="$(normalize_install_path LINKC_STATE_DIR "$STATE_DIR_INPUT" /var/lib/linkc-node)" || exit 1
RUN_DIR="$(normalize_install_path LINKC_RUN_DIR "$RUN_DIR_INPUT" /run/linkc-node)" || exit 1
UNIT_DIR="$(normalize_install_path LINKC_UNIT_DIR "$UNIT_DIR_INPUT" /etc/systemd/system)" || exit 1
ensure_disjoint_install_paths "$BIN_DIR" "$CONF_DIR" "$STATE_DIR" "$RUN_DIR" "$UNIT_DIR" || exit 1

CONF="$CONF_DIR/config.yaml"
API_KEY_PATH="$CONF_DIR/api-key"
WHITELIST_PATH="$CONF_DIR/whitelist.txt"
SOCKET_PATH="$RUN_DIR/admin.sock"
ensure_managed_file_slot "$CONF" "config file" || exit 1
ensure_managed_file_slot "$API_KEY_PATH" "API Key file" || exit 1
ensure_managed_file_slot "$WHITELIST_PATH" "whitelist file" || exit 1

have_systemd() { [[ -d /run/systemd/system ]] && command -v systemctl >/dev/null 2>&1; }
have_docker() { command -v docker >/dev/null 2>&1; }

resolve_mode() {
  [[ "$MODE" == "auto" ]] || return 0
  if have_systemd; then
    MODE="systemd"
  elif have_docker; then
    MODE="docker"
  else
    MODE="foreground"
  fi
}
resolve_mode

# ---- 提权 -------------------------------------------------------------------
# 必须早于向导输入与 --api-key-stdin 读取：sudo 重新执行会丢失已输入的内容。
# sudo 默认清空环境，因此逐个显式保留必要的变量。
ESCALATE_ENV_NAMES=(
  LINKC_BIN_DIR LINKC_CONF_DIR LINKC_STATE_DIR LINKC_RUN_DIR LINKC_UNIT_DIR
  LINKC_SKIP_SYSTEMCTL LINKC_IMAGE LINKC_SKIP_EXAMPLE_CONFIG
)

escalate_preserving_env() { # 保留 ESCALATE_ENV_NAMES 与 API Key 相关变量
  local -a names=("${ESCALATE_ENV_NAMES[@]}")
  local -a extra=()
  [[ -n "${API_KEY_ENV:-}" ]] && extra+=("$API_KEY_ENV")
  extra+=(PANEL_API_KEY)

  local v name
  for name in "${extra[@]}"; do
    [[ -n "$name" && -n "${!name:-}" ]] || continue
    names+=("$name")
  done

  local seen=""
  for v in "${names[@]}"; do
    [[ -n "${!v:-}" ]] || continue
    case ",$seen," in *",$v,"*) continue ;; esac
    if [[ -n "$seen" ]]; then
      seen="$seen,$v"
    else
      seen="$v"
    fi
  done

  log "需要 root 权限，使用 sudo 重新执行"
  if [[ -z "$seen" ]]; then
    exec sudo -- "$BASH" "$0" "${ORIG_ARGS[@]}"
  fi
  exec sudo --preserve-env="$seen" -- "$BASH" "$0" "${ORIG_ARGS[@]}"
}

if [[ "$(id -u)" -ne 0 && "$DRY_RUN" == 0 ]]; then
  need_root=0
  if [[ "$MODE" == "systemd" ]]; then
    need_root=1
  elif [[ "$MODE" == "docker" ]] && ! docker info >/dev/null 2>&1; then
    # 当前用户没有 docker 权限时，用 sudo 走 root 的 docker
    need_root=1
  fi
  if [[ "$need_root" == 1 ]]; then
    command -v sudo >/dev/null 2>&1 || die "$MODE 模式需要 root：请用 root 运行或安装 sudo"
    escalate_preserving_env
  fi
fi

ensure_managed_file_slot "$CONF" "config file" || exit 1
ensure_managed_file_slot "$API_KEY_PATH" "API Key file" || exit 1
ensure_managed_file_slot "$WHITELIST_PATH" "whitelist file" || exit 1

# ---- 交互式向导 -------------------------------------------------------------
prompt() { # prompt VAR "说明" "默认值"
  local __var="$1" __label="$2" __default="${3:-}" __input=""
  if [[ -n "$__default" ]]; then
    read -r -p "$__label [$__default]: " __input || true
    [[ -z "$__input" ]] && __input="$__default"
  else
    while [[ -z "$__input" ]]; do
      read -r -p "$__label: " __input || die "输入结束，已取消"
    done
  fi
  printf -v "$__var" '%s' "$__input"
}

# 可留空的输入：回车或 EOF 都表示"不填"
prompt_optional() { # prompt_optional VAR "说明" "默认值"
  local __var="$1" __label="$2" __default="${3:-}" __input=""
  read -r -p "$__label${__default:+ [$__default]}: " __input || true
  [[ -z "$__input" ]] && __input="$__default"
  printf -v "$__var" '%s' "$__input"
}

prompt_secret() { # prompt_secret VAR "说明"
  local __var="$1" __label="$2" __input=""
  while [[ -z "$__input" ]]; do
    read -r -s -p "$__label: " __input || die "输入结束，已取消"
    printf '\n'
  done
  printf -v "$__var" '%s' "$__input"
}

if [[ "$WIZARD" == 1 ]]; then
  log "交互式向导（直接回车使用默认值）"
  [[ -z "$PANEL_URL" ]] && prompt PANEL_URL "面板地址（https://...）"
  [[ -z "$NODE_ID" ]] && prompt NODE_ID "节点 ID"
  [[ "$PROFILE" == "sspanel-uim-soga" ]] && prompt PROFILE "面板协议" "sspanel-uim-soga"
  if [[ "$KEY_GIVEN" == 0 && ! -s "$API_KEY_PATH" ]]; then
    prompt_secret API_KEY "面板 API Key"
    KEY_GIVEN=1
  fi
  prompt LISTEN "监听地址" "$LISTEN"
  prompt AUDIT_ACTION "审计动作 (report/block)" "$AUDIT_ACTION"
  prompt LIMITS_MODE "限速模式 (enforce/report/off)" "$LIMITS_MODE"
  prompt MODE "启动模式 (systemd/docker/foreground)" "$MODE"
  if [[ "$MODE" != "docker" ]]; then
    prompt_optional CERT_FILE "TLS 证书路径（留空表示由面板决定）" "$CERT_FILE"
    if [[ -n "$CERT_FILE" ]]; then
      prompt_optional KEY_FILE "TLS 私钥路径" "$KEY_FILE"
      [[ -n "$KEY_FILE" ]] || die "提供了证书就必须同时提供私钥（--key-file）"
    fi
  fi
fi

# 向导会直接覆盖这些值，不能只校验命令行参数。
case "$MODE" in
  auto|systemd|docker|foreground) ;;
  *) die "启动模式只能是 auto|systemd|docker|foreground" ;;
esac
case "$LIMITS_MODE" in
  enforce|report|off) ;;
  *) die "限速模式只能是 enforce|report|off" ;;
esac
case "$AUDIT_ACTION" in
  report|block) ;;
  *) die "审计动作只能是 report|block" ;;
esac
resolve_mode

# ---- 取值校验 ---------------------------------------------------------------
[[ -n "$PANEL_URL" ]] || die "缺少 --panel-url（或用 wizard 交互输入）"
[[ -n "$NODE_ID" ]] || die "缺少 --node-id"

if [[ "$PANEL_URL" != https://* ]]; then
  if [[ "$ALLOW_HTTP" == "true" && "$PANEL_URL" == http://* ]]; then
    warn "面板地址使用 http://，仅应用于测试环境"
  else
    die "--panel-url 必须以 https:// 开头（测试可加 --allow-http）"
  fi
fi
# 这些值会写进 YAML（引号包裹），因此不能含引号、反斜杠或换行
yaml_safe() { # yaml_safe 值 参数名
  case "$1" in
    *\"*|*\\*) die "$2 不能包含引号或反斜杠" ;;
  esac
  case "$1" in
    *$'\n'*) die "$2 不能包含换行" ;;
  esac
}
case "$PANEL_URL" in
  *" "*) die "--panel-url 不能包含空格" ;;
esac
case "$NODE_ID" in
  *" "*) die "--node-id 不能包含空格" ;;
esac
case "$LISTEN" in
  *" "*) die "--listen 不能包含空格" ;;
esac
yaml_safe "$PANEL_URL" "--panel-url"
yaml_safe "$NODE_ID" "--node-id"
yaml_safe "$LISTEN" "--listen"
yaml_safe "$PROFILE" "--profile"
yaml_safe "$GEODATA_DIR" "--geodata-dir"
yaml_safe "$CERT_FILE" "--cert-file"
yaml_safe "$KEY_FILE" "--key-file"

# ---- API Key ----------------------------------------------------------------
read_api_key() {
  local key=""
  if [[ "$READ_KEY_STDIN" == 1 ]]; then
    IFS= read -r key || true
  elif [[ -n "$API_KEY_FILE_SRC" ]]; then
    [[ -r "$API_KEY_FILE_SRC" ]] || die "无法读取 --api-key-file: $API_KEY_FILE_SRC"
    IFS= read -r key < "$API_KEY_FILE_SRC" || true
  elif [[ -n "$API_KEY_ENV" ]]; then
    [[ -n "${!API_KEY_ENV:-}" ]] || die "环境变量 $API_KEY_ENV 为空"
    key="${!API_KEY_ENV}"
  elif [[ -n "$API_KEY" ]]; then
    key="$API_KEY"
  elif [[ -n "${PANEL_API_KEY:-}" ]]; then
    key="$PANEL_API_KEY"
  fi
  # 去首尾空白（与 linkc-node 读取 api_key_file 的行为一致）
  key="${key#"${key%%[![:space:]]*}"}"
  key="${key%"${key##*[![:space:]]}"}"
  printf '%s' "$key"
}

NEW_API_KEY=""
if [[ "$KEY_GIVEN" == 1 ]]; then
  NEW_API_KEY="$(read_api_key)"
  [[ -n "$NEW_API_KEY" ]] || die "API Key 为空"
fi
if [[ -z "$NEW_API_KEY" && ! -s "$API_KEY_PATH" ]]; then
  die "缺少面板 API Key：--api-key-stdin / --api-key-env / --api-key-file / --api-key，或已有 $API_KEY_PATH"
fi

# ---- 配置渲染 ---------------------------------------------------------------
render_config() {
  local tls_block=""
  if [[ -n "$CERT_FILE" || -n "$KEY_FILE" ]]; then
    [[ -n "$CERT_FILE" && -n "$KEY_FILE" ]] || die "--cert-file 与 --key-file 必须成对提供"
    tls_block="    certificates:
      - cert_file: \"$CERT_FILE\"
        key_file: \"$KEY_FILE\""
  else
    tls_block="    certificates: []"
  fi

  cat <<EOF
# 由 scripts/quickstart.sh 生成，可直接手工修改后 systemctl restart linkc-node
panel:
  base_url: "$PANEL_URL"
  profile: "$PROFILE"
  api_key_file: "$API_KEY_PATH"
  node_id: "$NODE_ID"
  node_type: "vless"
  timeout: 15s
  allow_http: $ALLOW_HTTP
  follow_redirects: false
  tls:
    verify: true

node:
  listen: "$LISTEN"
  tls:
$tls_block

  limits:
    mode: "$LIMITS_MODE"

  audit:
    enabled: $AUDIT_ENABLED
    action: "$AUDIT_ACTION"
    rule_mode: "auto"
    whitelist_file: "$WHITELIST_PATH"

  geodata:
    dir: "$GEODATA_DIR"

storage:
  path: "$STATE_DIR"
  max_journal_bytes: 67108864

logging:
  level: "$LOG_LEVEL"
  format: "$LOG_FORMAT"

admin:
  socket: "$SOCKET_PATH"
  metrics: true
  pprof: false
EOF
}

# ---- Docker 模式 ------------------------------------------------------------
docker_deploy() {
  local -a cmd=("$SCRIPT_DIR/docker-launch.sh" deploy
    --panel-url "$PANEL_URL" --node-id "$NODE_ID" --profile "$PROFILE"
    --listen "$LISTEN" --image "$IMAGE"
    --limits-mode "$LIMITS_MODE" --audit-action "$AUDIT_ACTION"
    --log-level "$LOG_LEVEL" --log-format "$LOG_FORMAT")
  [[ "$AUDIT_ENABLED" == "true" ]] || cmd+=(--no-audit-enabled)
  [[ "$ALLOW_HTTP" == "true" ]] && cmd+=(--allow-http)
  [[ -n "$CERT_FILE" ]] && cmd+=(--cert-file "$CERT_FILE")
  [[ -n "$KEY_FILE" ]] && cmd+=(--key-file "$KEY_FILE")
  [[ "$STAGE_ONLY" == 1 ]] && cmd+=(--stage-only)

  local stdin_key=0
  if [[ -z "$NEW_API_KEY" && -s "$API_KEY_PATH" ]]; then
    # 复用已有 api-key 文件（例如从 systemd 模式切到容器模式）
    NEW_API_KEY="$(head -n 1 -- "$API_KEY_PATH")"
  fi
  if [[ -n "$NEW_API_KEY" ]]; then
    if [[ "$READ_KEY_STDIN" == 1 || -z "$API_KEY_ENV" ]]; then
      cmd+=(--api-key-stdin)
      stdin_key=1
    else
      cmd+=(--api-key-env "$API_KEY_ENV")
    fi
  fi

  if [[ "$DRY_RUN" == 1 ]]; then
    printf '+' ; printf ' %q' "${cmd[@]}"; printf '\n'
    [[ "$stdin_key" == 1 ]] && printf '  (API Key 通过标准输入传递)\n'
    return 0
  fi

  log "使用 Docker 模式部署（scripts/docker-launch.sh deploy）"
  if [[ "$stdin_key" == 1 ]]; then
    printf '%s\n' "$NEW_API_KEY" | "${cmd[@]}"
  else
    "${cmd[@]}"
  fi

  [[ "$STAGE_ONLY" == 1 ]] && { log "配置已生成（--stage-only，未启动容器）"; return 0; }
  if [[ "$WAIT_SECONDS" -le 0 ]]; then
    log "容器已启动；--wait 0 跳过了就绪检查"
    return 0
  fi
  # 容器模式没有 systemctl，就绪检查走 launcher 的 node-ready
  local waited=0
  while [[ "$waited" -lt "$WAIT_SECONDS" ]]; do
    if "$SCRIPT_DIR/docker-launch.sh" node-ready >/dev/null 2>&1; then
      log "容器已就绪"
      "$SCRIPT_DIR/docker-launch.sh" node-status || true
      return 0
    fi
    sleep 2
    waited=$((waited + 2))
  done
  warn "容器已启动，但在 ${WAIT_SECONDS}s 内未就绪；查看日志: $SCRIPT_DIR/docker-launch.sh logs -f"
  return 1
}

if [[ "$MODE" == "docker" ]]; then
  have_docker || die "未找到 docker 命令，无法使用 docker 模式"
  docker_deploy
  exit $?
fi

# ---- 本地二进制 -------------------------------------------------------------
GO_BIN="$(command -v go || true)"
[[ -z "$GO_BIN" && -x /usr/local/go/bin/go ]] && GO_BIN=/usr/local/go/bin/go

build_binary() {
  local bin="$LOCAL_BIN"
  if [[ "$REBUILD" == 0 && -x "$bin" ]]; then
    return 0
  fi
  [[ -n "$GO_BIN" ]] || die "找不到 go 工具链，无法构建。请安装 Go，或手动放置 ./linkc-node"
  log "构建二进制: make build"
  ( cd "$REPO_ROOT" && GO="$GO_BIN" PATH="$(dirname "$GO_BIN"):$PATH" make build )
  [[ -x "$bin" ]] || die "构建失败：$bin 不存在"
}

# ---- 前台模式 ---------------------------------------------------------------
if [[ "$MODE" == "foreground" ]]; then
  FG_CONF="$CONF"
  if [[ "$DRY_RUN" == 1 ]]; then
    echo "# ---- $FG_CONF ----"
    if [[ -f "$CONF" && "$FORCE" == 0 ]]; then
      sed 's/^/# /' "$CONF"
      FG_PREVIEW_CONF="$FG_CONF"
    else
      (
        API_KEY_PATH="<temporary-directory>/api-key"
        WHITELIST_PATH="<temporary-directory>/whitelist.txt"
        STATE_DIR="<temporary-directory>/state"
        RUN_DIR="<temporary-directory>/run"
        SOCKET_PATH="<temporary-directory>/run/admin.sock"
        render_config
      )
      FG_PREVIEW_CONF="<temporary-config>"
    fi
    echo
    echo "# ---- 将执行的命令 ----"
    if [[ "$REBUILD" == 1 || ! -x "$LOCAL_BIN" ]]; then
      printf '+ cd %q && make build\n' "$REPO_ROOT"
    fi
    printf '+'; printf ' %q' "$LOCAL_BIN" run -c "$FG_PREVIEW_CONF"; printf '\n'
    exit 0
  fi
  build_binary
  if [[ ! -f "$CONF" || "$FORCE" == 1 ]]; then
    [[ -n "$NEW_API_KEY" ]] || die "生成临时配置需要 API Key（--api-key-*）"
    FG_DIR="$(mktemp -d)"
    trap 'rm -rf "$FG_DIR"' EXIT
    FG_CONF="$FG_DIR/config.yaml"
    # 全部落到临时目录，避免影响系统路径与非 root 权限
    API_KEY_PATH="$FG_DIR/api-key"
    WHITELIST_PATH="$FG_DIR/whitelist.txt"
    STATE_DIR="$FG_DIR/state"
    RUN_DIR="$FG_DIR/run"
    SOCKET_PATH="$RUN_DIR/admin.sock"
    mkdir -p "$STATE_DIR" "$RUN_DIR"
    printf '%s\n' "$NEW_API_KEY" > "$API_KEY_PATH"
    : > "$WHITELIST_PATH"
    chmod 0600 "$API_KEY_PATH" "$WHITELIST_PATH"
    render_config > "$FG_CONF"
    chmod 0600 "$FG_CONF"
    log "使用临时配置: $FG_CONF"
  fi
  log "前台运行: $FG_CONF（Ctrl+C 退出）"
  exec "$LOCAL_BIN" run -c "$FG_CONF"
fi

# ---- systemd 模式 -----------------------------------------------------------
if [[ "$DRY_RUN" == 1 ]]; then
  echo "# ---- $CONF ----"
  if [[ -f "$CONF" && "$FORCE" == 0 ]]; then
    echo "# 已存在，--dry-run 不覆盖；当前内容："
    sed 's/^/# /' "$CONF"
  else
    render_config
  fi
  echo
  echo "# ---- 将执行的步骤 ----"
  echo "1. 构建（若 ./linkc-node 不存在）: make build"
  echo "2. 安装二进制与 systemd unit: $BIN_DIR/linkc-node, $UNIT_DIR/$UNIT_NAME.service"
  echo "3. 写入配置: $CONF (0640 root:linkc), API Key: $API_KEY_PATH"
  echo "4. 本地校验: $BIN_DIR/linkc-node check -c $CONF"
  if [[ "$STAGE_ONLY" == 1 ]]; then
    echo "5. --stage-only：到此为止，不启动服务"
  else
    echo "5. 启用并重启: systemctl enable $UNIT_NAME.service && systemctl restart $UNIT_NAME.service"
    echo "6. 等待 ready: $SOCKET_PATH（最多 ${WAIT_SECONDS}s）"
  fi
  exit 0
fi

[[ "$(id -u)" -ne 0 ]] && die "请使用 root 或 sudo 运行"

if [[ "$BIN_DIR" != "/usr/local/bin" || "$CONF_DIR" != "/etc/linkc-node" || "$RUN_DIR" != "/run/linkc-node" ]]; then
  warn "检测到非默认路径（LINKC_* 环境变量）；deploy/linkc-node.service 中的路径写死为默认值，仅测试环境使用"
fi

build_binary

# 安装用户、目录、二进制与 unit（复用 install.sh，跳过它的示例配置）
log "安装二进制、用户与 systemd unit"
# install.sh 只负责落盘；配置校验通过后由 quickstart 统一执行 systemd 操作。
LINKC_BIN_DIR="$BIN_DIR" \
LINKC_CONF_DIR="$CONF_DIR" \
LINKC_STATE_DIR="$STATE_DIR" \
LINKC_RUN_DIR="$RUN_DIR" \
LINKC_UNIT_DIR="$UNIT_DIR" \
LINKC_SKIP_SYSTEMCTL=1 \
LINKC_SKIP_EXAMPLE_CONFIG=1 \
  sh "$SCRIPT_DIR/install.sh" "$CONF"

# install.sh has now made the config directory root-owned; re-check before any
# privileged chown/chmod so a symlink raced in by the service user is rejected.
ensure_managed_file_slot "$CONF" "config file" || exit 1
ensure_managed_file_slot "$API_KEY_PATH" "API Key file" || exit 1
ensure_managed_file_slot "$WHITELIST_PATH" "whitelist file" || exit 1

# 配置文件
if [[ -s "$CONF" && "$FORCE" == 0 ]]; then
  log "保留已有配置: $CONF（如需重写加 --force）"
else
  if [[ -f "$CONF" ]]; then
    backup="$(mktemp "$CONF.bak.$(date +%Y%m%d%H%M%S).XXXXXX")"
    cp -- "$CONF" "$backup"
    chown root:root "$backup"
    chmod 0600 "$backup"
    log "已备份旧配置: $backup"
    # 只保留最近 5 份，避免反复 --force 把配置目录撑满
    while IFS= read -r -d '' entry; do
      rm -f -- "${entry#* }"
    done < <(
      find "$CONF_DIR" -maxdepth 1 -type f -name 'config.yaml.bak.*' -printf '%T@ %p\0' |
        sort -z -rn |
        tail -z -n +6
    )
  fi
  log "写入配置: $CONF"
  render_config | write_managed_file "$CONF" 0640 root:linkc
fi

if [[ -n "$NEW_API_KEY" ]]; then
  log "写入 API Key: $API_KEY_PATH"
  printf '%s\n' "$NEW_API_KEY" | write_managed_file "$API_KEY_PATH" 0600 linkc:linkc
fi

if [[ ! -f "$WHITELIST_PATH" ]]; then
  write_managed_file "$WHITELIST_PATH" 0600 linkc:linkc </dev/null
fi

chown root:linkc "$CONF_DIR" "$CONF"
chmod 0750 "$CONF_DIR"
chmod 0640 "$CONF"
chown linkc:linkc "$API_KEY_PATH" "$WHITELIST_PATH" "$STATE_DIR"
chmod 0600 "$API_KEY_PATH" "$WHITELIST_PATH"
chmod 0700 "$STATE_DIR"

mkdir -p "$RUN_DIR"
chown linkc:linkc "$RUN_DIR"
chmod 0700 "$RUN_DIR"

# 证书可读性（linkc 用户需要能读）
for f in "$CERT_FILE" "$KEY_FILE"; do
  [[ -n "$f" ]] || continue
  [[ -r "$f" ]] || die "证书文件不可读: $f"
  if ! sudo -u linkc test -r "$f" 2>/dev/null; then
    warn "linkc 用户可能无法读取 $f；请确认路径权限（避免放在 /home 下）"
  fi
done

# 本地校验（不访问面板）
log "校验配置: $BIN_DIR/linkc-node check -c $CONF"
"$BIN_DIR/linkc-node" check -c "$CONF" || die "配置校验失败，请修正后再执行"

if [[ "$STAGE_ONLY" == 1 ]]; then
  log "配置已就绪（--stage-only，未启动服务）"
  printf '启动: %s\n' "systemctl enable $UNIT_NAME.service && systemctl restart $UNIT_NAME.service"
  exit 0
fi

# 启动服务
if [[ "$SKIP_SYSTEMCTL" != "1" ]]; then
  log "启用并重启服务: systemctl enable $UNIT_NAME.service && systemctl restart $UNIT_NAME.service"
  enable_and_restart_service "$UNIT_NAME.service"
else
  warn "LINKC_SKIP_SYSTEMCTL=1，跳过 systemd 启动"
fi

# 等待 ready
wait_ready() {
  local deadline=$(( $(date +%s) + WAIT_SECONDS ))
  while [[ "$(date +%s)" -lt "$deadline" ]]; do
    if "$BIN_DIR/linkc-node" ready -c "$CONF" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

STARTED=1
if [[ "$SKIP_SYSTEMCTL" == "1" ]]; then
  STARTED=0
fi

READY=1
SKIPPED_WAIT=0
if [[ "$STARTED" == 1 ]]; then
  if [[ "$WAIT_SECONDS" -gt 0 ]]; then
    log "等待节点就绪（最多 ${WAIT_SECONDS}s）"
    wait_ready || READY=0
  else
    SKIPPED_WAIT=1
    READY=0
  fi
fi

echo
if [[ "$STARTED" == 0 ]]; then
  echo "ℹ️  配置与二进制已就绪，但未启动服务（LINKC_SKIP_SYSTEMCTL=1）"
  printf '   手动启动: systemctl enable %s.service && systemctl restart %s.service\n' "$UNIT_NAME" "$UNIT_NAME"
elif [[ "$SKIPPED_WAIT" == 1 ]]; then
  echo "ℹ️  linkc-node 已启动；--wait 0 跳过了就绪检查"
  echo "   手动确认: $BIN_DIR/linkc-node ready -c $CONF"
elif [[ "$READY" == 1 ]]; then
  echo "✅ linkc-node 已启动并就绪"
else
  echo "⚠️  linkc-node 已启动，但在 ${WAIT_SECONDS}s 内未就绪"
  echo "   常见原因: 面板不可达 / API Key 或 node_id 错误 / 面板下发端口被占用 / 证书不可读"
  echo "   查看日志: journalctl -u $UNIT_NAME -n 50 --no-pager"
  if command -v journalctl >/dev/null 2>&1; then
    echo
    echo "最近日志:"
    journalctl -u "$UNIT_NAME" -n 6 --no-pager 2>/dev/null | sed 's/^/  /' || true
  fi
fi

if [[ "$STARTED" == 1 ]]; then
  "$BIN_DIR/linkc-node" status -c "$CONF" 2>/dev/null || true
fi

cat <<EOF

配置:        $CONF
API Key:     $API_KEY_PATH（0600）
状态目录:    $STATE_DIR
管理 socket: $SOCKET_PATH

常用命令:
  sudo systemctl status $UNIT_NAME
  sudo journalctl -u $UNIT_NAME -f
  sudo $BIN_DIR/linkc-node status -c $CONF
  curl --unix-socket $SOCKET_PATH http://localhost/status
  sudo systemctl reload $UNIT_NAME   # 仅重载证书；改配置用 restart

下一步: 在面板确认节点在线、用户数正确，再按 docs/usage.md 调整审计与限速。
EOF

[[ "$STARTED" == 1 && "$READY" == 0 && "$SKIPPED_WAIT" == 0 ]] && exit 1
exit 0
