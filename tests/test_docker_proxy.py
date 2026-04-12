#!/usr/bin/env python3
"""Tests for docker-proxy: volume mount filtering and API inspection."""

import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from threading import Thread

# Add bin/ to path
sys.path.insert(0, str(Path(__file__).parent.parent / "bin"))

# Import proxy functions directly
# We can't import the module (it's a script), so we exec it
_proxy_code = (Path(__file__).parent.parent / "bin" / "docker-proxy").read_text()
_proxy_ns = {}
exec(compile(_proxy_code, "docker-proxy", "exec"), _proxy_ns)

resolve_path = _proxy_ns["resolve_path"]
is_path_allowed = _proxy_ns["is_path_allowed"]
check_create = _proxy_ns["check_create"]


class TestPathAllowed(unittest.TestCase):
    """Test is_path_allowed()."""

    def test_workspace_root(self):
        self.assertTrue(is_path_allowed("/Users/Workspaces/acm", "acm"))

    def test_workspace_subdir(self):
        self.assertTrue(is_path_allowed("/Users/Workspaces/acm/project/src", "acm"))

    def test_tmp(self):
        self.assertTrue(is_path_allowed("/tmp", "acm"))

    def test_tmp_subdir(self):
        self.assertTrue(is_path_allowed("/tmp/build-cache/layer1", "acm"))

    def test_admin_home_blocked(self):
        self.assertFalse(is_path_allowed("/Users/bvt", "acm"))

    def test_admin_ssh_blocked(self):
        self.assertFalse(is_path_allowed("/Users/bvt/.ssh", "acm"))

    def test_admin_aws_blocked(self):
        self.assertFalse(is_path_allowed("/Users/bvt/.aws/credentials", "acm"))

    def test_other_user_workspace_blocked(self):
        self.assertFalse(is_path_allowed("/Users/Workspaces/click", "acm"))

    def test_other_user_workspace_subdir_blocked(self):
        self.assertFalse(is_path_allowed("/Users/Workspaces/click/src", "acm"))

    def test_etc_blocked(self):
        self.assertFalse(is_path_allowed("/etc/passwd", "acm"))

    def test_root_blocked(self):
        self.assertFalse(is_path_allowed("/", "acm"))

    def test_var_run_blocked(self):
        self.assertFalse(is_path_allowed("/var/run/docker.sock", "acm"))

    def test_workspace_prefix_attack(self):
        """Ensure /Users/Workspaces/acm-evil doesn't match acm."""
        self.assertFalse(is_path_allowed("/Users/Workspaces/acm-evil", "acm"))

    def test_workspace_prefix_attack_subdir(self):
        self.assertFalse(is_path_allowed("/Users/Workspaces/acm-evil/src", "acm"))


class TestCheckCreate(unittest.TestCase):
    """Test check_create() container create inspection."""

    def _make_request(self, **host_config):
        return json.dumps({
            "Image": "alpine",
            "Cmd": ["echo", "test"],
            "HostConfig": host_config,
        }).encode()

    def test_clean_create(self):
        ok, _ = check_create(self._make_request(), "acm")
        self.assertTrue(ok)

    def test_allowed_bind(self):
        ok, _ = check_create(
            self._make_request(Binds=["/Users/Workspaces/acm/src:/app"]),
            "acm")
        self.assertTrue(ok)

    def test_allowed_tmp_bind(self):
        ok, _ = check_create(
            self._make_request(Binds=["/tmp/cache:/cache:ro"]),
            "acm")
        self.assertTrue(ok)

    def test_allowed_named_volume(self):
        ok, _ = check_create(
            self._make_request(Binds=["myvolume:/data"]),
            "acm")
        self.assertTrue(ok)

    def test_blocked_admin_home(self):
        ok, reason = check_create(
            self._make_request(Binds=["/Users/bvt:/mnt"]),
            "acm")
        self.assertFalse(ok)
        self.assertIn("bind", reason)

    def test_blocked_admin_ssh(self):
        ok, reason = check_create(
            self._make_request(Binds=["/Users/bvt/.ssh:/mnt"]),
            "acm")
        self.assertFalse(ok)

    def test_blocked_other_workspace(self):
        ok, reason = check_create(
            self._make_request(Binds=["/Users/Workspaces/click:/mnt"]),
            "acm")
        self.assertFalse(ok)

    def test_blocked_etc(self):
        ok, reason = check_create(
            self._make_request(Binds=["/etc/passwd:/mnt:ro"]),
            "acm")
        self.assertFalse(ok)

    def test_blocked_privileged(self):
        ok, reason = check_create(
            self._make_request(Privileged=True),
            "acm")
        self.assertFalse(ok)
        self.assertIn("privileged", reason)

    def test_blocked_host_network(self):
        ok, reason = check_create(
            self._make_request(NetworkMode="host"),
            "acm")
        self.assertFalse(ok)
        self.assertIn("host", reason)

    def test_blocked_host_pid(self):
        ok, reason = check_create(
            self._make_request(PidMode="host"),
            "acm")
        self.assertFalse(ok)
        self.assertIn("pid", reason.lower())

    def test_blocked_volumes_from(self):
        ok, reason = check_create(
            self._make_request(VolumesFrom=["other_container"]),
            "acm")
        self.assertFalse(ok)
        self.assertIn("volumes-from", reason)

    def test_blocked_devices(self):
        ok, reason = check_create(
            self._make_request(Devices=[{"PathOnHost": "/dev/sda"}]),
            "acm")
        self.assertFalse(ok)
        self.assertIn("device", reason)

    def test_blocked_docker_socket_mount(self):
        ok, reason = check_create(
            self._make_request(Binds=["/var/run/docker.sock:/var/run/docker.sock"]),
            "acm")
        self.assertFalse(ok)
        self.assertIn("docker.sock", reason)

    def test_blocked_mount_type_bind(self):
        ok, reason = check_create(json.dumps({
            "Image": "alpine",
            "HostConfig": {
                "Mounts": [{"Type": "bind", "Source": "/Users/bvt", "Target": "/mnt"}]
            }
        }).encode(), "acm")
        self.assertFalse(ok)

    def test_allowed_mount_type_bind_workspace(self):
        ok, _ = check_create(json.dumps({
            "Image": "alpine",
            "HostConfig": {
                "Mounts": [{"Type": "bind", "Source": "/Users/Workspaces/acm/src", "Target": "/app"}]
            }
        }).encode(), "acm")
        self.assertTrue(ok)

    def test_multiple_binds_one_bad(self):
        ok, reason = check_create(
            self._make_request(Binds=[
                "/Users/Workspaces/acm/src:/app",
                "/Users/bvt/.aws:/secrets",
            ]),
            "acm")
        self.assertFalse(ok)

    def test_invalid_json(self):
        ok, reason = check_create(b"not json", "acm")
        self.assertFalse(ok)
        self.assertIn("invalid", reason)

    def test_empty_host_config(self):
        ok, _ = check_create(json.dumps({"Image": "alpine"}).encode(), "acm")
        self.assertTrue(ok)

    def test_bridge_network_allowed(self):
        ok, _ = check_create(
            self._make_request(NetworkMode="bridge"),
            "acm")
        self.assertTrue(ok)

    def test_custom_network_allowed(self):
        ok, _ = check_create(
            self._make_request(NetworkMode="iso-acm"),
            "acm")
        self.assertTrue(ok)


class TestProxyIntegration(unittest.TestCase):
    """Integration test: start proxy, send requests, verify filtering."""

    @classmethod
    def setUpClass(cls):
        """Start a mock Docker daemon and the proxy."""
        cls.tmpdir = tempfile.mkdtemp()
        cls.upstream_sock = os.path.join(cls.tmpdir, "upstream.sock")
        cls.proxy_sock = os.path.join(cls.tmpdir, "proxy.sock")

        # Start mock upstream daemon
        cls.upstream_server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        cls.upstream_server.bind(cls.upstream_sock)
        cls.upstream_server.listen(16)
        cls.mock_thread = Thread(target=cls._run_mock_upstream, daemon=True)
        cls.mock_thread.start()

        # Start proxy
        proxy_script = str(Path(__file__).parent.parent / "bin" / "docker-proxy")
        cls.proxy_proc = subprocess.Popen(
            [sys.executable, proxy_script,
             "--user", "testuser",
             "--socket", cls.proxy_sock,
             "--upstream", cls.upstream_sock],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        # Wait for proxy socket
        for _ in range(20):
            if os.path.exists(cls.proxy_sock):
                break
            time.sleep(0.1)

    @classmethod
    def _run_mock_upstream(cls):
        """Mock Docker daemon: accept connections, respond to any request."""
        while True:
            try:
                client, _ = cls.upstream_server.accept()
                Thread(target=cls._handle_mock, args=(client,), daemon=True).start()
            except OSError:
                break

    @classmethod
    def _handle_mock(cls, client):
        """Handle a mock Docker connection: read request, send OK response."""
        try:
            buf = b""
            while b"\r\n\r\n" not in buf:
                chunk = client.recv(8192)
                if not chunk:
                    return
                buf += chunk

            # Parse content-length to consume body
            hdr = buf[:buf.index(b"\r\n\r\n") + 4].decode()
            cl = 0
            for line in hdr.split("\r\n"):
                if line.lower().startswith("content-length:"):
                    cl = int(line.split(":", 1)[1].strip())
            body_start = buf[buf.index(b"\r\n\r\n") + 4:]
            remaining = cl - len(body_start)
            while remaining > 0:
                chunk = client.recv(min(remaining, 8192))
                if not chunk:
                    break
                remaining -= len(chunk)

            # Respond with a fake container ID
            resp_body = json.dumps({"Id": "abc123", "Warnings": []})
            resp = (
                f"HTTP/1.1 201 Created\r\n"
                f"Content-Type: application/json\r\n"
                f"Content-Length: {len(resp_body)}\r\n"
                f"\r\n{resp_body}"
            ).encode()
            client.sendall(resp)
        except Exception:
            pass
        finally:
            client.close()

    @classmethod
    def tearDownClass(cls):
        cls.proxy_proc.terminate()
        cls.proxy_proc.wait(timeout=5)
        cls.upstream_server.close()
        import shutil
        shutil.rmtree(cls.tmpdir, ignore_errors=True)

    def _send_create(self, host_config):
        """Send a container create request through the proxy."""
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self.proxy_sock)
        s.settimeout(5)

        body = json.dumps({
            "Image": "alpine",
            "Cmd": ["echo", "test"],
            "HostConfig": host_config,
        })
        req = (
            f"POST /v1.51/containers/create HTTP/1.1\r\n"
            f"Host: docker\r\n"
            f"Content-Type: application/json\r\n"
            f"Content-Length: {len(body)}\r\n"
            f"Connection: close\r\n"
            f"\r\n{body}"
        ).encode()
        s.sendall(req)

        resp = b""
        try:
            while True:
                chunk = s.recv(4096)
                if not chunk:
                    break
                resp += chunk
        except socket.timeout:
            pass
        s.close()
        return resp.decode("utf-8", errors="replace")

    def test_allowed_passes_through(self):
        resp = self._send_create({"Binds": ["/Users/Workspaces/testuser/src:/app"]})
        self.assertIn("201", resp)
        self.assertIn("abc123", resp)

    def test_blocked_returns_403(self):
        resp = self._send_create({"Binds": ["/Users/bvt/.ssh:/mnt"]})
        self.assertIn("403", resp)
        self.assertIn("isolator", resp)

    def test_privileged_returns_403(self):
        resp = self._send_create({"Privileged": True})
        self.assertIn("403", resp)

    def test_clean_create_passes(self):
        resp = self._send_create({})
        self.assertIn("201", resp)

    def test_ping_passes_through(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self.proxy_sock)
        s.settimeout(5)
        s.sendall(b"HEAD /_ping HTTP/1.1\r\nHost: docker\r\n\r\n")
        resp = b""
        try:
            while True:
                chunk = s.recv(4096)
                if not chunk:
                    break
                resp += chunk
        except socket.timeout:
            pass
        s.close()
        # Mock returns 201 for everything, but we should get some response
        self.assertIn(b"HTTP/1.1", resp)


class TestIsoHelpers(unittest.TestCase):
    """Test iso script helper functions."""

    @classmethod
    def _load_iso(cls):
        iso_path = Path(__file__).parent.parent / "bin" / "iso"
        iso_code = iso_path.read_text()
        ns = {"__file__": str(iso_path), "__name__": "__not_main__"}
        # Only exec up to the main() guard — skip actual execution
        # Extract just the function definitions by stopping at if __name__
        code_lines = iso_code.split("\n")
        trimmed = []
        for line in code_lines:
            if line.strip() == 'if __name__ == "__main__":':
                break
            trimmed.append(line)
        exec(compile("\n".join(trimmed), str(iso_path), "exec"), ns)
        return ns

    def test_next_uid_empty(self):
        """next_uid with no users returns BASE_UID."""
        ns = self._load_iso()
        self.assertEqual(ns["next_uid"]({}), 600)

    def test_next_uid_sequential(self):
        ns = self._load_iso()
        users = {"acm": {"uid": 600}, "click": {"uid": 601}}
        self.assertEqual(ns["next_uid"](users), 602)

    def test_next_uid_gap(self):
        ns = self._load_iso()
        users = {"acm": {"uid": 600}, "click": {"uid": 602}}
        self.assertEqual(ns["next_uid"](users), 601)

    def test_user_subnet(self):
        ns = self._load_iso()
        config = {"users": {"acm": {"uid": 600}, "click": {"uid": 601}}}
        self.assertEqual(ns["user_subnet"](config, "acm"), "172.30.0.0/24")
        self.assertEqual(ns["user_subnet"](config, "click"), "172.30.1.0/24")

    def test_is_unrestricted(self):
        ns = self._load_iso()
        self.assertTrue(ns["is_unrestricted"]({"hosts": ["*"]}))
        self.assertFalse(ns["is_unrestricted"]({"hosts": ["api.anthropic.com"]}))
        self.assertFalse(ns["is_unrestricted"]({}))

    def test_should_log(self):
        ns = self._load_iso()
        self.assertTrue(ns["should_log"]({"log": True}))
        self.assertFalse(ns["should_log"]({"log": False}))
        self.assertFalse(ns["should_log"]({}))


if __name__ == "__main__":
    unittest.main()
