#!/usr/bin/env python3
"""Tests for iso script helper functions."""

import unittest
from pathlib import Path


class TestIsoHelpers(unittest.TestCase):
    """Test iso script helper functions."""

    @classmethod
    def _load_iso(cls):
        iso_path = Path(__file__).parent.parent / "bin" / "iso"
        iso_code = iso_path.read_text()
        ns = {"__file__": str(iso_path), "__name__": "__not_main__"}
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
