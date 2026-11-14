#!/usr/bin/env python3
"""
Local-only web UI for launching linkc-node in Docker.

The launcher writes a complete config.yaml, an api-key file (0600), optional
TLS certificates, and a reproducible docker-run.sh, then starts the container
with host networking.  It only uses the Python standard library.

Security model:
  * binds to 0.0.0.0 by default; non-loopback binds require --username, and the
    page is plain HTTP, so keep it behind a firewall or an SSH tunnel;
  * every request needs a random token (in the URL for the page, in the
    X-Launcher-Token header for API calls);
  * Host/Origin checks block DNS-rebinding and cross-site POSTs;
  * failed logins are rate limited per client address and password hashing is
    capped to a few concurrent verifications;
  * secrets are never returned by the API, never put in the docker command,
    and never written to launcher.json.
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import getpass
import hashlib
import hmac
import ipaddress
import json
import os
import re
import secrets
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import traceback
import urllib.parse
import webbrowser
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

LAUNCHER_VERSION = "1.0.0"

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

CSP = (
    "default-src 'none'; "
    "style-src 'unsafe-inline'; "
    "script-src 'unsafe-inline'; "
    "img-src data:; "
    "connect-src 'self'; "
    "form-action 'none'; "
    "base-uri 'none'; "
    "frame-ancestors 'none'"
)

HASH_FILE_MAX_BYTES = 64 * 1024
SCRYPT_MAXMEM = 256 * 1024 * 1024

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

# Failed-login throttling for HTTP Basic Auth.
AUTH_MAX_FAILURES = 10
AUTH_FAILURE_WINDOW = 60.0
AUTH_MAX_CONCURRENT_VERIFY = 4
AUTH_VERIFY_TIMEOUT = 5.0

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


class PasswordHash:
    """A scrypt password hash loaded from a JSON or one-line file."""

    def __init__(self, n: int, r: int, p: int, salt: bytes, digest: bytes):
        self.n = n
        self.r = r
        self.p = p
        self.salt = salt
        self.digest = digest

    def verify(self, password: str) -> bool:
        candidate = hashlib.scrypt(
            password.encode("utf-8"),
            salt=self.salt,
            n=self.n,
            r=self.r,
            p=self.p,
            dklen=len(self.digest),
            maxmem=SCRYPT_MAXMEM,
        )
        return hmac.compare_digest(candidate, self.digest)


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
        "no_new_privileges": True,
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
    if values["listen"] != "localhost":
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


def default_credentials_path() -> Path:
    return Path.home() / ".linkc-node" / "launcher-credentials.json"


def write_credentials_file(
    path: Path,
    *,
    url: str,
    lan_url: str,
    username: str,
    password: str,
    listen: str,
    port: int,
) -> None:
    payload: dict[str, Any] = {
        "generated_at": utcnow(),
        "listen": listen,
        "port": port,
        "url": url,
    }
    if lan_url:
        payload["lan_url"] = lan_url
    payload["username"] = username
    payload["password"] = password
    atomic_write(
        path,
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
        0o600,
    )
    if path.parent == Path.home() / ".linkc-node":
        try:
            os.chmod(path.parent, 0o700)
        except OSError:
            pass


def _validate_scrypt_params(n: int, r: int, p: int, salt: bytes, digest: bytes) -> None:
    if n < 2**10 or n > 2**17 or (n & (n - 1)) != 0:
        raise ValidationError("scrypt n 必须是 1024..131072 之间的 2 的幂")
    if r < 1 or r > 16:
        raise ValidationError("scrypt r 必须在 1..16 之间")
    if p < 1 or p > 8:
        raise ValidationError("scrypt p 必须在 1..8 之间")
    if n * r > 2**20:
        raise ValidationError("scrypt n*r 过大（上限 1048576）")
    if len(salt) < 8 or len(salt) > 64:
        raise ValidationError("scrypt salt 长度必须在 8..64 字节")
    if len(digest) < 16 or len(digest) > 64:
        raise ValidationError("scrypt hash 长度必须在 16..64 字节")


def _b64decode(value: str, field: str) -> bytes:
    try:
        return base64.b64decode(value, validate=True)
    except (ValueError, TypeError) as exc:
        raise ValidationError(f"{field} 不是合法 base64: {exc}")


def load_password_hash(path: Path) -> PasswordHash:
    try:
        if path.stat().st_size > HASH_FILE_MAX_BYTES:
            raise ValidationError(f"密码哈希文件过大: {path}")
        text = path.read_text(encoding="utf-8-sig").strip()
    except (OSError, UnicodeDecodeError) as exc:
        raise ValidationError(f"读取密码哈希文件失败 {path}: {exc}")
    if not text:
        raise ValidationError(f"密码哈希文件为空: {path}")

    if text.startswith("{"):
        try:
            data = json.loads(text)
        except json.JSONDecodeError as exc:
            raise ValidationError(f"密码哈希 JSON 解析失败: {exc}")
        if not isinstance(data, dict):
            raise ValidationError("密码哈希 JSON 必须是对象")
        if str(data.get("algorithm", "")).lower() != "scrypt":
            raise ValidationError("密码哈希 algorithm 只支持 scrypt")
        try:
            n = int(data["n"])
            r = int(data["r"])
            p = int(data["p"])
        except (KeyError, TypeError, ValueError):
            raise ValidationError("密码哈希 JSON 需要整数 n/r/p")
        salt = _b64decode(str(data.get("salt", "")), "salt")
        digest_b64 = data.get("hash") or data.get("digest") or ""
        digest = _b64decode(str(digest_b64), "hash")
    else:
        parts = text.split("$")
        if len(parts) != 6 or parts[0].lower() != "scrypt":
            raise ValidationError("密码哈希格式应为 scrypt$n$r$p$salt$hash 或 JSON")
        try:
            n = int(parts[1])
            r = int(parts[2])
            p = int(parts[3])
        except ValueError:
            raise ValidationError("密码哈希 n/r/p 必须是整数")
        salt = _b64decode(parts[4], "salt")
        digest = _b64decode(parts[5], "hash")

    _validate_scrypt_params(n, r, p, salt, digest)
    return PasswordHash(n, r, p, salt, digest)


def generate_password_hash(args: argparse.Namespace) -> int:
    password = args.password or os.environ.get("LINKC_LAUNCHER_PASSWORD", "")
    if not password:
        try:
            first = getpass.getpass("页面密码: ")
            second = getpass.getpass("再次输入: ")
        except (EOFError, KeyboardInterrupt):
            print("\n错误: 需要密码。", file=sys.stderr)
            return 1
        if first != second:
            print("错误: 两次输入的密码不一致。", file=sys.stderr)
            return 1
        password = first
    if not password:
        print("错误: 密码不能为空。", file=sys.stderr)
        return 1
    if len(password) > 4096:
        print("错误: 密码过长。", file=sys.stderr)
        return 1
    if "\x00" in password:
        print("错误: 密码不能包含 NUL。", file=sys.stderr)
        return 1
    if "\n" in password or "\r" in password:
        print("错误: 密码不能包含换行。", file=sys.stderr)
        return 1

    n, r, p = 16384, 8, 1
    salt = secrets.token_bytes(16)
    try:
        digest = hashlib.scrypt(
            password.encode("utf-8"),
            salt=salt,
            n=n,
            r=r,
            p=p,
            dklen=32,
            maxmem=SCRYPT_MAXMEM,
        )
    except (ValueError, MemoryError) as exc:
        print(f"错误: 生成 scrypt 哈希失败: {exc}", file=sys.stderr)
        return 1

    line = "scrypt${}${}${}${}${}".format(
        n,
        r,
        p,
        base64.b64encode(salt).decode("ascii"),
        base64.b64encode(digest).decode("ascii"),
    )

    output_path: Path | None = None
    if args.hash_password_output:
        output_path = Path(args.hash_password_output).expanduser()
        if not output_path.is_absolute():
            output_path = Path.cwd() / output_path
        output_path = Path(os.path.abspath(output_path))
        if output_path.exists() and output_path.is_dir():
            print(f"错误: 哈希输出路径是目录: {output_path}", file=sys.stderr)
            return 1
        try:
            atomic_write(output_path, line + "\n", 0o600)
        except OSError as exc:
            print(f"错误: 写入哈希文件失败 {output_path}: {exc}", file=sys.stderr)
            return 1
        print(f"哈希文件: {output_path}（0600）")

    print("密码哈希:")
    print(line)
    if output_path is not None:
        print("启动示例:")
        print(
            "  ./scripts/docker-launch.sh --username admin "
            f"--password-hash-file {shlex.quote(str(output_path))}"
        )
    else:
        print("把上面的哈希保存到 0600 文件后，用 --password-hash-file 启动。")
    return 0


def atomic_write(path: Path, content: str, mode: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, tmp = tempfile.mkstemp(prefix=f".{path.name}.", dir=str(path.parent))
    try:
        os.fchmod(fd, mode)
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, path)
    except Exception:
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
    if not path.exists():
        return
    for root, dirs, files in os.walk(path):
        os.chown(root, uid, gid)
        for name in dirs + files:
            item = os.path.join(root, name)
            try:
                os.chown(item, uid, gid, follow_symlinks=False)
            except TypeError:
                os.chown(item, uid, gid)


def ensure_owned_dir(path: Path, allowed: set[str], label: str) -> None:
    """Create the directory, or adopt it only when it holds just our files.

    The launcher chmods these directories to 0700 and (as root) chowns them to
    the container user, so pointing one at an unrelated directory would damage
    the host.  Refuse anything that is not empty and not launcher-managed.
    """
    try:
        if not path.exists():
            path.mkdir(parents=True, exist_ok=True, mode=0o700)
    except OSError as exc:
        raise ValidationError(f"无法创建{label} {path}: {exc}")
    if not path.is_dir():
        raise ValidationError(f"{label}不是目录: {path}")
    real = os.path.realpath(path)
    if real in _BLOCKED_DIRS:
        raise ValidationError(f"{label}解析后指向系统目录 {real}")
    try:
        entries = {entry.name for entry in path.iterdir()}
    except OSError as exc:
        raise ValidationError(f"无法读取{label} {path}: {exc}")
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
    existing_files = [path for path in files_to_backup if path.is_file()]
    if existing_files:
        stamp = local_timestamp()
        backup_dir = config_dir / BACKUPS_DIRNAME / stamp
        suffix = 1
        while backup_dir.exists():
            suffix += 1
            backup_dir = config_dir / BACKUPS_DIRNAME / f"{stamp}-{suffix}"
        try:
            backup_dir.mkdir(parents=True)
            for path in existing_files:
                shutil.copy2(path, backup_dir / path.name)
        except OSError as exc:
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
        for directory in (config_dir, data_dir):
            try:
                chown_tree(directory, 10001, 10001)
            except OSError as exc:
                warnings.append(
                    {"tone": "warn", "text": f"chown 10001:10001 失败 {directory}: {exc}"}
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
            # The image runs as 10001; the check container must be able to read
            # the staged files (the real config dir is chowned the same way).
            chown_tree(staging, 10001, 10001)
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
            chown_tree(target, 10001, 10001)
        except OSError as exc:
            errors.append(f"chown {target}: {exc}")

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
    auth_enabled: bool = False,
    bind: str = "",
) -> dict[str, Any]:
    defaults = default_values()
    config_dir = Path(defaults["config_dir"])
    last = read_last_config_dir()
    if last and (Path(last) / METADATA_FILENAME).is_file():
        config_dir = Path(last)

    metadata, metadata_error = load_metadata(config_dir)
    values = dict(defaults)
    if metadata:
        for key in METADATA_KEYS:
            if key in metadata:
                values[key] = metadata[key]
        if metadata.get("config_dir"):
            config_dir = Path(str(metadata["config_dir"]))

    exposed = bind not in ("", "127.0.0.1", "::1", "[::1]", "localhost")
    existing = existing_state(values)
    docker = docker_state()
    container = (
        container_state(values["container_name"])
        if docker.get("ok")
        else {"exists": False, "error": docker.get("error", "")}
    )

    warnings: list[dict[str, str]] = []
    if exposed and not auth_enabled:
        warnings.append(
            {
                "tone": "bad",
                "text": (
                    f"页面正在监听 {bind} 且未启用用户名/密码；任何能访问该端口的人"
                    "只要拿到 token 就能控制 Docker。建议加 --username，或用防火墙限制来源。"
                ),
            }
        )
    elif exposed:
        warnings.append(
            {
                "tone": "warn",
                "text": (
                    f"页面正在监听 {bind}；请用防火墙限制来源。"
                    "Basic Auth 在纯 HTTP 下不加密。"
                ),
            }
        )
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
        "auth_enabled": auth_enabled,
        "bind": bind,
        "exposed": exposed,
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
# HTTP server
# ---------------------------------------------------------------------------


def log_error(message: str) -> None:
    sys.stderr.write(f"[{time.strftime('%H:%M:%S')}] {message}\n")
    sys.stderr.flush()


class LauncherServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(
        self,
        address: tuple[str, int],
        handler: type[BaseHTTPRequestHandler],
        *,
        token: str,
        html_path: Path,
        repo_root: Path,
        username: str = "",
        password: str = "",
        password_hash: PasswordHash | None = None,
    ):
        self.token = token
        self.html_path = html_path
        self.repo_root = repo_root
        self.bind = address[0]
        self.username = username
        self.password = password
        self.password_hash = password_hash
        self.auth_cache: set[str] = set()
        self.auth_cache_key = secrets.token_bytes(32)
        self.auth_lock = threading.Lock()
        self.auth_failures: dict[str, list[float]] = {}
        self.auth_verify_slots = threading.BoundedSemaphore(AUTH_MAX_CONCURRENT_VERIFY)
        self.action_lock = threading.Lock()
        super().__init__(address, handler)


class Handler(BaseHTTPRequestHandler):
    server_version = "linkc-launcher/" + LAUNCHER_VERSION
    protocol_version = "HTTP/1.1"
    # A stalled client cannot pin a thread forever (slowloris-ish).
    timeout = 30

    # -- logging ---------------------------------------------------------

    def log_message(self, fmt: str, *args: Any) -> None:
        del fmt, args
        path = urllib.parse.urlsplit(self.path).path
        if path == "/api/preview":
            return
        if path.startswith("/api/") or path == "/":
            log_error(f"{self.command} {path}")

    # -- security checks -------------------------------------------------

    def _client_ip(self) -> str:
        try:
            return str(self.client_address[0])
        except (IndexError, TypeError):
            return "?"

    def _auth_locked_out(self) -> bool:
        ip = self._client_ip()
        now = time.monotonic()
        with self.server.auth_lock:
            stamps = [
                stamp for stamp in self.server.auth_failures.get(ip, [])
                if now - stamp < AUTH_FAILURE_WINDOW
            ]
            if stamps:
                self.server.auth_failures[ip] = stamps
            else:
                self.server.auth_failures.pop(ip, None)
            return len(stamps) >= AUTH_MAX_FAILURES

    def _record_auth_failure(self) -> None:
        ip = self._client_ip()
        now = time.monotonic()
        with self.server.auth_lock:
            if len(self.server.auth_failures) > 1024:
                self.server.auth_failures.clear()
            stamps = [
                stamp for stamp in self.server.auth_failures.get(ip, [])
                if now - stamp < AUTH_FAILURE_WINDOW
            ]
            stamps.append(now)
            self.server.auth_failures[ip] = stamps

    def _check_auth(self) -> bool:
        """Authenticate and answer 401/429 itself; False means stop."""
        if not self.server.username:
            return True
        if self._auth_locked_out():
            self._send_too_many()
            return False
        if self._auth_ok():
            return True
        self._record_auth_failure()
        self._send_unauthorized()
        return False

    def _auth_ok(self) -> bool:
        if not self.server.username:
            return True
        header = self.headers.get("Authorization", "")
        scheme, _, encoded = header.partition(" ")
        if scheme.lower() != "basic" or not encoded:
            return False

        cache_key = ""
        if self.server.password_hash is not None:
            cache_key = hmac.new(
                self.server.auth_cache_key,
                header.encode("utf-8"),
                hashlib.sha256,
            ).hexdigest()
            with self.server.auth_lock:
                if cache_key in self.server.auth_cache:
                    return True

        try:
            decoded = base64.b64decode(encoded.strip(), validate=True).decode("utf-8")
        except (ValueError, UnicodeDecodeError):
            return False
        if ":" not in decoded:
            return False
        user, password = decoded.split(":", 1)
        if not hmac.compare_digest(user.encode("utf-8"), self.server.username.encode("utf-8")):
            return False

        if self.server.password_hash is not None:
            if not self.server.auth_verify_slots.acquire(timeout=AUTH_VERIFY_TIMEOUT):
                return False
            try:
                ok = self.server.password_hash.verify(password)
            except (ValueError, MemoryError):
                ok = False
            finally:
                self.server.auth_verify_slots.release()
            if ok:
                with self.server.auth_lock:
                    if len(self.server.auth_cache) >= 128:
                        self.server.auth_cache.clear()
                    self.server.auth_cache.add(cache_key)
            return ok

        return hmac.compare_digest(
            password.encode("utf-8"),
            self.server.password.encode("utf-8"),
        )

    def _token_ok(self) -> bool:
        token = self.headers.get("X-Launcher-Token", "")
        if not token:
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
            token = (query.get("token") or [""])[0]
        return bool(token) and hmac.compare_digest(token, self.server.token)

    def _host_ok(self) -> bool:
        host = self.headers.get("Host", "")
        if not host:
            return False
        hostname = host
        if host.startswith("["):
            end = host.find("]")
            if end < 0:
                return False
            hostname = host[1:end]
        elif host.count(":") == 1:
            hostname = host.rsplit(":", 1)[0]
        if hostname in ("localhost", "127.0.0.1", "::1"):
            return True
        try:
            ipaddress.ip_address(hostname)
            return True
        except ValueError:
            return False

    def _origin_ok(self) -> bool:
        origin = self.headers.get("Origin")
        if not origin:
            return True
        return origin == f"http://{self.headers.get('Host', '')}"

    # -- responses -------------------------------------------------------

    def _send_bytes(
        self,
        status: int,
        body: bytes,
        content_type: str,
        extra_headers: dict[str, str] | None = None,
    ) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("X-Frame-Options", "DENY")
        self.send_header("Referrer-Policy", "no-referrer")
        self.send_header("Content-Security-Policy", CSP)
        if self.close_connection:
            self.send_header("Connection", "close")
        if extra_headers:
            for key, value in extra_headers.items():
                self.send_header(key, value)
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _send_unauthorized(self) -> None:
        self.close_connection = True
        self._send_bytes(
            401,
            b"authentication required\n",
            "text/plain; charset=utf-8",
            {"WWW-Authenticate": 'Basic realm="linkc-node launcher", charset="UTF-8"'},
        )

    def _send_too_many(self) -> None:
        self.close_connection = True
        self._send_bytes(
            429,
            b"too many failed logins; retry later\n",
            "text/plain; charset=utf-8",
            {"Retry-After": str(int(AUTH_FAILURE_WINDOW))},
        )

    def _send_json(self, payload: Any, status: int = 200) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self._send_bytes(status, body, "application/json; charset=utf-8")

    def _send_text(self, status: int, text: str) -> None:
        self._send_bytes(status, text.encode("utf-8"), "text/plain; charset=utf-8")

    def _send_html(self) -> None:
        try:
            body = self.server.html_path.read_bytes()
        except OSError as exc:
            self._send_text(500, f"无法读取页面文件: {exc}")
            return
        self._send_bytes(200, body, "text/html; charset=utf-8")

    def _send_forbidden(self) -> None:
        body = """<!doctype html>
<meta charset="utf-8">
<title>需要启动 token</title>
<body style="font-family:system-ui,sans-serif;max-width:720px;margin:10vh auto;padding:0 24px;line-height:1.7">
<h1>需要启动 token</h1>
<p>请回到运行 <code>docker-launch.sh</code> 的终端，复制完整地址（包含 <code>?token=...</code>）再打开。</p>
<p>token 用于阻止其他网页调用本机的 Docker 控制接口；监听地址由 <code>--bind</code> 决定。</p>
</body>"""
        self._send_bytes(403, body.encode("utf-8"), "text/html; charset=utf-8")

    # -- routes ----------------------------------------------------------

    def do_GET(self) -> None:
        try:
            if not self._host_ok():
                self._send_text(400, "Bad Host header")
                return
            if not self._check_auth():
                return
            path = urllib.parse.urlsplit(self.path).path
            if path in ("/", "/index.html"):
                if not self._token_ok():
                    self._send_forbidden()
                    return
                self._send_html()
                return
            if path == "/api/health":
                self._send_json({"ok": True})
                return
            if path == "/api/state":
                if not self._token_ok():
                    self._send_json({"ok": False, "error": "unauthorized"}, 403)
                    return
                self._send_json(
                    state_payload(
                        self.server.repo_root,
                        auth_enabled=bool(self.server.username),
                        bind=self.server.bind,
                    )
                )
                return
            if path == "/favicon.ico":
                self.send_response(204)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            self._send_json({"ok": False, "error": "not found"}, 404)
        except Exception as exc:  # pragma: no cover - defensive
            log_error(f"GET {self.path}: {exc}")
            traceback.print_exc()
            self._send_json({"ok": False, "error": str(exc)}, 500)

    def do_POST(self) -> None:
        try:
            if not self._host_ok() or not self._origin_ok():
                self._send_json({"ok": False, "error": "bad origin"}, 403)
                return
            if not self._check_auth():
                return
            if not self._token_ok():
                self._send_json({"ok": False, "error": "unauthorized"}, 403)
                return

            path = urllib.parse.urlsplit(self.path).path
            if path not in ("/api/preview", "/api/deploy", "/api/action", "/api/quit"):
                self._send_json({"ok": False, "error": "not found"}, 404)
                return

            try:
                length = int(self.headers.get("Content-Length", "0"))
            except ValueError:
                length = 0
            if length <= 0 or length > 8 * 1024 * 1024:
                # The body (if any) is left unread, so the connection cannot be
                # reused: otherwise the leftover bytes would be parsed as the
                # next request.
                self.close_connection = True
                self._send_json({"ok": False, "error": "请求体为空或过大"}, 413)
                return
            raw = self.rfile.read(length)
            try:
                payload = json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                self._send_json({"ok": False, "error": "JSON 解析失败"}, 400)
                return
            if not isinstance(payload, dict):
                self._send_json({"ok": False, "error": "请求体必须是 JSON 对象"}, 400)
                return

            if path == "/api/preview":
                self._send_json(preview_payload(payload.get("values", payload), self.server.repo_root))
                return

            if path == "/api/deploy":
                values = payload.get("values", {})
                mode = payload.get("mode", "deploy")
                if mode not in ("deploy", "save", "check"):
                    self._send_json({"ok": False, "error": "mode 不合法"}, 400)
                    return
                with self.server.action_lock:
                    result = deploy(values, self.server.repo_root, mode)
                self._send_json(result)
                return

            if path == "/api/action":
                action = payload.get("action", "")
                if not isinstance(action, str):
                    self._send_json({"ok": False, "error": "action 不合法"}, 400)
                    return
                with self.server.action_lock:
                    result = action_payload(action, payload, self.server.repo_root)
                self._send_json(result)
                return

            if path == "/api/quit":
                self._send_json({"ok": True, "message": "页面服务已关闭"})
                threading.Thread(target=self.server.shutdown, daemon=True).start()
                return
        except Exception as exc:  # pragma: no cover - defensive
            log_error(f"POST {self.path}: {exc}")
            traceback.print_exc()
            self._send_json({"ok": False, "error": str(exc)}, 500)


# ---------------------------------------------------------------------------
# entrypoint
# ---------------------------------------------------------------------------


def display_host(bind: str) -> str:
    if bind in ("0.0.0.0", ""):
        return "127.0.0.1"
    if bind in ("::", "[::]"):
        return "[::1]"
    if ":" in bind and not bind.startswith("["):
        return f"[{bind}]"
    return bind


def interface_ipv4_addresses() -> list[str]:
    """Local IPv4 addresses, without probing any external host."""
    addresses: list[str] = []
    try:
        import fcntl
        import struct
    except ImportError:  # not available on Windows
        return addresses
    try:
        names = [name for _, name in socket.if_nameindex()]
    except (OSError, AttributeError):
        return addresses
    probe = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        for name in names:
            if name == "lo":
                continue
            try:
                packed = fcntl.ioctl(
                    probe.fileno(), 0x8915, struct.pack("256s", name.encode()[:15])
                )
            except OSError:
                continue
            addresses.append(socket.inet_ntoa(packed[20:24]))
    finally:
        probe.close()
    return addresses


def detect_lan_ip() -> str:
    candidates = interface_ipv4_addresses()
    try:
        for info in socket.getaddrinfo(socket.gethostname(), None, socket.AF_INET):
            candidates.append(info[4][0])
    except OSError:
        pass
    for ip in candidates:
        if ip and not ip.startswith("127.") and ip != "0.0.0.0":
            return ip
    return ""


def can_open_browser() -> bool:
    if sys.platform.startswith("linux"):
        return bool(os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY"))
    return True


def open_browser(url: str) -> None:
    try:
        opened = webbrowser.open(url)
    except Exception:
        opened = False
    if not opened:
        print("无法自动打开浏览器，请手动访问上面的地址。", flush=True)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="linkc-node Docker launcher with a local web UI",
        epilog=(
            "Non-loopback binds require --username. Use --bind 127.0.0.1 for token-only "
            "local access. A random password is generated and printed unless --password, "
            "LINKC_LAUNCHER_PASSWORD, or --ask-password is used. Generated credentials "
            "are also written as JSON to a 0600 file unless --no-credentials-file is used. "
            "Use --password-hash-file to read a scrypt hash; --hash-password generates one."
        ),
    )
    parser.add_argument("--port", type=int, default=0, help="listen port (0 = random free port)")
    parser.add_argument(
        "--bind",
        default="0.0.0.0",
        help=(
            "listen address (default: 0.0.0.0; non-loopback requires --username; "
            "use 127.0.0.1 for local-only)"
        ),
    )
    parser.add_argument("--no-browser", action="store_true", help="do not open a browser")
    parser.add_argument(
        "--repo-root",
        default=str(Path(__file__).resolve().parent.parent),
        help="repository root (for local docker build)",
    )
    parser.add_argument(
        "--html",
        default=str(Path(__file__).resolve().parent / "docker-launcher.html"),
        help="path to the launcher HTML file",
    )
    parser.add_argument(
        "--username",
        default=os.environ.get("LINKC_LAUNCHER_USER", ""),
        help=(
            "enable HTTP Basic Auth for the page (required for non-loopback binds; "
            "default: $LINKC_LAUNCHER_USER)"
        ),
    )
    parser.add_argument(
        "--password",
        default=os.environ.get("LINKC_LAUNCHER_PASSWORD", ""),
        help=(
            "HTTP Basic Auth password (default: $LINKC_LAUNCHER_PASSWORD; "
            "otherwise a random password is generated and printed)"
        ),
    )
    parser.add_argument(
        "--ask-password",
        action="store_true",
        help="prompt for the page password instead of generating one",
    )
    parser.add_argument(
        "--credentials-file",
        default=None,
        help=(
            "write generated page credentials as JSON to this 0600 file "
            "(default: ~/.linkc-node/launcher-credentials.json; "
            "env: $LINKC_LAUNCHER_CREDENTIALS_FILE)"
        ),
    )
    parser.add_argument(
        "--no-credentials-file",
        action="store_true",
        help="do not write generated page credentials to disk",
    )
    parser.add_argument(
        "--password-hash-file",
        default=os.environ.get("LINKC_LAUNCHER_PASSWORD_HASH_FILE", ""),
        help=(
            "read the page password hash from this file "
            "(scrypt; JSON or one-line; env: $LINKC_LAUNCHER_PASSWORD_HASH_FILE)"
        ),
    )
    parser.add_argument(
        "--hash-password",
        action="store_true",
        help="generate a scrypt password hash and exit",
    )
    parser.add_argument(
        "--hash-password-output",
        default="",
        help="with --hash-password, write the hash to this 0600 file",
    )
    parser.add_argument("--version", action="version", version=f"%(prog)s {LAUNCHER_VERSION}")
    args = parser.parse_args(argv)

    if sys.version_info < (3, 8):
        print("错误: 需要 Python 3.8 或更新版本。", file=sys.stderr)
        return 1
    if args.hash_password:
        if args.password_hash_file:
            print(
                "错误: --hash-password 不能和 --password-hash-file 同时使用。",
                file=sys.stderr,
            )
            return 1
        return generate_password_hash(args)
    if args.hash_password_output:
        print("错误: --hash-password-output 需要 --hash-password。", file=sys.stderr)
        return 1
    if args.port < 0 or args.port > 65535:
        parser.error("--port 必须在 0..65535 之间")

    html_path = Path(args.html).resolve()
    if not html_path.is_file():
        print(f"错误: 找不到页面文件 {html_path}", file=sys.stderr)
        return 1
    repo_root = Path(args.repo_root).resolve()

    password_hash: PasswordHash | None = None
    password_hash_path = ""
    if args.password_hash_file:
        hash_file = Path(args.password_hash_file).expanduser()
        if not hash_file.is_absolute():
            hash_file = Path.cwd() / hash_file
        password_hash_path = str(Path(os.path.abspath(hash_file)))
        try:
            password_hash = load_password_hash(Path(password_hash_path))
        except ValidationError as exc:
            print(f"错误: {exc}", file=sys.stderr)
            return 1

    username = args.username.strip()
    password = args.password
    password_source = "provided"

    if password_hash is not None:
        if password or args.ask_password:
            print(
                "错误: --password-hash-file 不能和 --password/--ask-password/"
                "LINKC_LAUNCHER_PASSWORD 同时使用。",
                file=sys.stderr,
            )
            return 1
        if not username:
            print(
                "错误: --password-hash-file 需要同时提供 --username。",
                file=sys.stderr,
            )
            return 1
        password = ""
        password_source = "hash"
    else:
        if args.ask_password and password:
            print(
                "错误: --ask-password 不能和 --password / LINKC_LAUNCHER_PASSWORD 同时使用。",
                file=sys.stderr,
            )
            return 1
        if args.ask_password and not username:
            print("错误: --ask-password 需要同时提供 --username。", file=sys.stderr)
            return 1

        if username and not password:
            if args.ask_password:
                try:
                    password = getpass.getpass(f"页面登录密码（{username}）: ")
                except (EOFError, KeyboardInterrupt):
                    print("\n错误: 需要密码才能启用页面登录。", file=sys.stderr)
                    return 1
                password_source = "prompted"
            else:
                password = secrets.token_urlsafe(18)
                password_source = "generated"

        if password and not username:
            print("错误: 设置密码时必须同时提供用户名。", file=sys.stderr)
            return 1

    if password and ("\n" in password or "\r" in password or "\x00" in password):
        print("错误: 密码不能包含换行或 NUL。", file=sys.stderr)
        return 1

    if username:
        if ":" in username:
            print("错误: 用户名不能包含冒号。", file=sys.stderr)
            return 1
        if any(ord(char) < 0x20 for char in username):
            print("错误: 用户名不能包含控制字符。", file=sys.stderr)
            return 1
        if not password and password_hash is None:
            print("错误: 密码不能为空。", file=sys.stderr)
            return 1

    if args.no_credentials_file and args.credentials_file:
        print(
            "错误: --no-credentials-file 不能和 --credentials-file 同时使用。",
            file=sys.stderr,
        )
        return 1

    if args.credentials_file and password_source != "generated":
        print(
            "错误: --credentials-file 只在自动生成密码时生效；"
            "--password / --ask-password / --password-hash-file 不会写入凭据文件。",
            file=sys.stderr,
        )
        return 1

    credentials_path: Path | None = None
    if password_source == "generated" and not args.no_credentials_file:
        raw_credentials = args.credentials_file or os.environ.get(
            "LINKC_LAUNCHER_CREDENTIALS_FILE", ""
        )
        credentials_path = (
            Path(raw_credentials).expanduser()
            if raw_credentials
            else default_credentials_path()
        )
        if not credentials_path.is_absolute():
            credentials_path = Path.cwd() / credentials_path
        credentials_path = Path(os.path.abspath(credentials_path))
        if credentials_path.exists() and credentials_path.is_dir():
            print(
                f"错误: 凭据文件路径是目录: {credentials_path}",
                file=sys.stderr,
            )
            return 1

    loopback_binds = ("127.0.0.1", "::1", "[::1]", "localhost")
    if args.bind not in loopback_binds and not username:
        print(
            f"错误: 监听 {args.bind} 时必须设置页面用户名/密码。",
            file=sys.stderr,
        )
        print(
            "  推荐: ./scripts/docker-launch.sh --username admin",
            file=sys.stderr,
        )
        print(
            "  仅本机: ./scripts/docker-launch.sh --bind 127.0.0.1",
            file=sys.stderr,
        )
        return 1

    if ":" in args.bind:
        LauncherServer.address_family = socket.AF_INET6

    token = secrets.token_urlsafe(24)
    try:
        server = LauncherServer(
            (args.bind, args.port),
            Handler,
            token=token,
            html_path=html_path,
            repo_root=repo_root,
            username=username,
            password=password,
            password_hash=password_hash,
        )
    except OSError as exc:
        print(f"无法监听 {args.bind}:{args.port}: {exc}", file=sys.stderr)
        return 1

    port = server.server_address[1]
    local_url = f"http://{display_host(args.bind)}:{port}/?token={token}"
    lan_url = ""
    if args.bind == "0.0.0.0":
        lan_ip = detect_lan_ip()
        if lan_ip:
            lan_url = f"http://{lan_ip}:{port}/?token={token}"
    url = local_url

    credentials_written = False
    if username and password_source == "generated" and credentials_path is not None:
        try:
            write_credentials_file(
                credentials_path,
                url=local_url,
                lan_url=lan_url,
                username=username,
                password=password,
                listen=args.bind,
                port=port,
            )
            credentials_written = True
        except OSError as exc:
            print(
                f"警告: 无法写入凭据文件 {credentials_path}: {exc}",
                file=sys.stderr,
            )

    defaults = default_values()
    config_dir = Path(defaults["config_dir"])
    data_dir = Path(defaults["data_dir"])
    last = read_last_config_dir()
    if last and (Path(last) / METADATA_FILENAME).is_file():
        config_dir = Path(last)
    metadata, _ = load_metadata(config_dir)
    if metadata:
        config_dir = Path(str(metadata.get("config_dir", config_dir)))
        data_dir = Path(str(metadata.get("data_dir", data_dir)))

    print("=" * 68)
    print(" linkc-node Docker 启动器")
    print("=" * 68)
    print(f" 页面地址 : {local_url}")
    if lan_url:
        print(f" 局域网   : {lan_url}")
    print(f" 配置目录 : {config_dir}")
    print(f" 数据目录 : {data_dir}")
    print(f" 运行身份 : {'root' if os.geteuid() == 0 else f'UID {os.getuid()}:{os.getgid()}'}")
    if username:
        if password_source == "hash":
            print(f" 页面登录 : {username}")
            print(f" 密码哈希 : {password_hash_path}（scrypt）")
        elif password_source == "generated":
            print(f" 页面登录 : {username}")
            print(f" 登录密码 : {password}  （自动生成，只显示这一次）")
            if credentials_path is None:
                print(" 凭据文件 : 未写入（--no-credentials-file）")
            elif credentials_written:
                print(f" 凭据文件 : {credentials_path}（0600）")
            else:
                print(f" 凭据文件 : 写入失败（{credentials_path}），密码见上方")
        elif password_source == "prompted":
            print(f" 页面登录 : {username} / ********（手动输入）")
        else:
            print(f" 页面登录 : {username} / ********（已提供）")
        print(" 登录方式 : 浏览器会弹出 HTTP Basic Auth 用户名/密码框")
    else:
        print(" 页面登录 : 未启用（仅 token）")
    if not shutil.which("docker"):
        print(" 警告     : 未找到 docker 命令；页面仍可生成配置，但无法启动容器。")
    if args.bind not in ("127.0.0.1", "::1", "[::1]", "localhost"):
        if username:
            print(
                f" 提示     : 正在监听 {args.bind}，网络内可访问；"
                "请用防火墙限制来源。Basic Auth 在 HTTP 下不加密。"
            )
        else:
            print(
                f" 警告     : 正在监听 {args.bind}，网络内可访问；"
                "必须设置 --username。"
            )
    print("-" * 68)
    print(" 按 Ctrl+C 结束页面服务；Docker 容器会继续运行。")
    print(" 不要把带 token 的地址发给别人。")
    print("=" * 68, flush=True)

    should_open = not args.no_browser and can_open_browser()
    if os.geteuid() == 0 and os.environ.get("SUDO_USER"):
        print("检测到 sudo：不自动打开浏览器，请手动复制上面的地址。", flush=True)
        should_open = False
    if should_open:
        timer = threading.Timer(0.4, open_browser, args=(url,))
        timer.daemon = True
        timer.start()
    else:
        print("请手动打开上面的地址。", flush=True)

    def shutdown(signum: int, frame: Any) -> None:
        del signum, frame
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    try:
        signal.signal(signal.SIGINT, shutdown)
    except ValueError:
        pass

    try:
        server.serve_forever()
    finally:
        server.server_close()
        print("\n页面服务已退出；Docker 容器不受影响。", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
