import io
import json
import unittest

from osp import tiering


class FakeClient:
    """Answers each route from a table and remembers what was sent."""

    def __init__(self, answers):
        self.answers, self.sent = answers, []

    def _answer(self, method, path, body=None, params=None):
        self.sent.append((method, path, body, params))
        for (m, prefix), answer in self.answers.items():
            if m == method and path.startswith(prefix):
                return answer(body, params) if callable(answer) else answer
        raise AssertionError(f"unexpected {method} {path}")

    def get(self, path, **params):
        return self._answer("GET", path, params=params)

    def post(self, path, body=None, headers=None):
        return self._answer("POST", path, body)

    def request(self, method, path, body=None, params=None, headers=None):
        return self._answer(method, path, body, params)


class TieringTest(unittest.TestCase):
    def test_rules_restores_and_expiry_send_what_the_api_expects(self):
        c = FakeClient({
            ("POST", "/api/v1/tier-rules"): {"id": "r1"},
            ("POST", "/api/v1/datasets/d1/versions/v1/archive-restores"): {"archived": 2, "requested": 2, "restores": []},
            ("POST", "/api/v1/datasets/d1/version-expiry"): lambda body, _: {"dry_run": body["dry_run"]},
            ("PUT", "/api/v1/cost-budget"): {},
        })
        tiering.add_rule(c, "cold", "ARCHIVE", min_age_days=90, unread_days=30)
        self.assertEqual(c.sent[-1][2], {"name": "cold", "tier": "ARCHIVE", "min_age_days": 90, "unread_days": 30, "dataset_id": ""})
        tiering.restore_version(c, "d1", "v1", days=3, speed="BULK")
        self.assertEqual(c.sent[-1][2], {"days": 3, "speed": "BULK"})
        self.assertTrue(tiering.expire(c, "d1", keep_last=2)["dry_run"], "expiry is a preview unless asked")
        tiering.set_budget(c, monthly_usd=50, action="BLOCK")
        self.assertEqual(c.sent[-1][2], {"monthly_usd": 50, "monthly_requests": 0, "action": "BLOCK"})

    def test_wait_restores_returns_when_none_is_open(self):
        states = iter([[{"state": "RESTORING"}], [{"state": "READY"}]])
        c = FakeClient({("GET", "/api/v1/archive-restores"): lambda *_: {"restores": next(states)}})
        self.assertEqual(tiering.wait_restores(c, version_id="v1", every=0), [{"state": "READY"}])

    def test_storage_manifest_pages_and_writes_json_lines(self):
        pages = iter([{"lines": [{"path": "a.parquet", "uri": "s3://b/k1"}], "next_after": "a.parquet"},
                      {"lines": [{"path": "b.parquet", "uri": "s3://b/k2"}]}])
        c = FakeClient({("GET", "/api/v1/datasets/d1/versions/v1/storage-manifest"): lambda *_: next(pages)})
        out = io.StringIO()
        self.assertEqual(tiering.write_manifest(c, "d1", "v1", out), 2)
        self.assertEqual([json.loads(l)["uri"] for l in out.getvalue().splitlines()], ["s3://b/k1", "s3://b/k2"])
        self.assertEqual(c.sent[1][3]["after"], "a.parquet")

    def test_iceberg_registers_the_pinned_parquet_files_in_place(self):
        lines = [{"path": "t/a.parquet", "uri": "s3://b/a"}, {"path": "t/b.parquet", "uri": "s3://b/b"}, {"path": "readme.md"}]
        c = FakeClient({("GET", "/api/v1/datasets/d1/versions/v1/storage-manifest"): {"lines": lines}})
        added = {}

        class Table:
            def add_files(self, file_paths):
                added["files"] = file_paths

        class Catalog:
            def create_table_if_not_exists(self, name, schema):
                added["name"], added["schema"] = name, schema
                return Table()

        tiering.register_iceberg(c, "d1", "v1", Catalog(), "lake.scans", schema="SCHEMA")
        self.assertEqual(added, {"name": "lake.scans", "schema": "SCHEMA", "files": ["s3://b/a", "s3://b/b"]})
        packed = FakeClient({("GET", "/api/v1/datasets/d1/versions/v1/storage-manifest"): {"lines": [{"path": "x.parquet"}]}})
        with self.assertRaises(ValueError):
            tiering.register_iceberg(packed, "d1", "v1", Catalog(), "lake.x", schema="S")


if __name__ == "__main__":
    unittest.main()
