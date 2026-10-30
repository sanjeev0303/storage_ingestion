import argparse
import contextlib
import csv
import io
import os
import tempfile
import unittest

from osp import adoption, cli


class FakeClient:
    def __init__(self):
        self.calls = []

    def post(self, path, body=None, headers=None):
        self.calls.append(("POST", path, body))
        return {"id": "ad-1", "state": "QUEUED", "entries": 0, "adopted": 0, "rejected": 0, "failed": 0, **(body or {})}

    def get(self, path, **params):
        self.calls.append(("GET", path, params))
        if path.endswith("/results"):
            if not params.get("after"):
                return {"entries": [{"line": 1, "object_key": "a", "path": "a", "state": "ADOPTED", "asset_id": "x"}], "next_after": 1}
            return {"entries": [{"line": 2, "object_key": "b", "path": "b", "state": "REJECTED", "code": "digest_mismatch"}],
                    "next_after": 0}
        return {"id": "ad-1", "state": "COMPLETED", "entries": 2, "adopted": 1, "rejected": 1, "failed": 0, "source_ref": ""}


class AdoptionTest(unittest.TestCase):
    def test_adopt_sends_only_what_is_set(self):
        client = FakeClient()
        adoption.adopt(client, "s3_inventory", connection_id="c1", source_ref="inv/manifest.json")
        self.assertEqual(client.calls[0][2], {"source": "S3_INVENTORY", "mode": "DETACH", "require_digest": True,
                                              "connection_id": "c1", "source_ref": "inv/manifest.json"})

    def test_results_pages_until_the_end(self):
        rows = list(adoption.results(FakeClient(), "ad-1"))
        self.assertEqual([r["line"] for r in rows], [1, 2])

    def test_cli_writes_a_results_file(self):
        client = FakeClient()
        original = cli.client_from_config
        cli.client_from_config = lambda: client
        path = os.path.join(tempfile.mkdtemp(), "r.csv")
        try:
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                cli.cmd_adoption(argparse.Namespace(job="ad-1", results=path))
        finally:
            cli.client_from_config = original
        with open(path, encoding="utf-8") as f:
            rows = list(csv.reader(f))
        self.assertEqual(rows[0][:5], ["line", "object_key", "url", "path", "state"])
        self.assertEqual([r[4] for r in rows[1:]], ["ADOPTED", "REJECTED"])
        self.assertEqual(rows[2][5], "digest_mismatch")


if __name__ == "__main__":
    unittest.main()
