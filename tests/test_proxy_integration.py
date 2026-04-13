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
        self.assertIn(b"HTTP/1.1", resp)


if __name__ == "__main__":
    unittest.main()
