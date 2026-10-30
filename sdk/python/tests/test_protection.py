import contextlib
import io
import unittest
from unittest import mock

from osp import cli, protection


class FakeClient:
    def __init__(self):
        self.calls = []

    def request(self, method, path, body=None, params=None, **kwargs):
        self.calls.append((method, path, body))
        return {}

    def get(self, path, **params):
        self.calls.append(("GET", path, params))
        if path == "/api/v1/datasets":
            return {"datasets": [{"id": "ds1", "name": "scans", "revision": 3}]}
        if path.endswith("/replication"):
            return {"connection_id": "c2", "objects": 3, "replicated": 2, "pending": 1, "failing": 0, "last_error": "",
                    "recovery_point_seconds": 60, "escrow": True, "vault_retention_days": 0, "scrub_days": 7}
        return {"id": "j1", "kind": "MIGRATE", "state": "COMPLETED", "done_items": 3, "total_items": 3, "report": {}}

    def pages(self, path, key, **params):
        return iter([{"id": "ds1", "name": "scans", "revision": 3}])

    def post(self, path, body=None, headers=None):
        self.calls.append(("POST", path, body))
        return {"plan": {"objects": 3, "bytes": 30, "already_there": 0, "egress_estimate_usd": 0.0, "egress_note": "n"},
                "job": {"id": "j1"}}


class ProtectionTest(unittest.TestCase):
    def test_protect_sends_only_what_is_set(self):
        c = FakeClient()
        protection.protect(c, "ds1", 3, replica_connection_id="c2", scrub_days=7)
        self.assertEqual(c.calls[-1], ("PATCH", "/api/v1/datasets/ds1", {"revision": 3, "replica_connection_id": "c2", "scrub_days": 7}))

    def test_migrate_is_a_dry_run_unless_yes(self):
        c = FakeClient()
        with mock.patch.object(cli, "client_from_config", return_value=c), contextlib.redirect_stdout(io.StringIO()), \
                contextlib.redirect_stderr(io.StringIO()):
            cli.main(["migrate", "scans", "c2"])
            self.assertTrue(c.calls[-1][2]["dry_run"])
            cli.main(["migrate", "scans", "c2", "--yes", "--wait"])
        self.assertFalse([x for x in c.calls if x[0] == "POST"][-1][2]["dry_run"])

    def test_protect_exit_code_shows_pending(self):
        c = FakeClient()
        with mock.patch.object(cli, "client_from_config", return_value=c), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(["protect", "scans"]), 1)


if __name__ == "__main__":
    unittest.main()
