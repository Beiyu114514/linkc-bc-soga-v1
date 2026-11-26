#!/usr/bin/env python3
"""
CLI tool for deploying and managing linkc-node in Docker.

The launcher writes a complete config.yaml, an api-key file (0600), optional
TLS certificates, and a reproducible docker-run.sh, then deploys or manages
the container with host networking.  It only uses the Python standard library.
"""

from __future__ import annotations

import argparse
import datetime as dt
import getpass
import ipaddress
import json
import os
import re
import secrets
import shlex
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import traceback
import urllib.parse
from pathlib import Path
from typing import Any

LAUNCHER_VERSION = "2.0.0"

CONFIG_FILENAME = "config.yaml"
API_KEY_FILENAME = "api-key"
METADATA_FILENAME = "launcher.json"
RUN_SCRIPT_FILENAME = "docker-run.sh"
CERTS_DIRNAME = "certs"
WHITELIST_FILENAME = "whitelist.txt"
BACKUPS_DIRNAME = "backups"

MANAGED_LABEL = "com.linkc.managed-by"
MANAGED_VALUE = "linkc-docker-launcher"

CONTAINER_CONFIG_PATH = "/etc/linkc-node"
CONTAINER_DATA_PATH = "/var/lib/linkc-node"
CONTAINER_RUN_PATH = "/run/linkc-node"
CONTAINER_SOCKET_PATH = "/run/linkc-node/admin.sock"
CONTAINER_GEODATA_PATH = "/usr/share/xray"

DEFAULT_IMAGE = "ghcr.io/beiyu114514/linkc-bc-soga-v1:latest"
DEFAULT_CONTAINER = "linkc-node"
DEFAULT_PROFILE = "sspanel-uim-soga"

_DURATION_RE = re.compile(r"^(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+$")
_DURATION_PART_RE = re.compile(r"(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)")
_IMAGE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/:@+-]{0,254}$")
_CONTAINER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
_ANSI_RE = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")

_BLOCKED_DIRS = {
    "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib64",
    "/media", "/mnt", "/opt", "/proc", "/root", "/run", "/sbin", "/srv",
    "/sys", "/tmp", "/usr", "/var",
    # Common parents of unrelated system state: taking them over would chown
    # (and chmod 0700) far more than this launcher owns.
    "/etc/docker", "/etc/nginx", "/etc/ssh", "/etc/ssl", "/etc/systemd",
    "/usr/bin", "/usr/lib", "/usr/lib64", "/usr/local", "/usr/sbin",
    "/usr/share", "/var/cache", "/var/lib", "/var/log", "/var/tmp",
}

# A directory may only be chmod/chown-ed when it is empty or contains nothing
# but files this launcher (or the node itself) manages.  Anything else is
# refused instead of being taken over recursively.
CONFIG_DIR_ALLOWED = {
    CONFIG_FILENAME,
    API_KEY_FILENAME,
    METADATA_FILENAME,
    RUN_SCRIPT_FILENAME,
    WHITELIST_FILENAME,
    CERTS_DIRNAME,
    BACKUPS_DIRNAME,
    "env",
}
DATA_DIR_ALLOWED = {"state.db", "state.db.bak", "state.db.lock", "run"}
CERTS_DIR_ALLOWED = {"fullchain.pem", "privkey.pem"}
RUN_DIR_ALLOWED = {"admin.sock"}

METADATA_KEYS = [
    "base_url", "node_id", "profile", "allow_http", "panel_tls_verify",
    "panel_timeout", "max_attempts", "max_response_mb", "panel_proxy",
    "listen", "audit_enabled", "audit_action", "limits_mode",
    "alive_ip_mode", "include_online_zero", "allow_private_targets",
    "whitelist", "geodata_dir", "image", "container_name", "config_dir",
    "data_dir", "restart_policy", "pull_latest", "build_local", "run_check",
    "no_new_privileges", "selinux_relabel", "log_level", "log_format",
    "pull_interval", "push_interval", "collect_interval", "status_interval",
    "max_journal_mb", "metrics", "pprof", "sniffing_mode",
    "mass_remove_guard",
]


class ValidationError(ValueError):
    """A user-facing validation error."""


class DeploymentError(RuntimeError):
    """A failed docker operation, carrying the steps completed so far."""

    def __init__(self, message: str, steps: list[dict[str, Any]] | None = None):
        super().__init__(message)
        self.steps = steps or []


# ---------------------------------------------------------------------------
# small helpers
# ---------------------------------------------------------------------------


def utcnow() -> str:
    return (
        dt.datetime.now(dt.timezone.utc)
        .replace(microsecond=0)
        .isoformat()
        .replace("+00:00", "Z")
    )


def local_timestamp() -> str:
    # Milliseconds keep two saves in the same second from sharing a backup dir.
    return dt.datetime.now().strftime("%Y%m%d-%H%M%S-%f")[:-3]


def default_paths() -> tuple[str, str]:
    if os.geteuid() == 0:
        return CONTAINER_CONFIG_PATH, CONTAINER_DATA_PATH
    base = Path.home() / ".linkc-node"
    return str(base / "etc"), str(base / "data")


def default_values() -> dict[str, Any]:
    config_dir, data_dir = default_paths()
    return {
        "base_url": "",
        "api_key": "",
        "node_id": "",
        "profile": DEFAULT_PROFILE,
        "allow_http": False,
        "panel_tls_verify": True,
        "panel_timeout": "15s",
        "max_attempts": 3,
        "max_response_mb": 32,
        "panel_proxy": "",
        "listen": "0.0.0.0",
        "cert_pem": "",
        "key_pem": "",
        "clear_certs": False,
        "audit_enabled": True,
        "audit_action": "report",
        "limits_mode": "enforce",
        "alive_ip_mode": "changed",
        "include_online_zero": False,
        "allow_private_targets": False,
        "whitelist": "",
        "geodata_dir": "",
        "image": DEFAULT_IMAGE,
        "container_name": DEFAULT_CONTAINER,
        "config_dir": config_dir,
        "data_dir": data_dir,
        "restart_policy": "unless-stopped",
        "pull_latest": True,
        "build_local": False,
        "run_check": True,
        "no_new_privileges": False,
        "selinux_relabel": False,
        "log_level": "info",
        "log_format": "json",
        "pull_interval": "60s",
        "push_interval": "60s",
        "collect_interval": "30s",
        "status_interval": "5s",
        "max_journal_mb": 64,
        "metrics": True,
        "pprof": False,
        "sniffing_mode": "auto",
        "mass_remove_guard": "warn",
    }


def _as_str(value: Any, key: str) -> str:
    if value is None:
        return ""
    if not isinstance(value, str):
        raise ValidationError(f"{key} 必须是字符串")
    if "\x00" in value:
        raise ValidationError(f"{key} 包含非法字符")
    return value.strip()


def _as_bool(value: Any, key: str) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        return value.strip().lower() in ("1", "true", "yes", "on")
    if isinstance(value, (int, float)):
        return bool(value)
    raise ValidationError(f"{key} 必须是布尔值")


def _as_int(value: Any, key: str, minimum: int, maximum: int) -> int:
    try:
        number = int(value)
    except (TypeError, ValueError):
        raise ValidationError(f"{key} 必须是整数")
    if number < minimum or number > maximum:
        raise ValidationError(f"{key} 必须在 {minimum}..{maximum} 之间")
    return number


def _as_duration(value: Any, key: str) -> str:
    text = _as_str(value, key)
    if not _DURATION_RE.match(text):
        raise ValidationError(f"{key} 不是合法时长（例如 60s、5m、1m30s）")
    factors = {
        "ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3,
        "s": 1.0, "m": 60.0, "h": 3600.0,
    }
    seconds = sum(
        float(number) * factors[unit]
        for number, unit in _DURATION_PART_RE.findall(text)
    )
    if seconds <= 0 or seconds > 86400:
        raise ValidationError(f"{key} 必须在 (0, 24h] 之间")
    return text


def _yaml_safe(text: str, key: str) -> str:
    if "${" in text:
        raise ValidationError(f"{key} 不能包含 ${{...}}（配置加载器会展开环境变量）")
    if any(ord(char) < 0x20 for char in text):
        raise ValidationError(f"{key} 包含控制字符")
    return text


def _as_abs_path(value: Any, key: str) -> str:
    text = _as_str(value, key)
    if not text:
        raise ValidationError(f"{key} 不能为空")
    if not os.path.isabs(text):
        raise ValidationError(f"{key} 必须是绝对路径")
    if any(ord(char) < 0x20 for char in text):
        raise ValidationError(f"{key} 包含控制字符")
    if ".." in Path(text).parts:
        raise ValidationError(f"{key} 不能包含 ..")
    if ":" in text:
        raise ValidationError(f"{key} 不能包含冒号 (:)")
    normalized = os.path.normpath(text)
    if normalized in _BLOCKED_DIRS:
        raise ValidationError(f"{key} 不能是系统目录 {normalized}")
    if len(Path(normalized).parts) < 2:
        raise ValidationError(f"{key} 路径过浅")
    real = os.path.realpath(normalized)
    if real in _BLOCKED_DIRS:
        raise ValidationError(f"{key} 解析后指向系统目录 {real}")
    return normalized


def _validate_base_url(raw: Any, allow_http: bool) -> str:
    text = _as_str(raw, "panel.base_url").rstrip("/")
    if not text:
        raise ValidationError("面板地址（base_url）不能为空")
    if any(char.isspace() for char in text):
        raise ValidationError("面板地址不能包含空白字符")
    _yaml_safe(text, "panel.base_url")
    try:
        parsed = urllib.parse.urlsplit(text)
        _ = parsed.port  # raises ValueError for an invalid port
        hostname = parsed.hostname
    except ValueError as exc:
        raise ValidationError(f"面板地址不合法: {exc}")
    if parsed.scheme not in ("http", "https") or not parsed.netloc or not hostname:
        raise ValidationError("面板地址必须是 http(s)://host[/path]")
    if parsed.username or parsed.password:
        raise ValidationError("面板地址不能包含用户名/密码")
    if parsed.query or parsed.fragment:
        raise ValidationError("面板地址不能包含查询参数或片段")
    if parsed.scheme == "http" and not allow_http:
        raise ValidationError("面板地址是 http；请勾选“允许 HTTP（仅测试）”或改用 https")
    return text


def _validate_pem(raw: Any, kind: str) -> str:
    if raw is None:
        return ""
    if not isinstance(raw, str):
        raise ValidationError(f"{kind} 必须是 PEM 文本")
    text = raw.replace("\r\n", "\n").strip()
    if not text:
        return ""
    if len(text) > 2_000_000:
        raise ValidationError(f"{kind} 文件过大")
    if kind == "证书":
        if "-----BEGIN CERTIFICATE-----" not in text or "-----END CERTIFICATE-----" not in text:
            raise ValidationError("证书不是 PEM 格式（缺少 CERTIFICATE 块）")
    else:
        if "-----BEGIN" not in text or "PRIVATE KEY-----" not in text:
            raise ValidationError("私钥不是 PEM 格式（缺少 PRIVATE KEY 块）")
    return text + "\n"


def normalize_values(raw: Any) -> dict[str, Any]:
    if not isinstance(raw, dict):
        raise ValidationError("参数必须是 JSON 对象")

    values = default_values()
    for key in values:
        if key in raw:
            values[key] = raw[key]

    string_keys = [
        "base_url", "node_id", "profile", "listen", "panel_proxy", "image",
        "container_name", "config_dir", "data_dir", "restart_policy",
        "log_level", "log_format", "pull_interval", "push_interval",
        "collect_interval", "status_interval", "sniffing_mode",
        "mass_remove_guard", "audit_action", "limits_mode", "alive_ip_mode",
        "geodata_dir",
    ]
    for key in string_keys:
        values[key] = _as_str(values.get(key), key)

    bool_keys = [
        "allow_http", "panel_tls_verify", "clear_certs", "audit_enabled",
        "include_online_zero", "allow_private_targets", "pull_latest",
        "build_local", "run_check", "no_new_privileges", "selinux_relabel",
        "metrics", "pprof",
    ]
    for key in bool_keys:
        values[key] = _as_bool(values.get(key), key)

    values["max_attempts"] = _as_int(values.get("max_attempts"), "max_attempts", 1, 10)
    values["max_response_mb"] = _as_int(values.get("max_response_mb"), "max_response_mb", 1, 1024)
    values["max_journal_mb"] = _as_int(values.get("max_journal_mb"), "max_journal_mb", 1, 10240)

    for key in ("panel_timeout", "pull_interval", "push_interval", "collect_interval", "status_interval"):
        values[key] = _as_duration(values.get(key), key)

    api_key = values.get("api_key", "")
    if not isinstance(api_key, str):
        raise ValidationError("api_key 必须是字符串")
    api_key = api_key.strip()
    if "\n" in api_key or "\r" in api_key or "\x00" in api_key:
        raise ValidationError("api_key 不能包含换行或 NUL")
    if len(api_key) > 4096:
        raise ValidationError("api_key 过长")
    values["api_key"] = api_key

    values["cert_pem"] = _validate_pem(values.get("cert_pem", ""), "证书")
    values["key_pem"] = _validate_pem(values.get("key_pem", ""), "私钥")
    if bool(values["cert_pem"]) != bool(values["key_pem"]):
        raise ValidationError("证书和私钥必须同时上传")
    if values["clear_certs"] and (values["cert_pem"] or values["key_pem"]):
        raise ValidationError("不能同时清除证书和上传新证书")

    whitelist = values.get("whitelist", "")
    if not isinstance(whitelist, str):
        raise ValidationError("whitelist 必须是字符串")
    if len(whitelist) > 2_000_000:
        raise ValidationError("whitelist 过大")
    if "\x00" in whitelist:
        raise ValidationError("whitelist 不能包含 NUL")
    values["whitelist"] = whitelist.replace("\r\n", "\n").strip()

    values["base_url"] = _validate_base_url(values["base_url"], values["allow_http"])

    if not values["node_id"]:
        raise ValidationError("节点 ID（node_id）不能为空")
    _yaml_safe(values["node_id"], "node_id")
    if len(values["node_id"]) > 128:
        raise ValidationError("节点 ID 过长")
    if any(char.isspace() for char in values["node_id"]):
        raise ValidationError("节点 ID 不能包含空白字符")

    if not values["listen"]:
        values["listen"] = "0.0.0.0"
    if values["listen"] == "localhost":
        values["listen"] = "127.0.0.1"
    else:
        try:
            ipaddress.ip_address(values["listen"])
        except ValueError:
            raise ValidationError("node.listen 必须是 IP 地址或 localhost")

    if values["profile"] != DEFAULT_PROFILE:
        raise ValidationError(f"profile 只支持 {DEFAULT_PROFILE}")

    if not _IMAGE_RE.match(values["image"]) or ".." in values["image"]:
        raise ValidationError("镜像名称不合法")
    if values["build_local"] and values["image"] == DEFAULT_IMAGE:
        raise ValidationError(
            "本地构建时请把镜像名改成 linkc-node:local（不要覆盖默认 GHCR 镜像名）"
        )
    if not _CONTAINER_RE.match(values["container_name"]):
        raise ValidationError("容器名称不合法（字母数字开头，允许 _ . -）")
    if values["restart_policy"] not in ("no", "always", "unless-stopped", "on-failure"):
        raise ValidationError("restart_policy 不合法")
    if values["log_level"] not in ("debug", "info", "warn", "error"):
        raise ValidationError("log_level 不合法")
    if values["log_format"] not in ("json", "text"):
        raise ValidationError("log_format 不合法")
    if values["audit_action"] not in ("block", "report"):
        raise ValidationError("audit_action 不合法")
    if values["limits_mode"] not in ("enforce", "report", "off"):
        raise ValidationError("limits_mode 不合法")
    if values["alive_ip_mode"] not in ("changed", "full"):
        raise ValidationError("alive_ip_mode 不合法")
    if values["sniffing_mode"] not in ("auto", "off"):
        raise ValidationError("sniffing_mode 不合法")
    if values["mass_remove_guard"] not in ("warn", "reject"):
        raise ValidationError("mass_remove_guard 不合法")

    if values["panel_proxy"]:
        if any(char.isspace() for char in values["panel_proxy"]):
            raise ValidationError("panel.proxy 不能包含空白字符")
        _yaml_safe(values["panel_proxy"], "panel.proxy")
        parsed = urllib.parse.urlsplit(values["panel_proxy"])
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise ValidationError("panel.proxy 必须是 http(s)://host:port")

    values["config_dir"] = _as_abs_path(values["config_dir"], "config_dir")
    values["data_dir"] = _as_abs_path(values["data_dir"], "data_dir")
    if values["config_dir"] == values["data_dir"]:
        raise ValidationError("配置目录和数据目录不能相同")
    if (
        values["config_dir"].startswith(values["data_dir"] + os.sep)
        or values["data_dir"].startswith(values["config_dir"] + os.sep)
    ):
        raise ValidationError("配置目录和数据目录不能互相嵌套")

    if values["geodata_dir"]:
        values["geodata_dir"] = _as_abs_path(values["geodata_dir"], "geodata_dir")
        if not os.path.isdir(values["geodata_dir"]):
            raise ValidationError(f"geodata 目录不存在: {values['geodata_dir']}")

    return values


# ---------------------------------------------------------------------------
# config / command generation
# ---------------------------------------------------------------------------


def q(value: str) -> str:
    """JSON strings are valid YAML double-quoted scalars."""
    return json.dumps(value, ensure_ascii=False)


def b(value: bool) -> str:
    return "true" if value else "false"


def build_config(values: dict[str, Any], existing: dict[str, Any]) -> str:
    use_certs = bool(values["cert_pem"] and values["key_pem"]) or (
        existing["has_certs"] and not values["clear_certs"]
    )
    whitelist_file = (
        f"{CONTAINER_CONFIG_PATH}/{WHITELIST_FILENAME}"
        if values["whitelist"].strip()
        else ""
    )

    lines = [
        "# Generated by linkc-node Docker launcher.",
        "# The panel API key is stored in api-key (0600), not in this file.",
        "panel:",
        f"  base_url: {q(values['base_url'])}",
        f"  profile: {q(DEFAULT_PROFILE)}",
        f"  api_key_file: {q(CONTAINER_CONFIG_PATH + '/' + API_KEY_FILENAME)}",
        f"  node_id: {q(values['node_id'])}",
        f"  node_type: {q('vless')}",
        f"  timeout: {q(values['panel_timeout'])}",
        f"  allow_http: {b(values['allow_http'])}",
        "  follow_redirects: false",
        "  tls:",
        f"    verify: {b(values['panel_tls_verify'])}",
        '    ca_file: ""',
        '    server_name: ""',
        '    client_cert_file: ""',
        '    client_key_file: ""',
        f"  proxy: {q(values['panel_proxy'])}",
        f"  max_response_bytes: {values['max_response_mb'] * 1024 * 1024}",
        f"  max_attempts: {values['max_attempts']}",
        "",
        "node:",
        f"  listen: {q(values['listen'])}",
        f"  pull_interval: {q(values['pull_interval'])}",
        f"  push_interval: {q(values['push_interval'])}",
        f"  collect_interval: {q(values['collect_interval'])}",
        f"  status_interval: {q(values['status_interval'])}",
        "",
        "  tls:",
    ]
    if use_certs:
        lines += [
            "    certificates:",
            f"      - cert_file: {q(CONTAINER_CONFIG_PATH + '/' + CERTS_DIRNAME + '/fullchain.pem')}",
            f"        key_file: {q(CONTAINER_CONFIG_PATH + '/' + CERTS_DIRNAME + '/privkey.pem')}",
        ]
    else:
        lines.append("    certificates: []")
    lines += [
        "    alpn: []",
        '    min_version: ""',
        '    ech_server_keys: ""',
        "",
        "  reality:",
        "    xver: 0",
        '    mldsa65_seed: ""',
        "",
        "  vless:",
        '    decryption: "none"',
        "  xhttp:",
        '    host: ""',
        '    path: ""',
        '    mode: "auto"',
        "    headers: {}",
        "  grpc:",
        '    authority: ""',
        '    service_name: ""',
        "    multi_mode: false",
        "    idle_timeout: 0",
        "    health_check_timeout: 0",
        "    permit_without_stream: false",
        "    initial_windows_size: 0",
        '    user_agent: ""',
        "",
        "  sniffing:",
        f"    mode: {q(values['sniffing_mode'])}",
        "",
        "  limits:",
        f"    mode: {q(values['limits_mode'])}",
        "    device_limit_ipv6_prefix: 128",
        "    burst_bytes: 65536",
        "",
        "  freedom:",
        f"    allow_private_targets: {b(values['allow_private_targets'])}",
        "",
        "  users:",
        "    allow_empty: true",
        f"    mass_remove_guard: {q(values['mass_remove_guard'])}",
        "    max_users: 100000",
        "",
        "  traffic:",
        f"    include_online_zero: {b(values['include_online_zero'])}",
        "    batch_users: 500",
        "    batch_bytes: 1048576",
        "",
        "  alive_ip:",
        f"    mode: {q(values['alive_ip_mode'])}",
        "    full_every: 20",
        "",
        "  status:",
        '    mode: "load_fraction"',
        "",
        "  audit:",
        f"    enabled: {b(values['audit_enabled'])}",
        f"    action: {q(values['audit_action'])}",
        '    rule_mode: "auto"',
        f"    whitelist_file: {q(whitelist_file)}",
        '    dedupe_ttl: "24h"',
        "    strict_rules: false",
        "    resolve_domain_for_ip_rules: true",
        "",
        "  geodata:",
        f"    dir: {q(CONTAINER_GEODATA_PATH)}",
        "",
        "storage:",
        f"  path: {q(CONTAINER_DATA_PATH)}",
        f"  max_journal_bytes: {values['max_journal_mb'] * 1024 * 1024}",
        "",
        "logging:",
        f"  level: {q(values['log_level'])}",
        f"  format: {q(values['log_format'])}",
        "",
        "admin:",
        f"  socket: {q(CONTAINER_SOCKET_PATH)}",
        f"  metrics: {b(values['metrics'])}",
        f"  pprof: {b(values['pprof'])}",
    ]
    return "\n".join(lines) + "\n"


def volume_arg(host: str, container: str, *, read_only: bool, values: dict[str, Any]) -> str:
    if ":" in host or ":" in container:
        raise ValidationError("挂载路径不能包含冒号 (:)")
    options = "ro" if read_only else ""
    if values["selinux_relabel"]:
        options = (options + ",z").lstrip(",")
    spec = f"{host}:{container}"
    if options:
        spec += f":{options}"
    return spec


def docker_run_command(values: dict[str, Any], existing: dict[str, Any], *, check: bool) -> list[str]:
    del existing  # kept in the signature for symmetry with build_config
    config_dir = values["config_dir"]
    data_dir = values["data_dir"]
    run_dir = os.path.join(data_dir, "run")

    command = ["docker", "run"]
    if check:
        command += ["--rm", "--network", "none"]
    else:
        command += [
            "-d",
            "--name", values["container_name"],
            "--network", "host",
            "--restart", values["restart_policy"],
            "--stop-timeout", "30",
            "--init",
            "--log-driver", "json-file",
            "--log-opt", "max-size=10m",
            "--log-opt", "max-file=3",
            "--label", f"{MANAGED_LABEL}={MANAGED_VALUE}",
        ]

    if values["no_new_privileges"]:
        command += ["--security-opt", "no-new-privileges:true"]
    if os.geteuid() != 0:
        command += ["--user", f"{os.getuid()}:{os.getgid()}"]

    command += ["-v", volume_arg(config_dir, CONTAINER_CONFIG_PATH, read_only=True, values=values)]
    if not check:
        command += ["-v", volume_arg(data_dir, CONTAINER_DATA_PATH, read_only=False, values=values)]
        command += ["-v", volume_arg(run_dir, CONTAINER_RUN_PATH, read_only=False, values=values)]
        if values["geodata_dir"]:
            # Do not add :z here: relabelling a shared system geodata directory
            # can affect other services.  The UI warns about SELinux instead.
            if ":" in values["geodata_dir"]:
                raise ValidationError("geodata_dir 不能包含冒号 (:)")
            command += ["-v", f"{values['geodata_dir']}:{CONTAINER_GEODATA_PATH}:ro"]

    command.append(values["image"])
    if check:
        command += ["check", "-c", f"{CONTAINER_CONFIG_PATH}/{CONFIG_FILENAME}"]
    return command


def build_run_script(values: dict[str, Any], command: list[str], repo_root: Path) -> str:
    lines = [
        "#!/usr/bin/env bash",
        "# Generated by linkc-node Docker launcher.",
        "# The panel API key is stored in api-key, not in this script.",
        "set -euo pipefail",
    ]
    if values["build_local"]:
        lines += [
            "# Build the image first:",
            f"#   docker build -t {shlex.quote(values['image'])} {shlex.quote(str(repo_root))}",
        ]
    lines.append("exec " + shlex.join(command))
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# filesystem state
# ---------------------------------------------------------------------------


def existing_state(values: dict[str, Any]) -> dict[str, Any]:
    config_dir = Path(values["config_dir"])
    data_dir = Path(values["data_dir"])
    api_key_path = config_dir / API_KEY_FILENAME
    cert_path = config_dir / CERTS_DIRNAME / "fullchain.pem"
    key_path = config_dir / CERTS_DIRNAME / "privkey.pem"
    whitelist_path = config_dir / WHITELIST_FILENAME
    config_path = config_dir / CONFIG_FILENAME
    metadata_path = config_dir / METADATA_FILENAME
    run_dir = data_dir / "run"

    has_api_key = False
    api_key_error = ""
    try:
        if api_key_path.is_file():
            with api_key_path.open("r", encoding="utf-8", errors="ignore") as handle:
                has_api_key = bool(handle.read(4096).strip())
    except OSError as exc:
        api_key_error = str(exc)

    def nonempty(path: Path) -> bool:
        try:
            return path.is_file() and path.stat().st_size > 0
        except OSError:
            return False

    return {
        "config_exists": nonempty(config_path),
        "metadata_exists": nonempty(metadata_path),
        "has_api_key": has_api_key,
        "has_certs": nonempty(cert_path) and nonempty(key_path),
        "has_whitelist": nonempty(whitelist_path),
        "api_key_error": api_key_error,
        "config_dir": str(config_dir),
        "data_dir": str(data_dir),
        "config_path": str(config_path),
        "api_key_path": str(api_key_path),
        "cert_dir": str(config_dir / CERTS_DIRNAME),
        "whitelist_path": str(whitelist_path),
        "run_script_path": str(config_dir / RUN_SCRIPT_FILENAME),
        "host_socket": str(run_dir / "admin.sock"),
    }


def load_metadata(config_dir: Path) -> tuple[dict[str, Any] | None, str]:
    path = config_dir / METADATA_FILENAME
    if not path.is_file():
        return None, ""
    try:
        if path.stat().st_size > 4_000_000:
            return None, f"{path} 过大"
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return None, f"读取 {path} 失败: {exc}"
    if not isinstance(data, dict):
        return None, f"{path} 不是 JSON 对象"
    return data, ""


def last_config_dir_path() -> Path:
    return Path.home() / ".linkc-node" / "last-config-dir"


def read_last_config_dir() -> str:
    try:
        text = last_config_dir_path().read_text(encoding="utf-8").strip()
    except OSError:
        return ""
    if not text or not os.path.isabs(text):
        return ""
    return text


def write_last_config_dir(config_dir: str) -> None:
    path = last_config_dir_path()
    try:
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        try:
            os.chmod(path.parent, 0o700)
        except OSError:
            pass
        path.write_text(config_dir + "\n", encoding="utf-8")
        os.chmod(path, 0o600)
    except OSError:
        pass


def atomic_write(path: Path, content: str, mode: int) -> None:
    if path.is_symlink():
        raise OSError(f"refusing to write through symlink: {path}")
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, tmp = tempfile.mkstemp(prefix=f".{path.name}.", dir=str(path.parent))
    fd_owned = False
    try:
        os.fchmod(fd, mode)
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
            fd_owned = True
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)
    except Exception:
        if not fd_owned:
            try:
                os.close(fd)
            except OSError:
                pass
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def safe_atomic_write(path: Path, content: str, mode: int) -> None:
    try:
        atomic_write(path, content, mode)
    except OSError as exc:
        raise ValidationError(f"写入 {path} 失败: {exc}")


def chown_tree(path: Path, uid: int, gid: int) -> None:
    if path.is_symlink() or not path.exists():
        return
    os.chown(path, uid, gid, follow_symlinks=False)
    for root, dirs, files in os.walk(path, followlinks=False):
        os.chown(root, uid, gid, follow_symlinks=False)
        for name in dirs + files:
            item = os.path.join(root, name)
            if os.path.islink(item):
                continue
            os.chown(item, uid, gid, follow_symlinks=False)


def apply_config_dir_permissions(config_dir: Path) -> None:
    """Keep config_dir root-owned while granting UID 10001 read-only access.

    The container mounts config_dir :ro and runs as UID 10001.  Giving 10001
    ownership of config_dir or host scripts like docker-run.sh would widen
    host privilege boundaries, so root owns everything and only the files the
    container actually reads get group 10001 with 0640/0750.
    """
    certs_dir = config_dir / CERTS_DIRNAME
    backups_dir = config_dir / BACKUPS_DIRNAME
    for d in (config_dir, certs_dir):
        if d.exists() and not d.is_symlink():
            os.chown(d, 0, 10001, follow_symlinks=False)
            os.chmod(d, 0o750)
    for name in (CONFIG_FILENAME, API_KEY_FILENAME, WHITELIST_FILENAME):
        p = config_dir / name
        if p.is_file() and not p.is_symlink():
            os.chown(p, 0, 10001, follow_symlinks=False)
            os.chmod(p, 0o640)
    for name in ("fullchain.pem", "privkey.pem"):
        p = certs_dir / name
        if p.is_file() and not p.is_symlink():
            os.chown(p, 0, 10001, follow_symlinks=False)
            os.chmod(p, 0o640)
    for p, mode in (
        (config_dir / RUN_SCRIPT_FILENAME, 0o700),
        (config_dir / METADATA_FILENAME, 0o600),
    ):
        if p.is_file() and not p.is_symlink():
            os.chown(p, 0, 0, follow_symlinks=False)
            os.chmod(p, mode)
    if backups_dir.exists() and not backups_dir.is_symlink():
        chown_tree(backups_dir, 0, 0)
        os.chmod(backups_dir, 0o700)


def ensure_owned_dir(path: Path, allowed: set[str], label: str) -> None:
    """Create the directory, or adopt it only when it holds just our files.

    The launcher chmods these directories to 0700 and (as root) chowns them to
    the container user, so pointing one at an unrelated directory would damage
    the host.  Refuse anything that is not empty and not launcher-managed.
    """
    if path.is_symlink():
        raise ValidationError(f"{label}不能是符号链接: {path}")
    for parent in path.parents:
        if parent.is_symlink():
            raise ValidationError(f"{label}父路径包含符号链接: {parent}")
    try:
        if not path.exists():
            path.mkdir(parents=True, exist_ok=True, mode=0o700)
    except OSError as exc:
        raise ValidationError(f"无法创建{label} {path}: {exc}")
    if path.is_symlink() or not path.is_dir():
        raise ValidationError(f"{label}不是有效目录: {path}")
    real = os.path.realpath(path)
    if real in _BLOCKED_DIRS:
        raise ValidationError(f"{label}解析后指向系统目录 {real}")
    try:
        dir_entries = list(path.iterdir())
    except OSError as exc:
        raise ValidationError(f"无法读取{label} {path}: {exc}")
    for entry in dir_entries:
        if entry.is_symlink():
            raise ValidationError(f"{label} {path} 包含符号链接 {entry.name}，已拒绝操作")
    entries = {entry.name for entry in dir_entries}
    unknown = sorted(entries - allowed)
    if unknown:
        shown = "、".join(unknown[:5])
        more = f" 等 {len(unknown)} 项" if len(unknown) > 5 else ""
        raise ValidationError(
            f"{label} {path} 已存在且包含非本启动器管理的文件（{shown}{more}）；"
            "为避免误改系统目录，请改用空目录，或先把这些文件移走。"
        )
    try:
        os.chmod(path, 0o700)
    except OSError:
        pass


def prepare_files(values: dict[str, Any], existing: dict[str, Any], repo_root: Path) -> dict[str, Any]:
    config_dir = Path(values["config_dir"])
    data_dir = Path(values["data_dir"])
    run_dir = data_dir / "run"
    certs_dir = config_dir / CERTS_DIRNAME
    warnings: list[dict[str, str]] = []

    ensure_owned_dir(config_dir, CONFIG_DIR_ALLOWED, "配置目录")
    ensure_owned_dir(data_dir, DATA_DIR_ALLOWED, "数据目录")
    ensure_owned_dir(run_dir, RUN_DIR_ALLOWED, "运行目录")
    ensure_owned_dir(certs_dir, CERTS_DIR_ALLOWED, "证书目录")

    for directory in (config_dir, data_dir):
        probe = directory / ".linkc-launcher-write-test"
        try:
            probe.write_text("ok", encoding="utf-8")
            probe.unlink()
        except OSError as exc:
            raise ValidationError(f"目录不可写 {directory}: {exc}")

    backup_dir: Path | None = None
    backup_error = ""
    files_to_backup = [
        config_dir / CONFIG_FILENAME,
        config_dir / API_KEY_FILENAME,
        config_dir / WHITELIST_FILENAME,
        config_dir / METADATA_FILENAME,
        config_dir / RUN_SCRIPT_FILENAME,
        certs_dir / "fullchain.pem",
        certs_dir / "privkey.pem",
    ]
    existing_files = [path for path in files_to_backup if path.is_file() and not path.is_symlink()]
    if existing_files:
        stamp = local_timestamp()
        backup_dir = config_dir / BACKUPS_DIRNAME / stamp
        suffix = 1
        while backup_dir.exists():
            suffix += 1
            backup_dir = config_dir / BACKUPS_DIRNAME / f"{stamp}-{suffix}"
        try:
            backup_dir.mkdir(parents=True, mode=0o700)
            for path in existing_files:
                safe_atomic_write(backup_dir / path.name, path.read_text(encoding="utf-8"), 0o600)
        except (OSError, UnicodeDecodeError, ValidationError) as exc:
            backup_error = str(exc)
            warnings.append({"tone": "warn", "text": f"备份旧文件失败: {exc}"})
            backup_dir = None

    api_key_path = config_dir / API_KEY_FILENAME
    if values["api_key"]:
        safe_atomic_write(api_key_path, values["api_key"] + "\n", 0o600)
    elif not existing["has_api_key"]:
        raise ValidationError("API Key 为空，且目标配置目录没有 api-key 文件")

    cert_path = certs_dir / "fullchain.pem"
    key_path = certs_dir / "privkey.pem"
    if values["cert_pem"] and values["key_pem"]:
        safe_atomic_write(cert_path, values["cert_pem"], 0o600)
        safe_atomic_write(key_path, values["key_pem"], 0o600)
    elif values["clear_certs"]:
        for path in (cert_path, key_path):
            try:
                path.unlink()
            except FileNotFoundError:
                pass
            except OSError as exc:
                warnings.append({"tone": "warn", "text": f"删除旧证书失败 {path}: {exc}"})

    whitelist_path = config_dir / WHITELIST_FILENAME
    if values["whitelist"].strip():
        safe_atomic_write(whitelist_path, values["whitelist"].strip() + "\n", 0o600)
    else:
        try:
            whitelist_path.unlink()
        except FileNotFoundError:
            pass
        except OSError as exc:
            warnings.append({"tone": "warn", "text": f"删除旧白名单失败: {exc}"})

    config_path = config_dir / CONFIG_FILENAME
    safe_atomic_write(config_path, build_config(values, existing), 0o600)

    run_command = docker_run_command(values, existing, check=False)
    run_script_path = config_dir / RUN_SCRIPT_FILENAME
    safe_atomic_write(run_script_path, build_run_script(values, run_command, repo_root), 0o755)

    metadata = {key: values[key] for key in METADATA_KEYS if key in values}
    metadata["launcher_version"] = LAUNCHER_VERSION
    metadata["updated_at"] = utcnow()
    metadata["cert_present"] = bool(values["cert_pem"] and values["key_pem"]) or (
        existing["has_certs"] and not values["clear_certs"]
    )
    metadata["whitelist_present"] = bool(values["whitelist"].strip())
    metadata_path = config_dir / METADATA_FILENAME
    safe_atomic_write(
        metadata_path,
        json.dumps(metadata, ensure_ascii=False, indent=2) + "\n",
        0o600,
    )

    if os.geteuid() == 0:
        try:
            apply_config_dir_permissions(config_dir)
        except OSError as exc:
            warnings.append(
                {"tone": "warn", "text": f"设置配置目录权限失败 {config_dir}: {exc}"}
            )
        try:
            chown_tree(data_dir, 10001, 10001)
        except OSError as exc:
            warnings.append(
                {"tone": "warn", "text": f"chown 10001:10001 失败 {data_dir}: {exc}"}
            )

    write_last_config_dir(str(config_dir))

    return {
        "config_path": str(config_path),
        "api_key_path": str(api_key_path),
        "run_script_path": str(run_script_path),
        "whitelist_path": str(whitelist_path),
        "cert_dir": str(certs_dir),
        "backup_dir": str(backup_dir) if backup_dir else "",
        "backup_error": backup_error,
        "host_socket": str(run_dir / "admin.sock"),
        "warnings": warnings,
    }


# ---------------------------------------------------------------------------
# docker helpers
# ---------------------------------------------------------------------------


def tail_output(text: str, max_lines: int = 200, max_chars: int = 20000) -> str:
    text = _ANSI_RE.sub("", text or "")
    text = text.replace("\r\n", "\n").replace("\r", "\n")
    lines = text.splitlines()
    if len(lines) > max_lines:
        lines = [f"... (省略前 {len(lines) - max_lines} 行)"] + lines[-max_lines:]
    output = "\n".join(lines)
    if len(output) > max_chars:
        output = f"... (省略前 {len(output) - max_chars} 字符)\n" + output[-max_chars:]
    return output


def run_cmd(
    args: list[str],
    *,
    timeout: float = 60,
    cwd: str | None = None,
) -> dict[str, Any]:
    started = time.monotonic()
    env = os.environ.copy()
    env["DOCKER_CLI_HINTS"] = "false"
    env["NO_COLOR"] = "1"
    try:
        completed = subprocess.run(
            args,
            cwd=cwd,
            env=env,
            text=True,
            encoding="utf-8",
            errors="replace",
            capture_output=True,
            timeout=timeout,
        )
        code = completed.returncode
        stdout = completed.stdout or ""
        stderr = completed.stderr or ""
    except FileNotFoundError:
        code = 127
        stdout = ""
        stderr = f"command not found: {args[0]}"
    except OSError as exc:
        code = 126
        stdout = ""
        stderr = f"cannot execute {args[0]}: {exc}"
    except subprocess.TimeoutExpired as exc:
        code = 124
        stdout = exc.stdout if isinstance(exc.stdout, str) else ""
        stderr = exc.stderr if isinstance(exc.stderr, str) else ""
        stderr += f"\n[timeout after {timeout:g}s]"
    duration = time.monotonic() - started
    combined = stdout + (("\n" + stderr) if stderr else "")
    return {
        "code": code,
        "stdout": stdout,
        "stderr": stderr,
        "output": combined,
        "duration": duration,
        "args": args,
    }


def make_step(name: str, result: dict[str, Any]) -> dict[str, Any]:
    return {
        "name": name,
        "ok": result["code"] == 0,
        "command": shlex.join(result["args"]),
        "exit_code": result["code"],
        "duration": round(result["duration"], 2),
        "output": tail_output(result["output"], 120, 12000),
    }


def docker_state() -> dict[str, Any]:
    if not shutil.which("docker"):
        return {"ok": False, "error": "未找到 docker 命令；请先安装 Docker。"}
    result = run_cmd(
        [
            "docker", "info", "--format",
            "{{.ServerVersion}}|{{.OperatingSystem}}|{{.OSType}}|{{.Architecture}}|{{json .SecurityOptions}}",
        ],
        timeout=4,
    )
    if result["code"] != 0:
        return {"ok": False, "error": tail_output(result["output"], 20, 2000)}
    parts = result["stdout"].strip().split("|", 4)
    if len(parts) < 5:
        return {"ok": False, "error": "无法解析 docker info 输出"}
    return {
        "ok": True,
        "version": parts[0],
        "os": parts[1],
        "ostype": parts[2],
        "arch": parts[3],
        "rootless": "rootless" in parts[4].lower(),
    }


def container_state(name: str) -> dict[str, Any]:
    if not name or not _CONTAINER_RE.match(name):
        return {"exists": False, "error": "容器名称不合法"}
    result = run_cmd(["docker", "inspect", "--type", "container", name], timeout=10)
    if result["code"] != 0:
        output = result["output"].lower()
        if "no such" in output or "not found" in output:
            return {"exists": False}
        return {"exists": False, "error": tail_output(result["output"], 20, 2000)}
    try:
        data = json.loads(result["stdout"])[0]
    except (json.JSONDecodeError, IndexError, TypeError):
        return {"exists": False, "error": "无法解析 docker inspect 输出"}
    state = data.get("State") or {}
    config = data.get("Config") or {}
    labels = config.get("Labels") or {}
    return {
        "exists": True,
        "managed": labels.get(MANAGED_LABEL) == MANAGED_VALUE,
        "id": (data.get("Id") or "")[:12],
        "name": (data.get("Name") or "").lstrip("/"),
        "status": state.get("Status"),
        "running": state.get("Running"),
        "exit_code": state.get("ExitCode"),
        "started_at": state.get("StartedAt"),
        "finished_at": state.get("FinishedAt"),
        "restart_count": data.get("RestartCount"),
        "image": config.get("Image"),
    }


def git_commit(repo_root: Path) -> str:
    result = run_cmd(["git", "-C", str(repo_root), "rev-parse", "--short", "HEAD"], timeout=5)
    if result["code"] == 0:
        return result["stdout"].strip()
    return ""


def ensure_image(values: dict[str, Any], repo_root: Path, steps: list[dict[str, Any]]) -> None:
    image = values["image"]
    if values["build_local"]:
        dockerfile = repo_root / "Dockerfile"
        if not dockerfile.is_file():
            raise ValidationError(f"本地构建失败：{dockerfile} 不存在")
        command = [
            "docker", "build", "-t", image,
            "--build-arg", "VERSION=local",
            "--build-arg", f"BUILD_DATE={utcnow()}",
        ]
        commit = git_commit(repo_root)
        if commit:
            command += ["--build-arg", f"COMMIT={commit}"]
        command.append(".")
        result = run_cmd(command, timeout=900, cwd=str(repo_root))
        steps.append(make_step("构建本地镜像", result))
        if result["code"] != 0:
            raise DeploymentError("构建本地镜像失败", steps)
        return

    exists = run_cmd(["docker", "image", "inspect", image], timeout=10)["code"] == 0
    if values["pull_latest"]:
        result = run_cmd(["docker", "pull", image], timeout=600)
        steps.append(make_step("拉取镜像", result))
        if result["code"] != 0:
            if exists:
                steps[-1]["ok"] = True
                steps[-1]["tone"] = "warn"
                steps[-1]["detail"] = "拉取失败，继续使用本地镜像"
            else:
                raise DeploymentError("拉取镜像失败", steps)
    elif not exists:
        raise ValidationError(
            f"本地不存在镜像 {image}；请勾选“启动前拉取镜像”或先执行 docker pull"
        )


# ---------------------------------------------------------------------------
# preview / deploy / actions
# ---------------------------------------------------------------------------


def collect_warnings(
    values: dict[str, Any],
    existing: dict[str, Any],
    repo_root: Path,
) -> list[dict[str, str]]:
    warnings: list[dict[str, str]] = []

    def add(tone: str, text: str) -> None:
        warnings.append({"tone": tone, "text": text})

    if values["allow_http"]:
        add("warn", "面板使用 HTTP，API Key 会明文传输；仅建议内网测试。")
    if not values["panel_tls_verify"]:
        add("warn", "已关闭面板 TLS 校验；存在中间人风险。")
    if values["allow_private_targets"]:
        add("warn", "已允许 freedom 访问私网/回环地址；存在 SSRF 风险。")
    if values["pprof"]:
        add("warn", "已开启 pprof；生产环境用完请关闭。")
    if values["audit_action"] == "block":
        add("info", "审计动作是 block；建议先用 report 观察误报。")
    if not values["api_key"] and not existing["has_api_key"]:
        add("bad", "API Key 为空，且目标配置目录没有 api-key 文件。")
    if existing["api_key_error"]:
        add("warn", f"无法读取已有 api-key: {existing['api_key_error']}")
    if existing["config_exists"] and not existing["metadata_exists"]:
        add("warn", "目标目录已有 config.yaml，但不是本启动器生成的；部署前会备份并覆盖。")
    if existing["has_certs"] and values["clear_certs"]:
        add("warn", "将删除已有证书文件。")
    if values["geodata_dir"]:
        missing = [
            name for name in ("geosite.dat", "geoip.dat")
            if not (Path(values["geodata_dir"]) / name).is_file()
        ]
        if missing:
            add("warn", f"geodata 目录缺少 {', '.join(missing)}；geosite/geoip 审计规则可能无法编译。")
        if values["selinux_relabel"]:
            add("warn", "geodata 目录没有加 :z；如被 SELinux 拒绝，请手动调整标签。")
    else:
        add(
            "warn",
            "未挂载 geodata：容器内 /usr/share/xray 没有 geosite.dat/geoip.dat，"
            "面板下发的 geosite/geoip 审计规则会被跳过（等于放行）。面板不用地理规则时可忽略。",
        )
    if os.geteuid() != 0:
        add("info", "非 root 运行：容器将使用 --user UID:GID，配置/数据目录需由当前用户可写。")
    else:
        add("info", "root 运行：配置/数据目录将 chown 到 10001:10001。")
    if values["no_new_privileges"]:
        add(
            "warn",
            "已启用 no-new-privileges；它会禁用镜像内 linkc-node 的 CAP_NET_BIND_SERVICE "
            "文件能力，面板端口 <1024 时容器将无法绑定。",
        )
    else:
        add(
            "info",
            "镜像内 linkc-node 已设置 CAP_NET_BIND_SERVICE 文件能力，非 root 也可绑定低端口。",
        )
    if values["build_local"] and not (repo_root / "Dockerfile").is_file():
        add("bad", "已勾选本地构建，但仓库根目录没有 Dockerfile。")
    if values["build_local"] and values["pull_latest"]:
        add("info", "本地构建时不会拉取镜像。")
    if not values["build_local"] and not values["pull_latest"]:
        add("warn", "不会自动拉取镜像；请确保本地已有该镜像。")
    if values["selinux_relabel"]:
        add("warn", "已启用 SELinux :z 重新标记；会修改配置/数据目录的 SELinux 标签。")
    if values["container_name"]:
        state = container_state(values["container_name"])
        if state.get("exists") and not state.get("managed"):
            add(
                "bad",
                f"同名容器 {values['container_name']} 已存在，但不是由本启动器创建；部署会被拒绝。",
            )
    return warnings


def preview_payload(raw: Any, repo_root: Path) -> dict[str, Any]:
    try:
        values = normalize_values(raw)
        existing = existing_state(values)
        config = build_config(values, existing)
        run_command = docker_run_command(values, existing, check=False)
        check_command = docker_run_command(values, existing, check=True)
        return {
            "ok": True,
            "config": config,
            "command": shlex.join(run_command),
            "check_command": shlex.join(check_command),
            "run_script": build_run_script(values, run_command, repo_root),
            "warnings": collect_warnings(values, existing, repo_root),
            "existing": existing,
            "paths": {
                "config_path": existing["config_path"],
                "api_key_path": existing["api_key_path"],
                "run_script_path": existing["run_script_path"],
                "host_socket": existing["host_socket"],
            },
        }
    except ValidationError as exc:
        return {"ok": False, "error": str(exc)}


def stage_config(values: dict[str, Any], existing: dict[str, Any], target: Path) -> None:
    """Write the generated config into a throwaway directory for `check`."""
    os.chmod(target, 0o700)
    certs_dir = target / CERTS_DIRNAME
    certs_dir.mkdir(mode=0o700)

    api_key = values["api_key"]
    if not api_key:
        source = Path(values["config_dir"]) / API_KEY_FILENAME
        try:
            api_key = source.read_text(encoding="utf-8", errors="ignore").strip()
        except OSError:
            api_key = ""
    if api_key:
        api_key_path = target / API_KEY_FILENAME
        api_key_path.write_text(api_key + "\n", encoding="utf-8")
        os.chmod(api_key_path, 0o600)

    if values["cert_pem"] and values["key_pem"]:
        (certs_dir / "fullchain.pem").write_text(values["cert_pem"], encoding="utf-8")
        (certs_dir / "privkey.pem").write_text(values["key_pem"], encoding="utf-8")
    elif existing["has_certs"] and not values["clear_certs"]:
        for name in ("fullchain.pem", "privkey.pem"):
            source = Path(values["config_dir"]) / CERTS_DIRNAME / name
            if source.is_file():
                shutil.copy2(source, certs_dir / name)

    if values["whitelist"].strip():
        (target / WHITELIST_FILENAME).write_text(
            values["whitelist"].strip() + "\n", encoding="utf-8"
        )

    config_path = target / CONFIG_FILENAME
    config_path.write_text(build_config(values, existing), encoding="utf-8")
    os.chmod(config_path, 0o600)


def run_config_check(
    values: dict[str, Any],
    existing: dict[str, Any],
    steps: list[dict[str, Any]],
) -> None:
    """Validate the generated config in a temp dir before touching the live one."""
    if not values["api_key"] and not existing["has_api_key"]:
        raise ValidationError("API Key 为空，且目标配置目录没有 api-key 文件")

    staging = Path(tempfile.mkdtemp(prefix="linkc-launcher-check-"))
    try:
        stage_config(values, existing, staging)
        if os.geteuid() == 0:
            # The check container mounts staging :ro as UID 10001.
            apply_config_dir_permissions(staging)
        check_values = dict(values)
        check_values["config_dir"] = str(staging)
        result = run_cmd(docker_run_command(check_values, existing, check=True), timeout=120)
        steps.append(make_step("校验配置", result))
        if result["code"] != 0:
            raise DeploymentError("配置校验失败（配置未写入，旧容器未改动）", steps)
    finally:
        shutil.rmtree(staging, ignore_errors=True)


def wait_container_start(
    name: str,
    *,
    stable: float = 3.0,
    grace: float = 10.0,
) -> tuple[dict[str, Any], bool]:
    """Wait until the container has been running without restarts.

    `docker run -d` succeeds even when the process crashes right away, so a
    single status snapshot is not enough: a crash loop keeps reporting
    "running" between restarts.
    """
    started = time.monotonic()
    running_since: float | None = None
    state = container_state(name)
    while True:
        now = time.monotonic()
        status = state.get("status")
        restarts = state.get("restart_count") or 0
        if status in ("exited", "dead", "restarting") or restarts > 0:
            return state, False
        if status == "running":
            if running_since is None:
                running_since = now
            if now - running_since >= stable:
                return state, True
        else:
            running_since = None
        if now - started >= grace:
            return state, False
        time.sleep(0.5)
        state = container_state(name)


def restore_container(
    name: str,
    backup_name: str,
    steps: list[dict[str, Any]],
    *,
    start: bool,
) -> bool:
    if not backup_name:
        return False
    result = run_cmd(["docker", "rename", backup_name, name], timeout=30)
    steps.append(make_step("回滚容器名", result))
    if result["code"] != 0:
        return False
    if not start:
        return True
    result = run_cmd(["docker", "start", name], timeout=60)
    steps.append(make_step("回滚启动旧容器", result))
    return result["code"] == 0


def restore_backup_files(
    backup_dir: str,
    config_dir: str,
    backup_error: str = "",
) -> list[dict[str, Any]]:
    """Put the pre-deploy files back after a failed container swap.

    `prepare_files` saves every file it is about to overwrite into
    `backups/<stamp>/`; a failed deploy must restore them before the old
    container is started again, otherwise the old container reads the new
    (possibly broken) config and fails too.
    """
    if not backup_dir:
        if backup_error:
            return [
                {
                    "name": "恢复旧配置",
                    "ok": False,
                    "detail": f"部署前备份失败，无法自动恢复: {backup_error}",
                }
            ]
        return []

    source = Path(backup_dir)
    target = Path(config_dir)
    if not source.is_dir():
        return [
            {
                "name": "恢复旧配置",
                "ok": False,
                "detail": f"备份目录不存在: {source}",
            }
        ]

    managed = [
        (CONFIG_FILENAME, target / CONFIG_FILENAME, 0o600),
        (API_KEY_FILENAME, target / API_KEY_FILENAME, 0o600),
        (WHITELIST_FILENAME, target / WHITELIST_FILENAME, 0o600),
        (METADATA_FILENAME, target / METADATA_FILENAME, 0o600),
        (RUN_SCRIPT_FILENAME, target / RUN_SCRIPT_FILENAME, 0o755),
        ("fullchain.pem", target / CERTS_DIRNAME / "fullchain.pem", 0o600),
        ("privkey.pem", target / CERTS_DIRNAME / "privkey.pem", 0o600),
    ]
    restored: list[str] = []
    removed: list[str] = []
    errors: list[str] = []
    for name, dest, mode in managed:
        src = source / name
        try:
            if src.is_file():
                atomic_write(dest, src.read_text(encoding="utf-8"), mode)
                restored.append(dest.name)
            elif dest.exists():
                dest.unlink()
                removed.append(dest.name)
        except (OSError, UnicodeDecodeError) as exc:
            errors.append(f"{dest.name}: {exc}")

    if os.geteuid() == 0:
        try:
            apply_config_dir_permissions(target)
        except OSError as exc:
            errors.append(f"chmod/chown {target}: {exc}")

    detail_parts: list[str] = []
    if restored:
        detail_parts.append("恢复 " + "、".join(restored))
    if removed:
        detail_parts.append("删除新增 " + "、".join(removed))
    if errors:
        detail_parts.append("失败: " + "；".join(errors[:3]))
    return [
        {
            "name": "恢复旧配置",
            "ok": not errors,
            "detail": "；".join(detail_parts) or "没有需要恢复的文件",
        }
    ]


def replace_container(
    values: dict[str, Any],
    existing: dict[str, Any],
    steps: list[dict[str, Any]],
    *,
    backup_dir: str = "",
    backup_error: str = "",
) -> dict[str, Any]:
    """Swap the container, keeping the old one until the new one is stable."""
    name = values["container_name"]
    state = container_state(name)
    backup_name = ""
    was_running = False
    if state.get("exists"):
        if not state.get("managed"):
            raise ValidationError(
                f"同名容器 {name} 已存在，但不是由本启动器创建；"
                "请更换名称或手动删除"
            )
        was_running = bool(state.get("running"))
        if was_running:
            result = run_cmd(["docker", "stop", name], timeout=60)
            steps.append(make_step("停止旧容器", result))
            if result["code"] != 0:
                steps.extend(restore_backup_files(backup_dir, values["config_dir"], backup_error))
                raise DeploymentError("停止旧容器失败", steps)
        backup_name = f"{name[:80]}-launcher-backup-{secrets.token_hex(3)}"
        result = run_cmd(["docker", "rename", name, backup_name], timeout=30)
        steps.append(make_step("暂存旧容器", result))
        if result["code"] != 0:
            steps.extend(restore_backup_files(backup_dir, values["config_dir"], backup_error))
            if was_running:
                result = run_cmd(["docker", "start", name], timeout=60)
                steps.append(make_step("回滚启动旧容器", result))
            raise DeploymentError("重命名旧容器失败", steps)

    result = run_cmd(docker_run_command(values, existing, check=False), timeout=120)
    steps.append(make_step("启动容器", result))
    if result["code"] != 0:
        # A failed `docker run` can leave a container in Created state with
        # the target name; remove it before renaming the old one back.
        run_cmd(["docker", "rm", "-f", name], timeout=60)
        steps.extend(restore_backup_files(backup_dir, values["config_dir"], backup_error))
        restored = restore_container(name, backup_name, steps, start=was_running)
        if backup_name:
            message = "启动容器失败，已回滚到旧容器" if restored else "启动容器失败，旧容器恢复失败"
        else:
            message = "启动容器失败"
        raise DeploymentError(message, steps)

    state, healthy = wait_container_start(name)
    if not healthy:
        logs_res = run_cmd(["docker", "logs", "--tail", "80", name], timeout=15)
        if logs_res.get("output"):
            steps.append(make_step("失败容器日志", logs_res))
        run_cmd(["docker", "rm", "-f", name], timeout=60)
        steps.extend(restore_backup_files(backup_dir, values["config_dir"], backup_error))
        restored = restore_container(name, backup_name, steps, start=was_running)
        detail = f"状态 {state.get('status', 'unknown')}，重启 {state.get('restart_count') or 0} 次"
        if backup_name:
            detail += "；旧容器已恢复" if restored else "；旧容器恢复失败，请查看步骤"
        raise DeploymentError(f"容器未稳定运行（{detail}）", steps)

    if backup_name:
        result = run_cmd(["docker", "rm", backup_name], timeout=60)
        steps.append(make_step("清理旧容器", result))
    return state


def deploy(raw_values: Any, repo_root: Path, mode: str = "deploy") -> dict[str, Any]:
    steps: list[dict[str, Any]] = []
    try:
        values = normalize_values(raw_values)
        existing = existing_state(values)

        if mode == "save":
            paths = prepare_files(values, existing, repo_root)
            steps.append({"name": "写入配置", "ok": True, "detail": paths["config_path"]})
            if paths.get("backup_dir"):
                steps.append({"name": "备份旧文件", "ok": True, "detail": paths["backup_dir"]})
            elif paths.get("backup_error"):
                steps.append({"name": "备份旧文件", "ok": False, "detail": paths["backup_error"]})
            return {
                "ok": True,
                "mode": mode,
                "steps": steps,
                "paths": paths,
                "existing": existing_state(values),
                "container": container_state(values["container_name"]),
                "warnings": paths["warnings"],
            }

        docker = docker_state()
        if not docker.get("ok"):
            raise ValidationError(f"Docker 不可用：{docker.get('error', '未知错误')}")

        # Refuse an unmanaged container before writing anything: otherwise a
        # deploy that is going to be rejected would still overwrite the config
        # that container may be using.  `check` does not touch the container,
        # so it stays available for validation.
        if mode == "deploy":
            container = container_state(values["container_name"])
            if container.get("exists") and not container.get("managed"):
                raise ValidationError(
                    f"同名容器 {values['container_name']} 已存在，但不是由本启动器创建；"
                    "请更换名称或手动删除"
                )

        # Pull/build before anything is written: a failed pull must not replace
        # the config the running node depends on.
        ensure_image(values, repo_root, steps)

        if mode == "check" or values["run_check"]:
            run_config_check(values, existing, steps)

        if mode == "check":
            return {
                "ok": True,
                "mode": mode,
                "steps": steps,
                "paths": existing,
                "existing": existing,
                "container": container_state(values["container_name"]),
                "warnings": collect_warnings(values, existing, repo_root),
            }

        paths = prepare_files(values, existing, repo_root)
        steps.append({"name": "写入配置", "ok": True, "detail": paths["config_path"]})
        if paths.get("backup_dir"):
            steps.append({"name": "备份旧文件", "ok": True, "detail": paths["backup_dir"]})
        elif paths.get("backup_error"):
            steps.append({"name": "备份旧文件", "ok": False, "detail": paths["backup_error"]})

        state = replace_container(
            values,
            existing,
            steps,
            backup_dir=paths.get("backup_dir", ""),
            backup_error=paths.get("backup_error", ""),
        )

        logs_result = run_cmd(
            ["docker", "logs", "--tail", "80", values["container_name"]],
            timeout=20,
        )
        logs = tail_output(logs_result["output"], 80, 12000)
        steps.append(
            {
                "name": "读取启动日志",
                "ok": logs_result["code"] == 0,
                "detail": f"exit {logs_result['code']}",
                "output": logs,
            }
        )

        return {
            "ok": True,
            "mode": mode,
            "steps": steps,
            "paths": paths,
            "existing": existing_state(values),
            "container": state,
            "logs": logs,
            "warnings": paths["warnings"],
        }
    except ValidationError as exc:
        return {"ok": False, "error": str(exc), "steps": steps}
    except DeploymentError as exc:
        container: dict[str, Any] = {}
        try:
            container = container_state(values["container_name"])
        except (NameError, KeyError):
            pass
        return {
            "ok": False,
            "error": str(exc),
            "steps": exc.steps,
            "container": container,
        }
    except Exception as exc:  # pragma: no cover - last-resort guard
        traceback.print_exc()
        return {"ok": False, "error": f"内部错误: {exc}", "steps": steps}


def action_payload(action: str, payload: dict[str, Any], repo_root: Path) -> dict[str, Any]:
    try:
        name = _as_str(payload.get("container_name", DEFAULT_CONTAINER), "container_name")
    except ValidationError as exc:
        return {"ok": False, "error": str(exc)}
    if not _CONTAINER_RE.match(name):
        return {"ok": False, "error": "容器名称不合法"}

    if action == "check":
        return deploy(payload.get("values", {}), repo_root, mode="check")

    if action == "status":
        state = container_state(name)
        return {
            "ok": not bool(state.get("error")),
            "action": action,
            "container": state,
            "error": state.get("error", ""),
        }

    if action == "logs":
        state = container_state(name)
        if not state.get("exists"):
            return {
                "ok": False,
                "error": state.get("error") or f"容器 {name} 不存在",
                "container": state,
            }
        result = run_cmd(
            ["docker", "logs", "--tail", "200", "--timestamps", name],
            timeout=20,
        )
        return {
            "ok": result["code"] == 0,
            "action": action,
            "container": state,
            "logs": tail_output(result["output"], 200, 20000),
            "error": "" if result["code"] == 0 else tail_output(result["output"], 20),
        }

    if action in ("stop", "start", "restart", "remove"):
        state = container_state(name)
        if not state.get("exists"):
            return {
                "ok": False,
                "error": state.get("error") or f"容器 {name} 不存在",
                "container": state,
            }
        if not state.get("managed"):
            return {
                "ok": False,
                "error": f"容器 {name} 不是由本启动器创建；拒绝执行 {action}",
                "container": state,
            }

        steps: list[dict[str, Any]] = []
        if action == "remove":
            if state.get("running"):
                result = run_cmd(["docker", "stop", name], timeout=60)
                steps.append(make_step("停止容器", result))
                if result["code"] != 0:
                    return {
                        "ok": False,
                        "action": action,
                        "steps": steps,
                        "container": container_state(name),
                        "error": tail_output(result["output"], 20),
                    }
            result = run_cmd(["docker", "rm", name], timeout=60)
            steps.append(make_step("删除容器", result))
        else:
            commands = {
                "stop": ["docker", "stop", name],
                "start": ["docker", "start", name],
                "restart": ["docker", "restart", name],
            }
            labels = {
                "stop": "停止容器",
                "start": "启动容器",
                "restart": "重启容器",
            }
            result = run_cmd(commands[action], timeout=60)
            steps.append(make_step(labels[action], result))

        time.sleep(1.0)
        return {
            "ok": result["code"] == 0,
            "action": action,
            "steps": steps,
            "container": container_state(name),
            "error": "" if result["code"] == 0 else tail_output(result["output"], 20),
        }

    return {"ok": False, "error": f"未知操作: {action}"}


def state_payload(
    repo_root: Path,
    config_dir_override: str = "",
    container_name_override: str = "",
) -> dict[str, Any]:
    defaults = default_values()
    config_dir = Path(defaults["config_dir"])
    if config_dir_override:
        config_dir = Path(config_dir_override).expanduser()
    else:
        last = read_last_config_dir()
        if last and (Path(last) / METADATA_FILENAME).is_file():
            config_dir = Path(last)

    metadata, metadata_error = load_metadata(config_dir)
    values = dict(defaults)
    if metadata:
        for key in METADATA_KEYS:
            if key in metadata:
                values[key] = metadata[key]
        if metadata.get("config_dir") and not config_dir_override:
            config_dir = Path(str(metadata["config_dir"]))
    if config_dir_override:
        values["config_dir"] = str(config_dir)
    if container_name_override:
        values["container_name"] = container_name_override

    existing = existing_state(values)
    docker = docker_state()
    container = (
        container_state(values["container_name"])
        if docker.get("ok")
        else {"exists": False, "error": docker.get("error", "")}
    )

    warnings: list[dict[str, str]] = []
    if os.geteuid() != 0:
        warnings.append(
            {
                "tone": "info",
                "text": "当前以非 root 运行；容器会使用 --user UID:GID，配置/数据目录需由当前用户可写。",
            }
        )
    if not docker.get("ok"):
        warnings.append({"tone": "bad", "text": f"Docker 不可用：{docker.get('error', '未知错误')}"})
    elif docker.get("ostype") != "linux":
        warnings.append({"tone": "warn", "text": "当前 Docker 不是 Linux 主机；--network host 的行为可能不同。"})
    elif docker.get("rootless"):
        warnings.append({"tone": "warn", "text": "检测到 rootless Docker；host 网络与低端口绑定可能受限。"})
    if metadata_error:
        warnings.append({"tone": "warn", "text": metadata_error})
    if existing["api_key_error"]:
        warnings.append({"tone": "warn", "text": f"无法读取已有 api-key: {existing['api_key_error']}"})
    if existing["config_exists"] and not existing["metadata_exists"]:
        warnings.append(
            {
                "tone": "warn",
                "text": "目标目录已有 config.yaml，但不是本启动器生成的；部署前会备份并覆盖。",
            }
        )
    if container.get("exists") and not container.get("managed"):
        warnings.append(
            {
                "tone": "bad",
                "text": f"同名容器 {values['container_name']} 已存在，但不是由本启动器创建；部署会被拒绝。",
            }
        )

    return {
        "ok": True,
        "launcher_version": LAUNCHER_VERSION,
        "python": sys.version.split()[0],
        "is_root": os.geteuid() == 0,
        "uid": os.getuid(),
        "gid": os.getgid(),
        "repo_root": str(repo_root),
        "dockerfile_exists": (repo_root / "Dockerfile").is_file(),
        "defaults": defaults,
        "values": values,
        "existing": existing,
        "metadata_found": metadata is not None,
        "metadata_error": metadata_error,
        "docker": docker,
        "container": container,
        "paths": {
            "config_path": existing["config_path"],
            "api_key_path": existing["api_key_path"],
            "run_script_path": existing["run_script_path"],
            "host_socket": existing["host_socket"],
            "config_dir": existing["config_dir"],
            "data_dir": existing["data_dir"],
        },
        "warnings": warnings,
    }


# ---------------------------------------------------------------------------
# Unix socket admin client & CLI helpers
# ---------------------------------------------------------------------------


def query_admin_socket(socket_path: str, endpoint: str) -> tuple[int, str]:
    sock_path = socket_path.strip()
    if not sock_path:
        raise ValidationError("admin socket 路径不能为空")
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.settimeout(5.0)
    try:
        sock.connect(sock_path)
        req = f"GET {endpoint} HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n\r\n"
        sock.sendall(req.encode("ascii"))
        chunks: list[bytes] = []
        total = 0
        while True:
            chunk = sock.recv(65536)
            if not chunk:
                break
            total += len(chunk)
            if total > 4 * 1024 * 1024:
                raise DeploymentError(f"{endpoint} 响应超过 4 MiB")
            chunks.append(chunk)
    except OSError as exc:
        raise DeploymentError(f"连接 {sock_path} 失败: {exc}") from exc
    finally:
        sock.close()

    raw = b"".join(chunks)
    sep = raw.find(b"\r\n\r\n")
    if sep < 0:
        raise DeploymentError(f"来自 {sock_path} 的响应不是合法 HTTP")
    head = raw[:sep].decode("iso-8859-1", errors="replace")
    body = raw[sep + 4:].decode("utf-8", errors="replace")
    status_line = head.splitlines()[0] if head else ""
    parts = status_line.split(" ", 2)
    if len(parts) < 2 or not parts[1].isdigit():
        raise DeploymentError(f"无法解析 HTTP 状态行: {status_line}")
    return int(parts[1]), body


def _read_text_input(path_str: str, label: str, *, max_bytes: int = 2 * 1024 * 1024) -> str:
    path = Path(path_str).expanduser()
    if path.is_symlink():
        raise ValidationError(f"{label} 不能是符号链接: {path}")
    if not path.is_file():
        raise ValidationError(f"找不到 {label}: {path}")
    if path.stat().st_size > max_bytes:
        raise ValidationError(f"{label} 过大: {path}")
    return path.read_text(encoding="utf-8")


def _load_cli_values(args: argparse.Namespace) -> dict[str, Any]:
    raw = default_values()

    if not getattr(args, "fresh", False):
        cfg_dir = getattr(args, "config_dir", None)
        if cfg_dir:
            candidate = Path(cfg_dir).expanduser()
        else:
            last = read_last_config_dir()
            candidate = (
                Path(last)
                if last and (Path(last) / METADATA_FILENAME).is_file()
                else Path(raw["config_dir"])
            )
        metadata, metadata_error = load_metadata(candidate)
        if metadata_error:
            raise ValidationError(metadata_error)
        if metadata:
            for key in METADATA_KEYS:
                if key in metadata:
                    raw[key] = metadata[key]

    config_json = getattr(args, "config_json", None)
    if config_json:
        if config_json == "-":
            json_text = sys.stdin.read()
        else:
            json_text = _read_text_input(config_json, "--config-json")
        try:
            loaded = json.loads(json_text)
        except json.JSONDecodeError as exc:
            raise ValidationError(f"--config-json 解析失败: {exc}") from exc
        if not isinstance(loaded, dict):
            raise ValidationError("--config-json 顶层必须是 JSON 对象")
        raw.update(loaded)

    mapping = {
        "panel_url": "base_url",
        "node_id": "node_id",
        "profile": "profile",
        "allow_http": "allow_http",
        "panel_tls_verify": "panel_tls_verify",
        "panel_timeout": "panel_timeout",
        "max_attempts": "max_attempts",
        "max_response_mb": "max_response_mb",
        "panel_proxy": "panel_proxy",
        "listen": "listen",
        "clear_certs": "clear_certs",
        "audit_enabled": "audit_enabled",
        "audit_action": "audit_action",
        "limits_mode": "limits_mode",
        "alive_ip_mode": "alive_ip_mode",
        "include_online_zero": "include_online_zero",
        "allow_private_targets": "allow_private_targets",
        "whitelist": "whitelist",
        "geodata_dir": "geodata_dir",
        "image": "image",
        "container_name": "container_name",
        "config_dir": "config_dir",
        "data_dir": "data_dir",
        "restart_policy": "restart_policy",
        "pull_latest": "pull_latest",
        "build_local": "build_local",
        "run_check": "run_check",
        "no_new_privileges": "no_new_privileges",
        "selinux_relabel": "selinux_relabel",
        "log_level": "log_level",
        "log_format": "log_format",
        "pull_interval": "pull_interval",
        "push_interval": "push_interval",
        "collect_interval": "collect_interval",
        "status_interval": "status_interval",
        "max_journal_mb": "max_journal_mb",
        "metrics": "metrics",
        "pprof": "pprof",
        "sniffing_mode": "sniffing_mode",
        "mass_remove_guard": "mass_remove_guard",
    }
    for attr, key in mapping.items():
        val = getattr(args, attr, None)
        if val is not None:
            raw[key] = val

    api_key_sources = sum(
        1
        for flag in (
            getattr(args, "api_key", None) is not None,
            bool(getattr(args, "api_key_env", None)),
            bool(getattr(args, "api_key_stdin", False)),
            bool(getattr(args, "ask_api_key", False)),
        )
        if flag
    )
    if api_key_sources > 1:
        raise ValidationError("--api-key / --api-key-env / --api-key-stdin / --ask-api-key 不能同时使用")
    if getattr(args, "api_key", None) is not None:
        raw["api_key"] = args.api_key
    elif getattr(args, "api_key_env", None):
        env_name = args.api_key_env
        if env_name not in os.environ:
            raise ValidationError(f"环境变量 {env_name} 未设置")
        raw["api_key"] = os.environ[env_name]
    elif getattr(args, "api_key_stdin", False):
        if getattr(args, "config_json", None) == "-":
            raise ValidationError("--api-key-stdin 不能与 --config-json - 同时使用")
        raw["api_key"] = sys.stdin.read().strip()
    elif getattr(args, "ask_api_key", False):
        try:
            raw["api_key"] = getpass.getpass("面板 API Key: ").strip()
        except (EOFError, KeyboardInterrupt) as exc:
            raise ValidationError("已取消输入 API Key") from exc

    cert_file = getattr(args, "cert_file", None)
    key_file = getattr(args, "key_file", None)
    if bool(cert_file) != bool(key_file):
        raise ValidationError("--cert-file 和 --key-file 必须成对提供")
    if cert_file and key_file:
        raw["cert_pem"] = _read_text_input(cert_file, "--cert-file")
        raw["key_pem"] = _read_text_input(key_file, "--key-file")

    whitelist_file = getattr(args, "whitelist_file", None)
    if whitelist_file:
        raw["whitelist"] = _read_text_input(whitelist_file, "--whitelist-file")

    return raw


def _print_warnings(warnings: list[dict[str, str]]) -> None:
    if not warnings:
        return
    print("\n提示/告警:")
    for item in warnings:
        tone = str(item.get("tone", "info")).upper()
        print(f"  [{tone}] {item.get('text', '')}")


def _print_steps(steps: list[dict[str, Any]]) -> None:
    if not steps:
        return
    print("\n执行步骤:")
    for step in steps:
        mark = "OK" if step.get("ok") else "FAIL"
        detail = step.get("detail", "")
        suffix = f" ({detail})" if detail else ""
        print(f"  [{mark}] {step.get('name', '')}{suffix}")
        output = str(step.get("output", "")).strip()
        if not step.get("ok") and output:
            print(f"       {output}")


def _prompt(label: str, default: str = "") -> str:
    suffix = f" [{default}]" if default else ""
    try:
        answer = input(f"{label}{suffix}: ").strip()
    except EOFError:
        return default
    return answer if answer else default


def _prompt_bool(label: str, default: bool) -> bool:
    hint = "Y/n" if default else "y/N"
    while True:
        try:
            answer = input(f"{label} [{hint}]: ").strip().lower()
        except EOFError:
            return default
        if not answer:
            return default
        if answer in ("y", "yes", "1", "true"):
            return True
        if answer in ("n", "no", "0", "false"):
            return False
        print("请输入 y 或 n。")


def _prompt_choice(label: str, choices: list[str], default: str) -> str:
    opts = "/".join(choices)
    while True:
        answer = _prompt(f"{label} ({opts})", default).lower()
        if answer in choices:
            return answer
        print(f"无效选项，请从 {opts} 中选择。")


def run_wizard(repo_root: Path, args: argparse.Namespace) -> int:
    try:
        raw = _load_cli_values(args)
    except ValidationError as exc:
        print(f"错误: {exc}", file=sys.stderr)
        return 1

    print("=" * 68)
    print(" linkc-node Docker 命令行配置向导")
    print("=" * 68)
    print(f" 配置目录 : {raw['config_dir']}")
    print(f" 数据目录 : {raw['data_dir']}")
    print(f" 运行身份 : {'root' if os.geteuid() == 0 else f'UID {os.getuid()}:{os.getgid()}'}")
    print("-" * 68)

    try:
        raw["config_dir"] = _prompt("配置目录", str(raw["config_dir"]))
        raw["data_dir"] = _prompt("数据目录", str(raw["data_dir"]))
        raw["container_name"] = _prompt("容器名称", str(raw["container_name"]))

        existing = existing_state(normalize_values(
            {
                **raw,
                "base_url": raw.get("base_url") or "https://panel.example.com",
                "node_id": raw.get("node_id") or "1",
                "api_key": raw.get("api_key") or "placeholder",
            }
        ))

        raw["base_url"] = _prompt("面板地址 (https://...)", str(raw.get("base_url", "")))
        if raw["base_url"].lower().startswith("http://"):
            raw["allow_http"] = _prompt_bool("检测到 http://，是否允许明文 HTTP（仅测试）", bool(raw.get("allow_http")))
        else:
            raw["allow_http"] = False

        raw["node_id"] = _prompt("节点 ID", str(raw.get("node_id", "")))
        if not raw.get("api_key"):
            key_hint = "面板 API Key (留空保留已有 api-key)" if existing["has_api_key"] else "面板 API Key"
            raw["api_key"] = getpass.getpass(f"{key_hint}: ").strip()

        raw["listen"] = _prompt("监听地址", str(raw.get("listen", "0.0.0.0")))

        cert_default = "keep" if existing["has_certs"] else "none"
        cert_choices = ["none", "files", "keep", "clear"] if existing["has_certs"] else ["none", "files"]
        cert_mode = _prompt_choice("TLS 证书来源", cert_choices, cert_default)
        if cert_mode == "files":
            cert_path = _prompt("fullchain.pem 路径")
            key_path = _prompt("privkey.pem 路径")
            raw["cert_pem"] = _read_text_input(cert_path, "证书文件")
            raw["key_pem"] = _read_text_input(key_path, "私钥文件")
            raw["clear_certs"] = False
        elif cert_mode == "clear":
            raw["cert_pem"] = ""
            raw["key_pem"] = ""
            raw["clear_certs"] = True
        else:
            raw["clear_certs"] = False

        has_dockerfile = (repo_root / "Dockerfile").is_file()
        img_default = "build" if raw.get("build_local") and has_dockerfile else ("pull" if raw.get("pull_latest", True) else "local")
        img_choices = ["pull", "build", "local"] if has_dockerfile else ["pull", "local"]
        img_mode = _prompt_choice("镜像来源", img_choices, img_default)
        raw["build_local"] = img_mode == "build"
        raw["pull_latest"] = img_mode == "pull"
        raw["image"] = _prompt("Docker 镜像", str(raw.get("image", DEFAULT_IMAGE)))

        raw["audit_enabled"] = _prompt_bool("启用审计", bool(raw.get("audit_enabled", True)))
        if raw["audit_enabled"]:
            raw["audit_action"] = _prompt_choice("审计动作", ["report", "block"], str(raw.get("audit_action", "report")))
        raw["limits_mode"] = _prompt_choice("限速/设备限制模式", ["enforce", "report", "off"], str(raw.get("limits_mode", "enforce")))
        raw["allow_private_targets"] = _prompt_bool("允许代理访问内网目标（通常为 n）", bool(raw.get("allow_private_targets", False)))
        raw["run_check"] = _prompt_bool("启动前执行 linkc-node check", bool(raw.get("run_check", True)))
    except (KeyboardInterrupt, EOFError):
        print("\n已取消。")
        return 1
    except ValidationError as exc:
        print(f"\n错误: {exc}", file=sys.stderr)
        return 1

    preview = preview_payload(raw, repo_root)
    if not preview.get("ok"):
        print(f"\n校验失败: {preview.get('error', '未知错误')}", file=sys.stderr)
        return 1

    _print_warnings(preview.get("warnings", []))
    print("\n--- 生成的 config.yaml ---")
    print(preview["config"], end="")
    print("\n--- Docker 启动命令 ---")
    print(preview["command"])

    action = _prompt_choice("下一步操作: deploy=部署启动, stage=仅生成配置, cancel=取消", ["deploy", "stage", "cancel"], "deploy")
    if action == "cancel":
        print("未修改任何文件或容器。")
        return 0

    mode = "deploy" if action == "deploy" else "save"
    result = deploy(raw, repo_root, mode=mode)
    if getattr(args, "json", False):
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0 if result.get("ok") else 1
    return _render_deploy_result(result)


def _render_deploy_result(result: dict[str, Any]) -> int:
    _print_steps(result.get("steps", []))
    _print_warnings(result.get("warnings", []))
    if not result.get("ok"):
        print(f"\n失败: {result.get('error', '未知错误')}", file=sys.stderr)
        return 1

    paths = result.get("paths", {})
    print("\n操作完成:")
    if paths.get("config_path"):
        print(f"  配置文件 : {paths['config_path']}")
    if paths.get("api_key_path"):
        print(f"  API Key  : {paths['api_key_path']}")
    if paths.get("run_script_path"):
        print(f"  启动脚本 : {paths['run_script_path']}")
    if paths.get("host_socket"):
        print(f"  管理 Sock: {paths['host_socket']}")
    container = result.get("container", {})
    if container and container.get("exists"):
        print(
            f"  容器状态 : {container.get('name')} ({container.get('status')}, "
            f"health={container.get('health') or 'n/a'})"
        )
    return 0


def _add_value_flags(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--fresh", action="store_true", help="ignore existing launcher.json defaults")
    parser.add_argument("--config-json", default=None, help="load parameter overrides from JSON file (or - for stdin)")
    parser.add_argument("--panel-url", default=None, help="panel base URL (https://...)")
    parser.add_argument("--node-id", default=None, help="panel node ID")
    parser.add_argument("--profile", default=None, help=f"panel profile (default: {DEFAULT_PROFILE})")
    parser.add_argument("--api-key", default=None, help="panel API key (prefer --api-key-env or --api-key-stdin)")
    parser.add_argument("--api-key-env", default=None, help="read panel API key from environment variable")
    parser.add_argument("--api-key-stdin", action="store_true", help="read panel API key from stdin")
    parser.add_argument("--ask-api-key", action="store_true", help="prompt interactively for panel API key")
    parser.add_argument("--allow-http", dest="allow_http", action="store_true", default=None, help="allow http:// panel URL (testing only)")
    parser.add_argument("--no-allow-http", dest="allow_http", action="store_false", default=None, help="require https:// panel URL")
    parser.add_argument("--panel-tls-verify", dest="panel_tls_verify", action="store_true", default=None, help="verify panel TLS certificate")
    parser.add_argument("--no-panel-tls-verify", dest="panel_tls_verify", action="store_false", default=None, help="disable panel TLS verification")
    parser.add_argument("--panel-timeout", default=None, help="panel HTTP timeout duration (e.g. 15s)")
    parser.add_argument("--max-attempts", type=int, default=None, help="panel HTTP max retry attempts (1..10)")
    parser.add_argument("--max-response-mb", type=int, default=None, help="panel max response size MiB (1..256)")
    parser.add_argument("--panel-proxy", default=None, help="HTTP/SOCKS5 proxy URL for panel requests")
    parser.add_argument("--listen", default=None, help="node listen IP address (default: 0.0.0.0)")
    parser.add_argument("--cert-file", default=None, help="TLS fullchain PEM file to stage")
    parser.add_argument("--key-file", default=None, help="TLS private key PEM file to stage")
    parser.add_argument("--clear-certs", action="store_true", default=None, help="remove staged TLS certificates")
    parser.add_argument("--audit-enabled", dest="audit_enabled", action="store_true", default=None, help="enable audit rules")
    parser.add_argument("--no-audit-enabled", dest="audit_enabled", action="store_false", default=None, help="disable audit rules")
    parser.add_argument("--audit-action", choices=["report", "block"], default=None, help="audit action")
    parser.add_argument("--limits-mode", choices=["enforce", "report", "off"], default=None, help="speed/device limits mode")
    parser.add_argument("--alive-ip-mode", choices=["changed", "full", "off"], default=None, help="alive_ip push mode")
    parser.add_argument("--include-online-zero", dest="include_online_zero", action="store_true", default=None)
    parser.add_argument("--no-include-online-zero", dest="include_online_zero", action="store_false", default=None)
    parser.add_argument("--allow-private-targets", dest="allow_private_targets", action="store_true", default=None)
    parser.add_argument("--no-allow-private-targets", dest="allow_private_targets", action="store_false", default=None)
    parser.add_argument("--whitelist-file", default=None, help="local audit whitelist file to copy")
    parser.add_argument("--whitelist", default=None, help="inline audit whitelist content")
    parser.add_argument("--geodata-dir", default=None, help="host directory containing geoip.dat/geosite.dat")
    parser.add_argument("--image", default=None, help=f"Docker image (default: {DEFAULT_IMAGE})")
    parser.add_argument("--container-name", default=None, help=f"Docker container name (default: {DEFAULT_CONTAINER})")
    parser.add_argument("--config-dir", default=None, help="host config directory")
    parser.add_argument("--data-dir", default=None, help="host data directory")
    parser.add_argument("--restart-policy", choices=["unless-stopped", "always", "on-failure", "no"], default=None)
    parser.add_argument("--pull-latest", dest="pull_latest", action="store_true", default=None, help="pull image before deploy")
    parser.add_argument("--no-pull-latest", dest="pull_latest", action="store_false", default=None, help="do not pull image")
    parser.add_argument("--build-local", dest="build_local", action="store_true", default=None, help="docker build from --repo-root")
    parser.add_argument("--no-build-local", dest="build_local", action="store_false", default=None, help="do not docker build locally")
    parser.add_argument("--run-check", dest="run_check", action="store_true", default=None, help="run linkc-node check before starting")
    parser.add_argument("--no-run-check", dest="run_check", action="store_false", default=None, help="skip linkc-node check")
    parser.add_argument("--no-new-privileges", dest="no_new_privileges", action="store_true", default=None)
    parser.add_argument("--allow-new-privileges", dest="no_new_privileges", action="store_false", default=None)
    parser.add_argument("--selinux-relabel", dest="selinux_relabel", action="store_true", default=None)
    parser.add_argument("--no-selinux-relabel", dest="selinux_relabel", action="store_false", default=None)
    parser.add_argument("--log-level", choices=["debug", "info", "warn", "error"], default=None)
    parser.add_argument("--log-format", choices=["json", "text"], default=None)
    parser.add_argument("--pull-interval", default=None)
    parser.add_argument("--push-interval", default=None)
    parser.add_argument("--collect-interval", default=None)
    parser.add_argument("--status-interval", default=None)
    parser.add_argument("--max-journal-mb", type=int, default=None)
    parser.add_argument("--metrics", dest="metrics", action="store_true", default=None)
    parser.add_argument("--no-metrics", dest="metrics", action="store_false", default=None)
    parser.add_argument("--pprof", dest="pprof", action="store_true", default=None)
    parser.add_argument("--no-pprof", dest="pprof", action="store_false", default=None)
    parser.add_argument("--sniffing-mode", choices=["auto", "on", "off"], default=None)
    parser.add_argument("--mass-remove-guard", choices=["warn", "reject", "off"], default=None)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="docker-launch.sh",
        description="CLI tool for deploying and managing linkc-node in Docker.",
    )
    parser.add_argument(
        "--repo-root",
        default=str(Path(__file__).resolve().parent.parent),
        help="repository root (for local docker build)",
    )
    parser.add_argument("--version", action="version", version=f"%(prog)s {LAUNCHER_VERSION}")
    sub = parser.add_subparsers(dest="command", metavar="COMMAND")

    p_wizard = sub.add_parser("wizard", aliases=["init"], help="interactive setup wizard")
    p_wizard.add_argument("--fresh", action="store_true", help="ignore existing launcher.json defaults")
    p_wizard.add_argument("--config-dir", default=None, help="host config directory")
    p_wizard.add_argument("--json", action="store_true", help="output final result as JSON")

    p_deploy = sub.add_parser("deploy", help="stage config, validate, and deploy container")
    _add_value_flags(p_deploy)
    p_deploy.add_argument("--stage-only", action="store_true", help="write config files without starting container")
    p_deploy.add_argument("--dry-run", action="store_true", help="preview config and docker command without writing")
    p_deploy.add_argument("--json", action="store_true", help="output JSON")

    p_preview = sub.add_parser("preview", help="preview config.yaml, docker command, and warnings")
    _add_value_flags(p_preview)
    p_preview.add_argument("--json", action="store_true", help="output JSON")

    p_stage = sub.add_parser("stage", help="write config.yaml, api-key, and docker-run.sh without starting")
    _add_value_flags(p_stage)
    p_stage.add_argument("--json", action="store_true", help="output JSON")

    p_check = sub.add_parser("check", help="validate config using temporary linkc-node check container")
    _add_value_flags(p_check)
    p_check.add_argument("--json", action="store_true", help="output JSON")

    p_status = sub.add_parser("status", aliases=["state"], help="show Docker, container, and config status")
    p_status.add_argument("--config-dir", default="", help="host config directory")
    p_status.add_argument("--container-name", default="", help="Docker container name")
    p_status.add_argument("--json", action="store_true", help="output JSON")

    p_logs = sub.add_parser("logs", help="show or follow container logs")
    p_logs.add_argument("--container-name", default="", help="Docker container name")
    p_logs.add_argument("-n", "--tail", type=int, default=200, help="number of lines to show (default: 200)")
    p_logs.add_argument("-f", "--follow", action="store_true", help="follow log output")
    p_logs.add_argument("--json", action="store_true", help="output JSON")

    for act, desc in (
        ("start", "start managed container"),
        ("stop", "stop managed container"),
        ("restart", "restart managed container"),
        ("remove", "stop and remove managed container"),
    ):
        p_act = sub.add_parser(act, help=desc)
        p_act.add_argument("--container-name", default="", help="Docker container name")
        p_act.add_argument("--json", action="store_true", help="output JSON")

    for cmd_name, desc in (
        ("node-status", "query node /status over admin Unix socket"),
        ("node-metrics", "query node /metrics over admin Unix socket"),
        ("node-ready", "query node /readyz over admin Unix socket"),
    ):
        p_node = sub.add_parser(cmd_name, help=desc)
        p_node.add_argument("--config-dir", default="", help="host config directory")
        p_node.add_argument("-s", "--socket", default="", help="explicit admin.sock path")
        p_node.add_argument("--json", action="store_true", help="output JSON")

    return parser


def _resolve_container_name(explicit: str) -> str:
    if explicit:
        return explicit
    last = read_last_config_dir()
    if last:
        meta, _ = load_metadata(Path(last))
        if meta and meta.get("container_name"):
            return str(meta["container_name"])
    return DEFAULT_CONTAINER


def main(argv: list[str] | None = None) -> int:
    if sys.version_info < (3, 8):
        print("错误: 需要 Python 3.8 或更新版本。", file=sys.stderr)
        return 1

    parser = build_parser()
    args = parser.parse_args(argv)
    if not args.command:
        parser.print_help()
        return 0

    repo_root = Path(args.repo_root).resolve()

    if args.command in ("wizard", "init"):
        return run_wizard(repo_root, args)

    if args.command in ("preview", "stage", "check", "deploy"):
        try:
            raw = _load_cli_values(args)
        except ValidationError as exc:
            if args.json:
                print(json.dumps({"ok": False, "error": str(exc)}, ensure_ascii=False))
            else:
                print(f"错误: {exc}", file=sys.stderr)
            return 1

        if args.command == "preview" or getattr(args, "dry_run", False):
            res = preview_payload(raw, repo_root)
            if args.json:
                print(json.dumps(res, ensure_ascii=False, indent=2))
                return 0 if res.get("ok") else 1
            if not res.get("ok"):
                print(f"错误: {res.get('error', '未知错误')}", file=sys.stderr)
                return 1
            _print_warnings(res.get("warnings", []))
            print("--- config.yaml ---")
            print(res["config"], end="")
            print("\n--- docker run ---")
            print(res["command"])
            return 0

        mode = "save" if (args.command == "stage" or getattr(args, "stage_only", False)) else (
            "check" if args.command == "check" else "deploy"
        )
        res = deploy(raw, repo_root, mode=mode)
        if args.json:
            print(json.dumps(res, ensure_ascii=False, indent=2))
            return 0 if res.get("ok") else 1
        return _render_deploy_result(res)

    if args.command in ("status", "state"):
        res = state_payload(
            repo_root,
            config_dir_override=args.config_dir,
            container_name_override=args.container_name,
        )
        if args.json:
            print(json.dumps(res, ensure_ascii=False, indent=2))
            return 0
        docker = res["docker"]
        container = res["container"]
        paths = res["paths"]
        print(f"Launcher  : v{res['launcher_version']} (Python {res['python']})")
        print(f"Docker    : {'ok ' + docker.get('version', '') if docker.get('ok') else docker.get('error', '不可用')}")
        if container.get("exists"):
            print(
                f"Container : {container.get('name')} | status={container.get('status')} "
                f"| managed={container.get('managed')} | health={container.get('health') or 'n/a'} "
                f"| restarts={container.get('restart_count', 0)}"
            )
        else:
            print(f"Container : {res['values']['container_name']} (未创建)")
        print(f"Config    : {paths['config_path']} ({'存在' if res['existing']['config_exists'] else '未生成'})")
        print(f"API Key   : {paths['api_key_path']} ({'存在' if res['existing']['has_api_key'] else '未设置'})")
        print(f"Data Dir  : {paths['data_dir']}")
        print(f"Admin Sock: {paths['host_socket']}")
        _print_warnings(res.get("warnings", []))
        return 0

    if args.command == "logs":
        name = _resolve_container_name(args.container_name)
        if not _CONTAINER_RE.match(name):
            print("错误: 容器名称不合法", file=sys.stderr)
            return 1
        if args.follow and args.json:
            print("错误: --follow 不能与 --json 同时使用", file=sys.stderr)
            return 1
        if args.follow:
            proc = subprocess.run(
                ["docker", "logs", "-f", "--tail", str(max(1, args.tail)), "--timestamps", name],
                check=False,
            )
            return proc.returncode
        if args.json:
            res = action_payload("logs", {"container_name": name}, repo_root)
            print(json.dumps(res, ensure_ascii=False, indent=2))
            return 0 if res.get("ok") else 1
        result = run_cmd(
            ["docker", "logs", "--tail", str(max(1, args.tail)), "--timestamps", name],
            timeout=20,
        )
        if result["output"]:
            print(result["output"], end="" if result["output"].endswith("\n") else "\n")
        return 0 if result["code"] == 0 else 1

    if args.command in ("start", "stop", "restart", "remove"):
        name = _resolve_container_name(args.container_name)
        res = action_payload(args.command, {"container_name": name}, repo_root)
        if args.json:
            print(json.dumps(res, ensure_ascii=False, indent=2))
            return 0 if res.get("ok") else 1
        _print_steps(res.get("steps", []))
        if not res.get("ok"):
            print(f"错误: {res.get('error', '操作失败')}", file=sys.stderr)
            return 1
        container = res.get("container", {})
        if container:
            print(f"容器 {container.get('name', name)}: status={container.get('status', 'unknown')}")
        return 0

    if args.command in ("node-status", "node-metrics", "node-ready"):
        sock = args.socket
        if not sock:
            st = state_payload(repo_root, config_dir_override=args.config_dir)
            sock = st["paths"]["host_socket"]
        endpoint_map = {
            "node-status": "/status",
            "node-metrics": "/metrics",
            "node-ready": "/readyz",
        }
        endpoint = endpoint_map[args.command]
        try:
            status_code, body = query_admin_socket(sock, endpoint)
        except (ValidationError, DeploymentError) as exc:
            if args.json:
                print(json.dumps({"ok": False, "error": str(exc)}, ensure_ascii=False))
            else:
                print(f"错误: {exc}", file=sys.stderr)
            return 1
        if args.json and args.command != "node-status":
            print(
                json.dumps(
                    {"ok": status_code == 200, "status_code": status_code, "body": body},
                    ensure_ascii=False,
                    indent=2,
                )
            )
            return 0 if status_code == 200 else 1
        if status_code != 200:
            print(f"HTTP {status_code}: {body.strip()}", file=sys.stderr)
            return 1
        if args.command == "node-status":
            try:
                parsed = json.loads(body)
                print(json.dumps(parsed, ensure_ascii=False, indent=2))
            except json.JSONDecodeError:
                print(body, end="" if body.endswith("\n") else "\n")
        else:
            print(body, end="" if body.endswith("\n") else "\n")
        return 0

    parser.print_help()
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
