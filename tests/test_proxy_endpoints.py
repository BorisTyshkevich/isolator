#!/usr/bin/env python3
"""Tests for docker-proxy endpoint allowlist."""

import unittest
from pathlib import Path

# Load proxy functions
_proxy_code = (Path(__file__).parent.parent / "bin" / "docker-proxy").read_text()
_proxy_ns = {}
exec(compile(_proxy_code, "docker-proxy", "exec"), _proxy_ns)

is_endpoint_allowed = _proxy_ns["is_endpoint_allowed"]


class TestEndpointAllowlist(unittest.TestCase):
    """Verify default-deny endpoint policy."""

    # ── System ──

    def test_ping(self):
        self.assertTrue(is_endpoint_allowed("HEAD", "/_ping"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/_ping"))

    def test_info_versioned(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/info"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.44/info"))

    def test_info_unversioned(self):
        # Go Docker SDK calls /info without version prefix before negotiation
        self.assertTrue(is_endpoint_allowed("GET", "/info"))

    def test_version(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/version"))

    def test_system_df(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/system/df"))

    def test_events(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/events"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/system/events"))

    # ── Containers ──

    def test_container_create(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/create"))

    def test_container_list(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/containers/json"))

    def test_container_lifecycle(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/start"))
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/stop"))
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/kill"))
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/wait"))
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/attach"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/containers/abc123/logs"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/containers/abc123/json"))
        self.assertTrue(is_endpoint_allowed("DELETE", "/v1.51/containers/abc123"))

    def test_blocked_container_update(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/containers/abc/update"))

    # ── Images ──

    def test_image_pull(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/images/create?fromImage=alpine"))

    def test_image_inspect(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/images/alpine/json"))

    def test_image_list(self):
        # testcontainers needs image list
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/images/json"))

    def test_blocked_image_from_src(self):
        self.assertFalse(
            is_endpoint_allowed("POST", "/v1.51/images/create?fromSrc=https://evil.example/payload.tar")
        )

    def test_blocked_image_push(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/images/evil/push"))

    # ── Networks ──

    def test_network_list(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/networks"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/networks/json"))

    def test_network_inspect(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/networks/bridge"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/networks/abc123def"))

    def test_network_create(self):
        # testcontainers needs this — iptables provides egress defense-in-depth
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/networks/create"))

    def test_network_connect_disconnect(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/networks/abc123/connect"))
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/networks/abc123/disconnect"))

    def test_network_delete(self):
        self.assertTrue(is_endpoint_allowed("DELETE", "/v1.51/networks/abc123"))

    def test_iso_user_network(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/networks/iso-acm", "acm"))
        self.assertTrue(is_endpoint_allowed("DELETE", "/v1.51/networks/iso-acm", "acm"))

    # ── Build ──

    def test_build_allowed(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/build"))

    # ── Volumes ──

    def test_volumes_allowed(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/volumes/create"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/volumes"))

    # ── Exec ──

    def test_exec(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/exec/abc123/start"))

    # ── Blocked ──

    def test_blocked_swarm(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/swarm/init"))
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/swarm/join"))

    def test_blocked_plugin_install(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/plugins/pull"))

    def test_blocked_unknown(self):
        self.assertFalse(is_endpoint_allowed("GET", "/v1.51/foo/bar"))
        self.assertFalse(is_endpoint_allowed("POST", "/random"))


if __name__ == "__main__":
    unittest.main()
