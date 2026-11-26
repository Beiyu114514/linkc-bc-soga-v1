#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf -- "$TMP_ROOT"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local needle=$1 haystack=$2
  [[ "$haystack" == *"$needle"* ]] || fail "expected output to contain: $needle"
}

test_install_path_guards() {
  local output
  if output=$(bash -c '. "$1"; normalize_install_path TEST /etc /tmp/default' _ "$ROOT/scripts/install_paths.sh" 2>&1); then
    fail "system directory override was accepted"
  fi
  assert_contains "system-wide directory" "$output"

  if output=$(bash -c '. "$1"; normalize_install_path LINKC_CONF_DIR /etc/ssl /tmp/default' _ "$ROOT/scripts/install_paths.sh" 2>&1); then
    fail "existing system service directory was accepted"
  fi
  assert_contains "dedicated linkc-node directory" "$output"

  if output=$(bash -c '. "$1"; normalize_install_path LINKC_CONF_DIR /tmp/not-dedicated /tmp/default' _ "$ROOT/scripts/install_paths.sh" 2>&1); then
    fail "non-dedicated override directory was accepted"
  fi
  assert_contains "dedicated linkc-node directory" "$output"

  output=$(bash -c '. "$1"; normalize_install_path LINKC_CONF_DIR /etc/linkc-node-test /tmp/default' _ \
    "$ROOT/scripts/install_paths.sh")
  [[ "$output" == "/etc/linkc-node-test" ]] || fail "dedicated system-tree directory was not normalized"

  if output=$(bash -c '. "$1"; normalize_install_path TEST /tmp/../etc /tmp/default' _ "$ROOT/scripts/install_paths.sh" 2>&1); then
    fail "path traversal override was accepted"
  fi
  assert_contains "must not contain . or .." "$output"

  ln -s /etc "$TMP_ROOT/etc-link"
  if output=$(bash -c '. "$1"; normalize_install_path TEST "$2" /tmp/default' _ \
    "$ROOT/scripts/install_paths.sh" "$TMP_ROOT/etc-link" 2>&1); then
    fail "symlink to a system directory was accepted"
  fi
  assert_contains "system-wide directory" "$output"

  if output=$(bash -c '. "$1"; ensure_disjoint_install_paths /tmp/linkc /tmp/linkc/state' _ \
    "$ROOT/scripts/install_paths.sh" 2>&1); then
    fail "overlapping install directories were accepted"
  fi
  assert_contains "must not overlap" "$output"
}

test_managed_files_reject_symlinks() {
  local dir="$TMP_ROOT/config" target="$TMP_ROOT/root-owned" link="$TMP_ROOT/config/api-key"
  mkdir -p "$dir"
  printf 'do not change\n' >"$target"
  ln -s "$target" "$link"
  if output=$(bash -c '. "$1"; write_managed_file "$2" 0600 0:0' _ \
    "$ROOT/scripts/install_paths.sh" "$link" </dev/null 2>&1); then
    fail "symlinked managed file was overwritten"
  fi
  assert_contains "must not be a symbolic link" "$output"
  [[ "$(cat "$target")" == "do not change" ]] || fail "symlink target was modified"

  printf 'new contents\n' | bash -c '. "$1"; chown() { :; }; write_managed_file "$2" 0640 0:0' _ \
    "$ROOT/scripts/install_paths.sh" "$TMP_ROOT/config/config.yaml"
  [[ "$(cat "$TMP_ROOT/config/config.yaml")" == "new contents" ]] || fail "managed file was not replaced"
  [[ "$(stat -c '%a' "$TMP_ROOT/config/config.yaml")" == "640" ]] || fail "managed file mode was not applied"
}

test_quickstart_rejects_unsafe_paths() {
  local output conf_dir="$TMP_ROOT/quickstart/linkc-node/config"
  if output=$(LINKC_CONF_DIR=/etc \
    "$ROOT/scripts/quickstart.sh" --dry-run --mode foreground \
      --panel-url https://panel.example.com --node-id 1 --api-key test-key 2>&1); then
    fail "quickstart accepted LINKC_CONF_DIR=/etc"
  fi
  assert_contains "system-wide directory" "$output"

  mkdir -p "$conf_dir"
  printf 'do not change\n' >"$TMP_ROOT/quickstart-target"
  ln -s "$TMP_ROOT/quickstart-target" "$conf_dir/config.yaml"
  if output=$(LINKC_BIN_DIR="$TMP_ROOT/quickstart/linkc-node/bin" \
    LINKC_CONF_DIR="$conf_dir" \
    LINKC_STATE_DIR="$TMP_ROOT/quickstart/linkc-node/state" \
    LINKC_RUN_DIR="$TMP_ROOT/quickstart/linkc-node/run" \
    LINKC_UNIT_DIR="$TMP_ROOT/quickstart/linkc-node/unit" \
    "$ROOT/scripts/quickstart.sh" --dry-run --mode foreground \
      --panel-url https://panel.example.com --node-id 1 --api-key test-key 2>&1); then
    fail "quickstart accepted a symlinked config file"
  fi
  assert_contains "must not be a symbolic link" "$output"
  [[ "$(cat "$TMP_ROOT/quickstart-target")" == "do not change" ]] || fail "quickstart changed symlink target"
}

test_installer_validates_before_side_effects() {
  local fakebin="$TMP_ROOT/install-bin" marker="$TMP_ROOT/useradd-called"
  mkdir -p "$fakebin"
  cat >"$fakebin/id" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "-u" ]; then
  echo 0
  exit 0
fi
exit 0
EOF
  cat >"$fakebin/useradd" <<EOF
#!/bin/sh
: >"$marker"
EOF
  chmod +x "$fakebin/id" "$fakebin/useradd"

  if PATH="$fakebin:/usr/bin:/bin" \
    LINKC_BIN_DIR="$TMP_ROOT/install/linkc-node/bin" \
    LINKC_CONF_DIR=/etc \
    LINKC_STATE_DIR="$TMP_ROOT/install/linkc-node/state" \
    LINKC_RUN_DIR="$TMP_ROOT/install/linkc-node/run" \
    LINKC_UNIT_DIR="$TMP_ROOT/install/linkc-node/unit" \
    LINKC_SKIP_SYSTEMCTL=1 \
    sh "$ROOT/scripts/install.sh" >/dev/null 2>&1; then
    fail "installer accepted LINKC_CONF_DIR=/etc"
  fi
  [[ ! -e "$marker" ]] || fail "installer created the service user before validating paths"
}

test_sudo_preserves_secret_by_name() {
  local fakebin="$TMP_ROOT/sudo-bin" args="$TMP_ROOT/sudo-args"
  mkdir -p "$fakebin"
  cat >"$fakebin/id" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "-u" ]; then
  echo 1000
  exit 0
fi
exit 0
EOF
  cat >"$fakebin/sudo" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"$SUDO_ARGS_FILE"
exit 0
EOF
  chmod +x "$fakebin/id" "$fakebin/sudo"

  PATH="$fakebin:/usr/bin:/bin" \
  SUDO_ARGS_FILE="$args" \
  PANEL_API_KEY='top-secret-value' \
  LINKC_BIN_DIR="$TMP_ROOT/sudo/linkc-node/bin" \
  LINKC_CONF_DIR="$TMP_ROOT/sudo/linkc-node/etc" \
  LINKC_STATE_DIR="$TMP_ROOT/sudo/linkc-node/state" \
  LINKC_RUN_DIR="$TMP_ROOT/sudo/linkc-node/run" \
  LINKC_UNIT_DIR="$TMP_ROOT/sudo/linkc-node/unit" \
    "$ROOT/scripts/quickstart.sh" --mode systemd \
      --panel-url https://panel.example.com --node-id 1 --api-key-env PANEL_API_KEY

  if grep -Fq 'top-secret-value' "$args"; then
    fail "API key leaked into sudo argv"
  fi
  assert_contains "PANEL_API_KEY" "$(cat "$args")"
  assert_contains "--preserve-env=" "$(cat "$args")"
}

run_wizard_error_case() {
  local expected=$1 profile=$2 listen=$3 audit=$4 limits=$5 mode=$6 output
  if output=$(printf '%s\n' "$profile" "$listen" "$audit" "$limits" "$mode" "" | \
    "$ROOT/scripts/quickstart.sh" wizard --dry-run \
      --panel-url https://panel.example.com --node-id 1 --api-key test-key 2>&1); then
    fail "wizard accepted invalid values"
  fi
  assert_contains "$expected" "$output"
}

test_wizard_revalidates_choices() {
  run_wizard_error_case "审计动作" sspanel-uim-soga 0.0.0.0 invalid enforce systemd
  run_wizard_error_case "限速模式" sspanel-uim-soga 0.0.0.0 report invalid systemd
  run_wizard_error_case "启动模式" sspanel-uim-soga 0.0.0.0 report enforce dockr
}

test_foreground_dry_run_does_not_build() {
  local fakebin="$TMP_ROOT/build-bin" marker="$TMP_ROOT/make-called"
  mkdir -p "$fakebin"
  cat >"$fakebin/make" <<EOF
#!/bin/sh
: >"$marker"
EOF
  cat >"$fakebin/go" <<'EOF'
#!/bin/sh
exit 0
EOF
  chmod +x "$fakebin/make" "$fakebin/go"

  PATH="$fakebin:/usr/bin:/bin" \
  LINKC_BIN_DIR="$TMP_ROOT/dry/linkc-node/bin" \
  LINKC_CONF_DIR="$TMP_ROOT/dry/linkc-node/etc" \
  LINKC_STATE_DIR="$TMP_ROOT/dry/linkc-node/state" \
  LINKC_RUN_DIR="$TMP_ROOT/dry/linkc-node/run" \
  LINKC_UNIT_DIR="$TMP_ROOT/dry/linkc-node/unit" \
    "$ROOT/scripts/quickstart.sh" --mode foreground --dry-run --build --wait 0 \
      --panel-url https://panel.example.com --node-id 1 --api-key test-key >/dev/null

  [[ ! -e "$marker" ]] || fail "foreground dry-run invoked make"
}

test_wait_validation() {
  local output
  if output=$("$ROOT/scripts/quickstart.sh" --dry-run --mode foreground --wait invalid \
    --panel-url https://panel.example.com --node-id 1 --api-key test-key 2>&1); then
    fail "invalid --wait value was accepted"
  fi
  assert_contains "非负整数" "$output"

  "$ROOT/scripts/quickstart.sh" --dry-run --mode foreground --wait 08 \
    --panel-url https://panel.example.com --node-id 1 --api-key test-key >/dev/null
}

test_service_restart() {
  local output
  output=$(bash -c '
    . "$1"
    systemctl() { printf "systemctl %s\\n" "$*"; }
    enable_and_restart_service linkc-node.service
  ' _ "$ROOT/scripts/install_paths.sh")
  [[ "$output" == $'systemctl daemon-reload\nsystemctl enable linkc-node.service\nsystemctl restart linkc-node.service' ]] ||
    fail "service restart sequence was incorrect: $output"
}

test_install_path_guards
test_managed_files_reject_symlinks
test_quickstart_rejects_unsafe_paths
test_installer_validates_before_side_effects
test_sudo_preserves_secret_by_name
test_wizard_revalidates_choices
test_foreground_dry_run_does_not_build
test_wait_validation
test_service_restart

printf 'quickstart tests passed\n'
