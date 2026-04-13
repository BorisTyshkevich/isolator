#!/usr/bin/env python3
"""Tests for docker-proxy container create inspection."""

import json
import unittest
from pathlib import Path

# Load proxy functions
_proxy_code = (Path(__file__).parent.parent / "bin" / "docker-proxy").read_text()
_proxy_ns = {}
exec(compile(_proxy_code, "docker-proxy", "exec"), _proxy_ns)

check_create = _proxy_ns["check_create"]


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

    def test_blocked_tmp_bind(self):
        # /tmp shared between sandbox users — disallowed
        ok, _ = check_create(
            self._make_request(Binds=["/tmp/cache:/cache:ro"]),
            "acm")
        self.assertFalse(ok)

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
        self.assertIn("Privileged", reason)

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
        self.assertIn("VolumesFrom", reason)

    def test_blocked_devices(self):
        ok, reason = check_create(
            self._make_request(Devices=[{"PathOnHost": "/dev/sda"}]),
            "acm")
        self.assertFalse(ok)
        self.assertIn("Devices", reason)

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

    def test_iso_user_network_allowed(self):
        ok, _ = check_create(
            self._make_request(NetworkMode="iso-acm"),
            "acm")
        self.assertTrue(ok)

    def test_iso_other_user_network_blocked(self):
        # acm cannot use click's network
        ok, _ = check_create(
            self._make_request(NetworkMode="iso-click"),
            "acm")
        self.assertFalse(ok)

    def test_custom_network_blocked(self):
        ok, _ = check_create(
            self._make_request(NetworkMode="my-network"),
            "acm")
        self.assertFalse(ok)

    def test_blocked_capadd(self):
        ok, reason = check_create(
            self._make_request(CapAdd=["SYS_ADMIN"]),
            "acm")
        self.assertFalse(ok)
        self.assertIn("CapAdd", reason)

    def test_blocked_security_opt(self):
        ok, reason = check_create(
            self._make_request(SecurityOpt=["seccomp=unconfined"]),
            "acm")
        self.assertFalse(ok)
        self.assertIn("SecurityOpt", reason)

    def test_blocked_ipc_host(self):
        ok, reason = check_create(
            self._make_request(IpcMode="host"),
            "acm")
        self.assertFalse(ok)
        self.assertIn("IpcMode", reason)

    def test_blocked_userns_host(self):
        ok, reason = check_create(
            self._make_request(UsernsMode="host"),
            "acm")
        self.assertFalse(ok)
        self.assertIn("UsernsMode", reason)

    def test_blocked_explicit_root_user(self):
        body = json.dumps({
            "Image": "alpine",
            "User": "root",
            "HostConfig": {},
        }).encode()
        ok, _ = check_create(body, "acm")
        self.assertFalse(ok)


if __name__ == "__main__":
    unittest.main()
