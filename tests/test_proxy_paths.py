#!/usr/bin/env python3
"""Tests for docker-proxy path filtering."""

import sys
import unittest
from pathlib import Path

# Load proxy functions
_proxy_code = (Path(__file__).parent.parent / "bin" / "docker-proxy").read_text()
_proxy_ns = {}
exec(compile(_proxy_code, "docker-proxy", "exec"), _proxy_ns)

resolve_path = _proxy_ns["resolve_path"]
is_path_allowed = _proxy_ns["is_path_allowed"]


class TestPathAllowed(unittest.TestCase):
    """Test is_path_allowed()."""

    def test_workspace_root(self):
        self.assertTrue(is_path_allowed("/Users/Workspaces/acm", "acm"))

    def test_workspace_subdir(self):
        self.assertTrue(is_path_allowed("/Users/Workspaces/acm/project/src", "acm"))

    def test_tmp_blocked(self):
        # /tmp is shared among sandbox users → not allowed
        self.assertFalse(is_path_allowed("/tmp", "acm"))

    def test_tmp_subdir_blocked(self):
        self.assertFalse(is_path_allowed("/tmp/build-cache/layer1", "acm"))

    def test_private_tmp_blocked(self):
        self.assertFalse(is_path_allowed("/private/tmp/foo", "acm"))

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


if __name__ == "__main__":
    unittest.main()
