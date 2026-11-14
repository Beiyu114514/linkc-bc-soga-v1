#!/usr/bin/env python3
"""Unit tests for the Docker launcher (no docker daemon or root required).

Run with:  python3 -m unittest discover -s scripts -p 'test_*.py' -v
"""

from __future__ import annotations

import hashlib
import importlib.util
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent


def load_launcher():
    spec = importlib.util.spec_from_file_location("docker_launcher", HERE / "docker_launcher.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


dl = load_launcher()

BASE = {
    "base_url": "https://panel.example.com",
    "api_key": "secret-key",
    "node_id": "n1",
}


def values_with(tmp: Path, **overrides):
    raw = dict(BASE)
    raw["config_dir"] = str(tmp / "etc")
    raw["data_dir"] = str(tmp / "data")
    raw.update(overrides)
    return raw


class FakeHeaders(dict):
    def get(self, key, default=None):
        return dict.get(self, key, default)


class FakeServer:
    def __init__(self, **overrides):
        self.token = "tok"
        self.username = ""
        self.password = ""
        self.password_hash = None
        self.auth_cache = set()
        self.auth_cache_key = b"k" * 32
        self.auth_lock = threading.Lock()
        self.auth_failures = {}
        self.auth_verify_slots = threading.BoundedSemaphore(dl.AUTH_MAX_CONCURRENT_VERIFY)
        for key, value in overrides.items():
            setattr(self, key, value)


def make_handler(*, path="/", headers=None, server=None, client=("127.0.0.1", 1234)):
    handler = object.__new__(dl.Handler)
    handler.path = path
    handler.headers = FakeHeaders(headers or {})
    handler.client_address = client
    handler.server = server or FakeServer()
    handler.close_connection = False
    handler.responses = []

    def send_bytes(status, body, content_type, extra_headers=None):
        handler.responses.append((status, body))

    handler._send_bytes = send_bytes
    return handler


def basic(user: str, password: str) -> str:
    import base64

    token = base64.b64encode(f"{user}:{password}".encode("utf-8")).decode("ascii")
    return f"Basic {token}"


class NormalizeValuesTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())

    def test_accepts_valid_values(self):
        values = dl.normalize_values(values_with(self.tmp))
        self.assertEqual(values["profile"], dl.DEFAULT_PROFILE)
        self.assertEqual(values["max_response_mb"], 32)

    def test_rejects_http_without_allow(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(values_with(self.tmp, base_url="http://panel.example.com"))

    def test_accepts_http_when_allowed(self):
        values = dl.normalize_values(
            values_with(self.tmp, base_url="http://panel.example.com", allow_http=True)
        )
        self.assertTrue(values["allow_http"])

    def test_rejects_env_expansion(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(values_with(self.tmp, node_id="${HOME}"))

    def test_rejects_newline_in_api_key(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(values_with(self.tmp, api_key="a\nb"))

    def test_rejects_traversal(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(values_with(self.tmp, config_dir=str(self.tmp / "a" / ".." / "b")))

    def test_rejects_system_dirs(self):
        for path in ("/etc", "/var", "/var/lib", "/usr/local", "/home", "/root"):
            with self.subTest(path=path):
                with self.assertRaises(dl.ValidationError):
                    dl.normalize_values(values_with(self.tmp, config_dir=path))

    def test_rejects_nested_dirs(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(values_with(self.tmp, data_dir=str(self.tmp / "etc" / "sub")))

    def test_rejects_bad_listen(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(values_with(self.tmp, listen="not-an-ip"))

    def test_rejects_image_traversal(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(values_with(self.tmp, image="../../etc/passwd"))

    def test_rejects_zero_duration(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(values_with(self.tmp, pull_interval="0s"))

    def test_rejects_url_userinfo_and_query(self):
        for url in ("https://u:p@panel.example.com", "https://panel.example.com/?a=1"):
            with self.subTest(url=url):
                with self.assertRaises(dl.ValidationError):
                    dl.normalize_values(values_with(self.tmp, base_url=url))

    def test_rejects_cert_without_key(self):
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(
                values_with(self.tmp, cert_pem="-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----")
            )


class BuildConfigTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.values = dl.normalize_values(values_with(self.tmp))

    def test_api_key_never_in_config(self):
        config = dl.build_config(self.values, {"has_certs": False})
        self.assertNotIn("secret-key", config)
        self.assertIn('api_key_file: "/etc/linkc-node/api-key"', config)

    def test_certificates_follow_existing_state(self):
        without = dl.build_config(self.values, {"has_certs": False})
        self.assertIn("certificates: []", without)
        with_certs = dl.build_config(self.values, {"has_certs": True})
        self.assertIn("fullchain.pem", with_certs)
        cleared = dl.normalize_values(values_with(self.tmp, clear_certs=True))
        self.assertIn("certificates: []", dl.build_config(cleared, {"has_certs": True}))

    def test_whitelist_file_reference(self):
        values = dl.normalize_values(values_with(self.tmp, whitelist="example.com\n"))
        config = dl.build_config(values, {"has_certs": False})
        self.assertIn('whitelist_file: "/etc/linkc-node/whitelist.txt"', config)

    def test_geodata_dir_is_container_path(self):
        values = dl.normalize_values(values_with(self.tmp, geodata_dir=str(self.tmp)))
        config = dl.build_config(values, {"has_certs": False})
        self.assertIn('dir: "/usr/share/xray"', config)


class DockerCommandTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.values = dl.normalize_values(values_with(self.tmp))
        self.existing = {"has_certs": False}

    def test_check_command_is_isolated(self):
        command = dl.docker_run_command(self.values, self.existing, check=True)
        self.assertIn("--network", command)
        self.assertIn("none", command)
        self.assertIn("--rm", command)
        self.assertIn("check", command)
        self.assertNotIn("--name", command)
        joined = " ".join(command)
        self.assertNotIn(str(self.tmp / "data"), joined)

    def test_deploy_command_mounts_data_and_labels(self):
        command = dl.docker_run_command(self.values, self.existing, check=False)
        joined = " ".join(command)
        self.assertIn("--network host", joined)
        self.assertIn(f"{dl.MANAGED_LABEL}={dl.MANAGED_VALUE}", joined)
        self.assertIn(f"{self.tmp / 'etc'}:/etc/linkc-node:ro", joined)
        self.assertIn(f"{self.tmp / 'data'}:/var/lib/linkc-node", joined)
        self.assertIn("no-new-privileges:true", joined)
        self.assertNotIn("secret-key", joined)

    def test_run_script_has_no_secret(self):
        command = dl.docker_run_command(self.values, self.existing, check=False)
        script = dl.build_run_script(self.values, command, Path("/repo"))
        self.assertIn("set -euo pipefail", script)
        self.assertNotIn("secret-key", script)


class EnsureOwnedDirTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())

    def test_creates_missing_directory(self):
        target = self.tmp / "new"
        dl.ensure_owned_dir(target, dl.CONFIG_DIR_ALLOWED, "配置目录")
        self.assertTrue(target.is_dir())
        self.assertEqual(target.stat().st_mode & 0o777, 0o700)

    def test_accepts_managed_entries(self):
        target = self.tmp / "etc"
        target.mkdir()
        (target / "config.yaml").write_text("x")
        (target / "certs").mkdir()
        dl.ensure_owned_dir(target, dl.CONFIG_DIR_ALLOWED, "配置目录")
        self.assertEqual(target.stat().st_mode & 0o777, 0o700)

    def test_refuses_unmanaged_entries(self):
        target = self.tmp / "var-lib"
        target.mkdir()
        (target / "docker").mkdir()
        with self.assertRaises(dl.ValidationError):
            dl.ensure_owned_dir(target, dl.DATA_DIR_ALLOWED, "数据目录")
        # The directory must be left untouched.
        self.assertNotEqual(target.stat().st_mode & 0o777, 0o700)

    def test_prepare_files_refuses_unmanaged_data_dir(self):
        data = self.tmp / "data"
        data.mkdir()
        (data / "docker").mkdir()
        values = dl.normalize_values(values_with(self.tmp))
        with mock.patch.object(dl, "last_config_dir_path", lambda: self.tmp / "last"):
            with self.assertRaises(dl.ValidationError):
                dl.prepare_files(values, {"has_api_key": False, "has_certs": False}, Path("/repo"))

    def test_prepare_files_writes_private_files(self):
        values = dl.normalize_values(values_with(self.tmp))
        with mock.patch.object(dl, "last_config_dir_path", lambda: self.tmp / "last"):
            paths = dl.prepare_files(values, {"has_api_key": False, "has_certs": False}, Path("/repo"))
        config_path = Path(paths["config_path"])
        api_key_path = Path(paths["api_key_path"])
        self.assertEqual(config_path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(api_key_path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(api_key_path.read_text().strip(), "secret-key")
        self.assertNotIn("secret-key", config_path.read_text())
        self.assertTrue(Path(paths["run_script_path"]).is_file())


class StageConfigTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.values = dl.normalize_values(values_with(self.tmp))

    def test_stages_a_complete_config_dir(self):
        target = Path(tempfile.mkdtemp())
        dl.stage_config(self.values, {"has_certs": False}, target)
        self.assertTrue((target / "config.yaml").is_file())
        self.assertEqual((target / "api-key").read_text().strip(), "secret-key")
        self.assertEqual((target / "api-key").stat().st_mode & 0o777, 0o600)
        self.assertTrue((target / "certs").is_dir())

    def test_reuses_existing_api_key(self):
        config_dir = Path(self.values["config_dir"])
        config_dir.mkdir(parents=True)
        (config_dir / "api-key").write_text("existing-key\n")
        values = dl.normalize_values(values_with(self.tmp, api_key=""))
        target = Path(tempfile.mkdtemp())
        dl.stage_config(values, {"has_certs": False}, target)
        self.assertEqual((target / "api-key").read_text().strip(), "existing-key")


class WaitContainerStartTest(unittest.TestCase):
    def test_success(self):
        states = [{"exists": True, "status": "running", "restart_count": 0}]
        with mock.patch.object(dl, "container_state", lambda name: states[0]):
            state, ok = dl.wait_container_start("x", stable=0.0, grace=0.1)
        self.assertTrue(ok)
        self.assertEqual(state["status"], "running")

    def test_detects_restart_loop(self):
        states = [{"exists": True, "status": "running", "restart_count": 2}]
        with mock.patch.object(dl, "container_state", lambda name: states[0]):
            state, ok = dl.wait_container_start("x", stable=0.0, grace=0.1)
        self.assertFalse(ok)
        self.assertEqual(state["restart_count"], 2)

    def test_detects_exited(self):
        states = [{"exists": True, "status": "exited", "restart_count": 0}]
        with mock.patch.object(dl, "container_state", lambda name: states[0]):
            _, ok = dl.wait_container_start("x", stable=0.0, grace=0.1)
        self.assertFalse(ok)


class HandlerSecurityTest(unittest.TestCase):
    def test_host_check_allows_ip_and_localhost(self):
        for host in ("127.0.0.1:8080", "192.168.1.10:8080", "localhost:8080", "[::1]:8080"):
            with self.subTest(host=host):
                handler = make_handler(headers={"Host": host})
                self.assertTrue(handler._host_ok())

    def test_host_check_rejects_dns_names(self):
        handler = make_handler(headers={"Host": "evil.example.com"})
        self.assertFalse(handler._host_ok())

    def test_origin_check(self):
        handler = make_handler(headers={"Host": "127.0.0.1:8080"})
        self.assertTrue(handler._origin_ok())
        handler = make_handler(
            headers={"Host": "127.0.0.1:8080", "Origin": "http://evil.example.com"}
        )
        self.assertFalse(handler._origin_ok())

    def test_token_check(self):
        handler = make_handler(path="/?token=tok")
        self.assertTrue(handler._token_ok())
        handler = make_handler(path="/?token=nope")
        self.assertFalse(handler._token_ok())
        handler = make_handler(headers={"X-Launcher-Token": "tok"})
        self.assertTrue(handler._token_ok())

    def test_auth_disabled_accepts(self):
        handler = make_handler()
        self.assertTrue(handler._check_auth())
        self.assertEqual(handler.responses, [])

    def test_auth_requires_credentials(self):
        server = FakeServer(username="admin", password="pw")
        handler = make_handler(server=server)
        self.assertFalse(handler._check_auth())
        self.assertEqual(handler.responses[-1][0], 401)

        handler = make_handler(server=server, headers={"Authorization": basic("admin", "pw")})
        self.assertTrue(handler._check_auth())

        handler = make_handler(server=server, headers={"Authorization": basic("admin", "bad")})
        self.assertFalse(handler._check_auth())

    def test_auth_rate_limit(self):
        server = FakeServer(username="admin", password="pw")
        for _ in range(dl.AUTH_MAX_FAILURES):
            handler = make_handler(server=server, headers={"Authorization": basic("admin", "bad")})
            self.assertFalse(handler._check_auth())
            self.assertEqual(handler.responses[-1][0], 401)
        handler = make_handler(server=server, headers={"Authorization": basic("admin", "bad")})
        self.assertFalse(handler._check_auth())
        self.assertEqual(handler.responses[-1][0], 429)

    def test_auth_with_scrypt_hash(self):
        salt = b"s" * 16
        digest = hashlib.scrypt(
            b"pw", salt=salt, n=1024, r=8, p=1, dklen=32, maxmem=dl.SCRYPT_MAXMEM
        )
        password_hash = dl.PasswordHash(1024, 8, 1, salt, digest)
        server = FakeServer(username="admin", password_hash=password_hash)
        handler = make_handler(server=server, headers={"Authorization": basic("admin", "pw")})
        self.assertTrue(handler._check_auth())
        handler = make_handler(server=server, headers={"Authorization": basic("admin", "bad")})
        self.assertFalse(handler._check_auth())


class PasswordHashFileTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())

    def test_reads_one_line_format(self):
        salt = b"saltsalt"
        digest = hashlib.scrypt(b"pw", salt=salt, n=1024, r=8, p=1, dklen=32, maxmem=dl.SCRYPT_MAXMEM)
        import base64

        line = "scrypt$1024$8$1${}${}".format(
            base64.b64encode(salt).decode(), base64.b64encode(digest).decode()
        )
        path = self.tmp / "password.hash"
        path.write_text(line + "\n")
        parsed = dl.load_password_hash(path)
        self.assertTrue(parsed.verify("pw"))
        self.assertFalse(parsed.verify("nope"))

    def test_rejects_bad_params(self):
        import base64

        line = "scrypt$3$8$1${}${}".format(
            base64.b64encode(b"saltsalt").decode(),
            base64.b64encode(b"x" * 32).decode(),
        )
        path = self.tmp / "password.hash"
        path.write_text(line)
        with self.assertRaises(dl.ValidationError):
            dl.load_password_hash(path)


class DetectLanIpTest(unittest.TestCase):
    def test_never_returns_loopback(self):
        ip = dl.detect_lan_ip()
        self.assertIsInstance(ip, str)
        self.assertFalse(ip.startswith("127."))
        self.assertNotEqual(ip, "0.0.0.0")


if __name__ == "__main__":
    unittest.main()
