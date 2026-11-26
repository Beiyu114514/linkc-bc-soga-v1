#!/usr/bin/env python3
"""Unit tests for the Docker launcher (no docker daemon or root required).

Run with:  python3 -m unittest discover -s scripts -p 'test_*.py' -v
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import socket
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
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
        self.assertNotIn("no-new-privileges:true", joined)
        self.assertNotIn("secret-key", joined)

    def test_no_new_privileges_is_opt_in(self):
        values = dl.normalize_values(values_with(self.tmp, no_new_privileges=True))
        joined = " ".join(dl.docker_run_command(values, self.existing, check=False))
        self.assertIn("no-new-privileges:true", joined)

    def test_run_script_has_no_secret(self):
        command = dl.docker_run_command(self.values, self.existing, check=False)
        script = dl.build_run_script(self.values, command, Path("/repo"))
        self.assertIn("set -euo pipefail", script)
        self.assertNotIn("secret-key", script)


class WarningTest(unittest.TestCase):
    def test_no_new_privileges_warns_about_low_ports(self):
        tmp = Path(tempfile.mkdtemp())
        values = dl.normalize_values(values_with(tmp, no_new_privileges=True))
        existing = dl.existing_state(values)
        with mock.patch.object(dl, "container_state", lambda name: {"exists": False}):
            warnings = dl.collect_warnings(values, existing, Path("."))
        self.assertTrue(any("no-new-privileges" in w["text"] for w in warnings))


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
        run_script_path = Path(paths["run_script_path"])
        expected_cfg_mode = 0o640 if os.geteuid() == 0 else 0o600
        expected_run_mode = 0o700 if os.geteuid() == 0 else 0o755
        self.assertEqual(config_path.stat().st_mode & 0o777, expected_cfg_mode)
        self.assertEqual(api_key_path.stat().st_mode & 0o777, expected_cfg_mode)
        self.assertEqual(run_script_path.stat().st_mode & 0o777, expected_run_mode)
        if os.geteuid() == 0:
            self.assertEqual(config_path.stat().st_uid, 0)
            self.assertEqual(config_path.stat().st_gid, 10001)
            self.assertEqual(run_script_path.stat().st_uid, 0)
            self.assertEqual(run_script_path.stat().st_gid, 0)
        self.assertEqual(api_key_path.read_text().strip(), "secret-key")
        self.assertNotIn("secret-key", config_path.read_text())
        self.assertTrue(run_script_path.is_file())


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


class CliAndUnixSocketTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())

    def test_preview_and_stage_cli(self):
        cfg_dir = self.tmp / "etc"
        data_dir = self.tmp / "data"
        buf = io.StringIO()
        with mock.patch.dict(os.environ, {"TEST_PANEL_KEY": "env-secret-key"}), \
             mock.patch.object(dl, "last_config_dir_path", lambda: self.tmp / "last"), \
             mock.patch.object(dl, "docker_state", lambda: {"ok": False, "error": "no docker"}), \
             contextlib.redirect_stdout(buf):
            rc = dl.main([
                "preview",
                "--fresh",
                "--panel-url", "https://panel.example.com",
                "--node-id", "42",
                "--api-key-env", "TEST_PANEL_KEY",
                "--config-dir", str(cfg_dir),
                "--data-dir", str(data_dir),
                "--json",
            ])
        self.assertEqual(rc, 0)
        out = json.loads(buf.getvalue())
        self.assertTrue(out["ok"])
        self.assertIn("42", out["config"])
        self.assertNotIn("env-secret-key", out["config"])
        self.assertFalse(cfg_dir.exists())

        buf = io.StringIO()
        with mock.patch.dict(os.environ, {"TEST_PANEL_KEY": "env-secret-key"}), \
             mock.patch.object(dl, "last_config_dir_path", lambda: self.tmp / "last"), \
             mock.patch.object(dl, "docker_state", lambda: {"ok": False, "error": "no docker"}), \
             contextlib.redirect_stdout(buf):
            rc = dl.main([
                "stage",
                "--fresh",
                "--panel-url", "https://panel.example.com",
                "--node-id", "42",
                "--api-key-env", "TEST_PANEL_KEY",
                "--config-dir", str(cfg_dir),
                "--data-dir", str(data_dir),
                "--json",
            ])
        self.assertEqual(rc, 0)
        staged = json.loads(buf.getvalue())
        self.assertTrue(staged["ok"])
        self.assertEqual((cfg_dir / "api-key").read_text().strip(), "env-secret-key")
        self.assertTrue((cfg_dir / "config.yaml").is_file())
        self.assertTrue((cfg_dir / "docker-run.sh").is_file())

    def test_deploy_dry_run_and_config_json(self):
        cfg_json = self.tmp / "opts.json"
        cfg_json.write_text(json.dumps({
            "base_url": "https://panel.example.com",
            "node_id": "7",
            "api_key": "json-key",
            "config_dir": str(self.tmp / "etc"),
            "data_dir": str(self.tmp / "data"),
            "listen": "localhost",
        }))
        buf = io.StringIO()
        with mock.patch.object(dl, "last_config_dir_path", lambda: self.tmp / "last"), \
             mock.patch.object(dl, "docker_state", lambda: {"ok": False, "error": "no docker"}), \
             contextlib.redirect_stdout(buf):
            rc = dl.main(["deploy", "--fresh", "--config-json", str(cfg_json), "--dry-run", "--json"])
        self.assertEqual(rc, 0)
        out = json.loads(buf.getvalue())
        self.assertTrue(out["ok"])
        self.assertIn('listen: "127.0.0.1"', out["config"])
        self.assertFalse((self.tmp / "etc" / "config.yaml").exists())

    def test_cert_file_mismatch_and_status_cli(self):
        cert = self.tmp / "cert.pem"
        cert.write_text("CERT")
        err_buf = io.StringIO()
        with mock.patch.object(dl, "last_config_dir_path", lambda: self.tmp / "last"), \
             contextlib.redirect_stderr(err_buf):
            rc = dl.main(["preview", "--fresh", "--cert-file", str(cert)])
        self.assertEqual(rc, 1)
        self.assertIn("--cert-file", err_buf.getvalue())

        buf = io.StringIO()
        with mock.patch.object(dl, "last_config_dir_path", lambda: self.tmp / "last"), \
             mock.patch.object(dl, "docker_state", lambda: {"ok": False, "error": "no docker"}), \
             contextlib.redirect_stdout(buf):
            rc = dl.main(["status", "--config-dir", str(self.tmp / "etc"), "--json"])
        self.assertEqual(rc, 0)
        st = json.loads(buf.getvalue())
        self.assertTrue(st["ok"])
        self.assertNotIn("auth_enabled", st)

    def test_node_status_unix_socket_cli(self):
        sock_path = str(self.tmp / "admin.sock")

        class _UnixServer(HTTPServer):
            address_family = socket.AF_UNIX

        class _Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path == "/status":
                    body = b'{"ready":true,"users":5}'
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                else:
                    body = b"not ready\n"
                    self.send_response(503)
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)

            def log_message(self, fmt, *args):
                del fmt, args

        srv = _UnixServer(sock_path, _Handler)
        thread = threading.Thread(target=srv.serve_forever, daemon=True)
        thread.start()
        try:
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                rc = dl.main(["node-status", "-s", sock_path])
            self.assertEqual(rc, 0)
            self.assertEqual(json.loads(buf.getvalue()), {"ready": True, "users": 5})

            err_buf = io.StringIO()
            with contextlib.redirect_stderr(err_buf):
                rc = dl.main(["node-ready", "-s", sock_path])
            self.assertEqual(rc, 1)
            self.assertIn("503", err_buf.getvalue())
        finally:
            srv.shutdown()
            srv.server_close()


class RestoreBackupFilesTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.config_dir = self.tmp / "etc"
        self.config_dir.mkdir()
        self.backup = self.tmp / "backup"
        self.backup.mkdir()
        for name, content in {
            "config.yaml": "old: true\n",
            "api-key": "old-key\n",
            "whitelist.txt": "old.example\n",
            "launcher.json": "{}\n",
            "docker-run.sh": "#!/bin/sh\n",
            "fullchain.pem": "CERT\n",
            "privkey.pem": "KEY\n",
        }.items():
            (self.backup / name).write_text(content)
        (self.config_dir / "certs").mkdir()
        for name, content in {
            "config.yaml": "new: true\n",
            "api-key": "new-key\n",
            "whitelist.txt": "new.example\n",
            "launcher.json": '{"new": true}\n',
            "docker-run.sh": "#!/bin/sh\n# new\n",
        }.items():
            (self.config_dir / name).write_text(content)
        (self.config_dir / "certs" / "fullchain.pem").write_text("NEWCERT\n")
        (self.config_dir / "certs" / "privkey.pem").write_text("NEWKEY\n")

    def test_restores_all_managed_files(self):
        steps = dl.restore_backup_files(str(self.backup), str(self.config_dir))
        self.assertTrue(steps[0]["ok"], steps[0])
        self.assertEqual((self.config_dir / "config.yaml").read_text(), "old: true\n")
        self.assertEqual((self.config_dir / "api-key").read_text(), "old-key\n")
        self.assertEqual((self.config_dir / "whitelist.txt").read_text(), "old.example\n")
        self.assertEqual((self.config_dir / "launcher.json").read_text(), "{}\n")
        self.assertEqual((self.config_dir / "docker-run.sh").read_text(), "#!/bin/sh\n")
        self.assertEqual(
            (self.config_dir / "certs" / "fullchain.pem").read_text(), "CERT\n"
        )
        self.assertEqual((self.config_dir / "certs" / "privkey.pem").read_text(), "KEY\n")
        expected_cfg_mode = 0o640 if os.geteuid() == 0 else 0o600
        expected_run_mode = 0o700 if os.geteuid() == 0 else 0o755
        self.assertEqual((self.config_dir / "config.yaml").stat().st_mode & 0o777, expected_cfg_mode)
        self.assertEqual((self.config_dir / "docker-run.sh").stat().st_mode & 0o777, expected_run_mode)
        if os.geteuid() == 0:
            self.assertEqual((self.config_dir / "docker-run.sh").stat().st_uid, 0)
            self.assertEqual((self.config_dir / "docker-run.sh").stat().st_gid, 0)

    def test_removes_files_that_did_not_exist_before(self):
        (self.backup / "whitelist.txt").unlink()
        (self.backup / "fullchain.pem").unlink()
        (self.backup / "privkey.pem").unlink()
        steps = dl.restore_backup_files(str(self.backup), str(self.config_dir))
        self.assertTrue(steps[0]["ok"], steps[0])
        self.assertFalse((self.config_dir / "whitelist.txt").exists())
        self.assertFalse((self.config_dir / "certs" / "fullchain.pem").exists())
        self.assertFalse((self.config_dir / "certs" / "privkey.pem").exists())

    def test_reports_backup_failure(self):
        steps = dl.restore_backup_files("", str(self.config_dir), "disk full")
        self.assertFalse(steps[0]["ok"])
        self.assertIn("disk full", steps[0]["detail"])

    def test_no_backup_is_a_noop(self):
        self.assertEqual(dl.restore_backup_files("", str(self.config_dir)), [])


class DeployPreflightTest(unittest.TestCase):
    def test_unmanaged_container_is_refused_before_writing(self):
        tmp = Path(tempfile.mkdtemp())
        raw = values_with(tmp, run_check=False, pull_latest=False)
        with mock.patch.object(dl, "docker_state", lambda: {"ok": True}), \
             mock.patch.object(
                 dl,
                 "container_state",
                 lambda name: {"exists": True, "managed": False},
             ), \
             mock.patch.object(dl, "ensure_image") as ensure_image:
            result = dl.deploy(raw, Path("/repo"), mode="deploy")
        self.assertFalse(result["ok"])
        self.assertIn("不是由本启动器创建", result["error"])
        self.assertFalse((tmp / "etc" / "config.yaml").exists())
        ensure_image.assert_not_called()


class ReplaceContainerRollbackTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.values = dl.normalize_values(values_with(self.tmp))
        self.config_dir = Path(self.values["config_dir"])
        self.config_dir.mkdir(parents=True)
        (self.config_dir / "config.yaml").write_text("new: true\n")
        self.backup = self.tmp / "backup"
        self.backup.mkdir()
        (self.backup / "config.yaml").write_text("old: true\n")

    def test_failed_start_restores_config_before_old_container(self):
        def fake_run_cmd(args, **kwargs):
            return {
                "code": 0,
                "args": args,
                "output": "",
                "stdout": "",
                "stderr": "",
                "duration": 0.0,
            }

        steps = []
        with mock.patch.object(
            dl,
            "container_state",
            lambda name: {"exists": True, "managed": True, "running": True},
        ), mock.patch.object(dl, "run_cmd", fake_run_cmd), mock.patch.object(
            dl,
            "wait_container_start",
            lambda name: ({"status": "exited", "restart_count": 0}, False),
        ):
            with self.assertRaises(dl.DeploymentError):
                dl.replace_container(
                    self.values,
                    {"has_certs": False},
                    steps,
                    backup_dir=str(self.backup),
                )

        self.assertEqual((self.config_dir / "config.yaml").read_text(), "old: true\n")
        names = [step["name"] for step in steps]
        self.assertIn("恢复旧配置", names)
        self.assertIn("回滚容器名", names)
        self.assertIn("回滚启动旧容器", names)
        self.assertLess(names.index("恢复旧配置"), names.index("回滚容器名"))
        self.assertLess(names.index("回滚容器名"), names.index("回滚启动旧容器"))


class SecurityHardeningTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())

    def test_ensure_owned_dir_rejects_symlinks(self):
        real = self.tmp / "real"
        real.mkdir()
        link = self.tmp / "link"
        link.symlink_to(real)
        with self.assertRaises(dl.ValidationError):
            dl.ensure_owned_dir(link, dl.CONFIG_DIR_ALLOWED, "配置目录")

        parent = self.tmp / "dir"
        parent.mkdir()
        (parent / "config.yaml").symlink_to(self.tmp / "elsewhere")
        with self.assertRaises(dl.ValidationError):
            dl.ensure_owned_dir(parent, dl.CONFIG_DIR_ALLOWED, "配置目录")

    def test_rejects_colon_in_mount_paths(self):
        raw = values_with(self.tmp)
        raw["config_dir"] = str(self.tmp / "etc:evil")
        with self.assertRaises(dl.ValidationError):
            dl.normalize_values(raw)
        with self.assertRaises(dl.ValidationError):
            dl.volume_arg("/tmp/a:b", "/etc/linkc-node", read_only=True, values={"selinux_relabel": False})

    def test_localhost_listen_normalizes_to_loopback_ip(self):
        raw = values_with(self.tmp)
        raw["listen"] = "localhost"
        values = dl.normalize_values(raw)
        self.assertEqual(values["listen"], "127.0.0.1")


if __name__ == "__main__":
    unittest.main()
