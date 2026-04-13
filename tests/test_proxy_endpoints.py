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

    def test_ping(self):
        self.assertTrue(is_endpoint_allowed("HEAD", "/_ping"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/_ping"))

    def test_create(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/create"))

    def test_start_stop(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/start"))
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/stop"))
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/wait"))
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/containers/abc123/attach"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/containers/abc123/logs"))

    def test_image_pull(self):
        self.assertTrue(is_endpoint_allowed("POST", "/v1.51/images/create?fromImage=alpine"))
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/images/alpine/json"))

    def test_blocked_global_metadata(self):
        self.assertFalse(is_endpoint_allowed("GET", "/v1.51/info"))
        self.assertFalse(is_endpoint_allowed("GET", "/v1.51/images/json"))
        self.assertFalse(is_endpoint_allowed("GET", "/v1.51/system/df"))

    def test_build(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/build"))

    def test_blocked_image_from_src(self):
        self.assertFalse(
            is_endpoint_allowed("POST", "/v1.51/images/create?fromSrc=https://evil.example/payload.tar")
        )

    def test_blocked_image_push(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/images/evil/push"))

    def test_blocked_image_unknown_query_mode(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/images/create?repo=alpine"))

    def test_iso_user_network(self):
        self.assertTrue(is_endpoint_allowed("GET", "/v1.51/networks/iso-acm", "acm"))
        self.assertTrue(is_endpoint_allowed("DELETE", "/v1.51/networks/iso-acm", "acm"))

    def test_blocked_networks_create(self):
        # Agent could create unrestricted network — block /networks/create
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/networks/create"))

    def test_blocked_volumes_create(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/volumes/create"))

    def test_blocked_other_user_network(self):
        self.assertFalse(is_endpoint_allowed("GET", "/v1.51/networks/iso-click", "acm"))

    def test_blocked_swarm(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/swarm/init"))
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/swarm/join"))

    def test_blocked_plugin_install(self):
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/plugins/pull"))
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/plugins/install"))

    def test_blocked_container_update(self):
        # /update lets agent change resource limits, RestartPolicy etc.
        self.assertFalse(is_endpoint_allowed("POST", "/v1.51/containers/abc/update"))

    def test_blocked_unknown(self):
        self.assertFalse(is_endpoint_allowed("GET", "/v1.51/foo/bar"))
        self.assertFalse(is_endpoint_allowed("POST", "/random"))


if __name__ == "__main__":
    unittest.main()
