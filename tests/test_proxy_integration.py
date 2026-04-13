#!/usr/bin/env python3
"""Integration tests: start proxy with mock daemon, verify end-to-end filtering."""

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from threading import Thread


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
        cls.last_create_body = None

        # Start proxy with --insecure-skip-checks (test runs as non-root in tempdir)
        proxy_script = str(Path(__file__).parent.parent / "bin" / "docker-proxy")
        cls.proxy_proc = subprocess.Popen(
            [sys.executable, proxy_script,
             "--user", "testuser",
             "--socket", cls.proxy_sock,
             "--upstream", cls.upstream_sock,
             "--insecure-skip-checks"],
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

            hdr_end = buf.index(b"\r\n\r\n") + 4
            hdr = buf[:hdr_end].decode()
            request_line = hdr.split("\r\n", 1)[0]
            method, path, _ = request_line.split(" ", 2)
            cl = 0
            for line in hdr.split("\r\n"):
                if line.lower().startswith("content-length:"):
                    cl = int(line.split(":", 1)[1].strip())
            body_start = buf[hdr_end:]
            remaining = cl - len(body_start)
            body = body_start
            while remaining > 0:
                chunk = client.recv(min(remaining, 8192))
                if not chunk:
                    break
                body += chunk
                remaining -= len(chunk)

            if method == "POST" and path == "/v1.51/containers/create":
                cls.last_create_body = body.decode("utf-8")
                resp_body = json.dumps({"Id": "abc123", "Warnings": []})
                status = "201 Created"
            elif method == "GET" and path == "/v1.51/containers/json":
                resp_body = json.dumps([
                    {"Id": "abc123", "Labels": {"dev.boris.isolator.user": "testuser"}},
                    {"Id": "victim999", "Labels": {"dev.boris.isolator.user": "other"}},
                ])
                status = "200 OK"
            elif method == "GET" and path == "/v1.51/containers/abc123/json":
                payload = json.dumps({
                    "Id": "abc123",
                    "Config": {"Labels": {"dev.boris.isolator.user": "testuser"}},
                }).encode()
                chunks = [
                    f"{len(payload):X}\r\n".encode(),
                    payload,
                    b"\r\n0\r\n\r\n",
                ]
                resp = (
                    b"HTTP/1.1 200 OK\r\n"
                    b"Content-Type: application/json\r\n"
                    b"Transfer-Encoding: chunked\r\n"
                    b"\r\n" + b"".join(chunks)
                )
                client.sendall(resp)
                return
            elif method == "GET" and path == "/v1.51/containers/victim999/json":
                payload = json.dumps({
                    "Id": "victim999",
                    "Config": {"Labels": {"dev.boris.isolator.user": "other"}},
                }).encode()
                chunks = [
                    f"{len(payload):X}\r\n".encode(),
                    payload,
                    b"\r\n0\r\n\r\n",
                ]
                resp = (
                    b"HTTP/1.1 200 OK\r\n"
                    b"Content-Type: application/json\r\n"
                    b"Transfer-Encoding: chunked\r\n"
                    b"\r\n" + b"".join(chunks)
                )
                client.sendall(resp)
                return
            elif method == "GET" and path == "/v1.51/exec/exec-owned/json":
                resp_body = json.dumps({"ID": "exec-owned", "ContainerID": "abc123"})
                status = "200 OK"
            elif method == "GET" and path == "/v1.51/exec/exec-other/json":
                resp_body = json.dumps({"ID": "exec-other", "ContainerID": "victim999"})
                status = "200 OK"
            else:
                resp_body = json.dumps({"Id": "abc123", "Warnings": []})
                status = "200 OK"
            resp = (
                f"HTTP/1.1 {status}\r\n"
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
        self.assertIn('"dev.boris.isolator.user": "testuser"', self.last_create_body)

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

    def test_socket_perms_locked_down(self):
        """Socket should be mode 600 — only target user can connect."""
        import stat
        st = os.stat(self.proxy_sock)
        mode = stat.S_IMODE(st.st_mode)
        # In test mode --insecure-skip-checks skips chown but still chmods 600
        self.assertEqual(mode, 0o600,
                         f"Socket mode is {oct(mode)}, expected 0o600")

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
        self.assertIn(b"HTTP/1.1", resp)

    def test_blocked_build_returns_403(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self.proxy_sock)
        s.settimeout(5)
        s.sendall(
            b"POST /v1.51/build HTTP/1.1\r\n"
            b"Host: docker\r\n"
            b"Content-Length: 0\r\n"
            b"Connection: close\r\n\r\n"
        )
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
        self.assertIn(b"403", resp)

    def test_container_list_filtered_to_owned(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self.proxy_sock)
        s.settimeout(5)
        s.sendall(b"GET /v1.51/containers/json HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n")
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
        decoded = resp.decode("utf-8", errors="replace")
        self.assertIn("abc123", decoded)
        self.assertNotIn("victim999", decoded)
        hdr, body = decoded.split("\r\n\r\n", 1)
        self.assertIn("Content-Length", hdr)
        self.assertTrue(body.startswith("["), repr(body[:20]))
        parsed = json.loads(body)
        self.assertEqual(len(parsed), 1)
        self.assertEqual(parsed[0]["Id"], "abc123")

    def test_owned_container_start_allowed(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self.proxy_sock)
        s.settimeout(5)
        s.sendall(b"POST /v1.51/containers/abc123/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
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
        self.assertNotIn(b"403", resp)
        self.assertIn(b"HTTP/1.1 200 OK", resp)

    def test_other_container_inspect_blocked(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self.proxy_sock)
        s.settimeout(5)
        s.sendall(b"GET /v1.51/containers/victim999/json HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n")
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
        self.assertIn(b"403", resp)

    def test_other_exec_blocked(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self.proxy_sock)
        s.settimeout(5)
        s.sendall(b"POST /v1.51/exec/exec-other/start HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
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
        self.assertIn(b"403", resp)


if __name__ == "__main__":
    unittest.main()
