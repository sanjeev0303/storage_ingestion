import json
import os
import re
import stat
import sys
import types
import unittest

import osp.ray as ospray
from osp.client import Client
from osp.mount import SnapshotFS
from osp.zarr import SnapshotIndex

from test_sdk import Server, sha
from test_shards import tar_stream


class FakeAPI:
    """Members, shard streams, ranged content and download links."""

    def __init__(self, files):
        self.files = files
        self.paths = sorted(files)
        self.members = [{"ordinal": i + 1, "object_id": f"o{i}", "version_id": f"asset{i}", "object_version": 1,
                         "path": p, "size_bytes": len(files[p]), "checksum_sha256": sha(files[p]),
                         "availability": "AVAILABLE", "split": "", "label": "", "metadata": {"n": str(i)}}
                        for i, p in enumerate(self.paths)]
        self.posted = []
        by_ordinal = {m["ordinal"]: m for m in self.members}
        by_asset = {m["version_id"]: m for m in self.members}

        def api(method, path, body, headers):
            if path.startswith("/api/v1/datasets/ds/versions/v1/members"):
                return 200, {"members": self.members}, None
            if path.startswith("/api/v1/datasets/ds/versions/v1/shards"):
                return 200, {"shards": [{"ordinals": [m["ordinal"] for m in self.members]}]}, None
            if path.endswith("/stream"):
                ordinals = json.loads(body)["ordinals"]
                return 200, tar_stream([(by_ordinal[o]["path"], files[by_ordinal[o]["path"]], o) for o in ordinals]), None
            m = re.match(r"/api/v1/assets/(\w+)/(content|download-url)", path)
            if m:
                data = files[by_asset[m.group(1)]["path"]]
                if m.group(2) == "download-url":
                    return 200, {"url": f"/api/v1/assets/{m.group(1)}/content"}, None
                r = re.match(r"bytes=(\d+)-(\d+)", headers.get("Range") or "")
                if r:
                    return 206, data[int(r.group(1)):int(r.group(2)) + 1], None
                return 200, data, None
            if method == "POST":
                self.posted.append((path, json.loads(body or b"null")))
                return 201, {"id": "new", "name": "out", "version": 1}, None
            return 404, {"error": "no"}, None

        self.server = Server(api)
        self.client = Client(self.server.url)


class ZarrAndMountTest(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI({"scan.zarr/zarr.json": b'{"zarr_format":3}', "scan.zarr/c/0/0": bytes(range(100)),
                            "scan.zarr/c/0/1": b"\x01" * 50, "notes.txt": b"hello"})

    def tearDown(self):
        self.api.server.close()

    def test_index_reads_keys_and_ranges(self):
        index = SnapshotIndex(self.api.client, "ds", "v1", prefix="scan.zarr")
        self.assertEqual(index.list(), ["c/0/0", "c/0/1", "zarr.json"])
        self.assertEqual(index.list_dir(""), ["c", "zarr.json"])
        self.assertEqual(index.list_dir("c/0"), ["0", "1"])
        self.assertEqual(index.get("zarr.json"), b'{"zarr_format":3}')
        self.assertEqual(index.get("c/0/0", 10, 14), bytes([10, 11, 12, 13]))
        self.assertEqual(index.get("c/0/0", suffix=3), bytes([97, 98, 99]))
        self.assertIsNone(index.get("missing"))

    def test_mount_answers_from_the_snapshot(self):
        fs = SnapshotFS(SnapshotIndex(self.api.client, "ds", "v1"))
        self.assertTrue(stat.S_ISDIR(fs.attr("/scan.zarr/c")["st_mode"]))
        self.assertEqual(fs.attr("/notes.txt")["st_size"], 5)
        self.assertEqual(fs.list("/"), [".", "..", "notes.txt", "scan.zarr"])
        self.assertEqual(fs.read("/notes.txt", 3, 1), b"ell")
        with self.assertRaises(OSError):
            fs.attr("/nope")


@unittest.skipUnless(__import__("importlib").util.find_spec("zarr"), "Zarr is not installed")
class ZarrStoreTest(unittest.TestCase):
    def test_a_sharded_array_reads_back_through_the_store(self):
        import tempfile

        import numpy as np
        import zarr
        from osp.zarr import SnapshotStore

        with tempfile.TemporaryDirectory() as d:
            data = np.arange(64 * 64, dtype="int32").reshape(64, 64)
            arr = zarr.create_array(store=os.path.join(d, "scan.zarr"), shape=(64, 64), chunks=(16, 16),
                                    shards=(32, 32), dtype="int32")
            arr[:] = data
            files = {}
            for root, _, names in os.walk(d):
                for n in names:
                    with open(os.path.join(root, n), "rb") as f:
                        files[os.path.relpath(os.path.join(root, n), d).replace(os.sep, "/")] = f.read()
        api = FakeAPI(files)
        try:
            back = zarr.open_array(SnapshotStore(api.client, "ds", "v1", prefix="scan.zarr"), mode="r")
            self.assertTrue((back[:] == data).all())
            self.assertTrue((back[20:24, 40:44] == data[20:24, 40:44]).all(), "a chunk out of a shard, by range")
        finally:
            api.server.close()


class FakeRayDataset:
    def __init__(self, rows):
        self.rows = rows

    def flat_map(self, fn):
        return FakeRayDataset([out for row in self.rows for out in fn(row)])

    def map_batches(self, fn, batch_size, batch_format):
        outs = []
        for i in range(0, len(self.rows), batch_size):
            chunk = self.rows[i:i + batch_size]
            outs.append(fn({k: [r[k] for r in chunk] for k in chunk[0]}))
        return FakeRayDataset(outs)

    def materialize(self):
        return self


class RayTest(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI({"a/1.jpg": b"J1", "a/1.txt": b"cat", "a/2.jpg": b"J2"})
        data = types.SimpleNamespace(from_items=lambda items, override_num_blocks: FakeRayDataset(items))
        sys.modules["ray"] = types.SimpleNamespace(data=data)
        sys.modules["ray.data"] = data

    def tearDown(self):
        del sys.modules["ray"], sys.modules["ray.data"]
        self.api.server.close()

    def test_read_snapshot_reads_shards_as_blocks(self):
        rows = ospray.read_snapshot(self.api.client, "ds", "v1").rows
        self.assertEqual(sorted((r["path"], r["data"]) for r in rows), [("a/1.jpg", b"J1"), ("a/1.txt", b"cat"), ("a/2.jpg", b"J2")])
        self.assertEqual(json.loads(rows[0]["metadata"])["n"], "0")
        samples = ospray.read_snapshot(self.api.client, "ds", "v1", kind="samples").rows
        self.assertEqual([(s["__key__"], s.get("txt")) for s in samples], [("a/1", b"cat"), ("a/2", None)])

    def test_write_snapshot_uploads_batches_and_records_the_parents(self):
        seen = []

        def fake_ingest(client, folder, dataset, concurrency=8, into=None):
            for root, _, names in os.walk(folder):
                for n in names:
                    with open(os.path.join(root, n), "rb") as f:
                        seen.append((os.path.relpath(os.path.join(root, n), folder).replace(os.sep, "/"), f.read()))
            return {"failed": []}

        real, ospray.ingest_folder = ospray.ingest_folder, fake_ingest
        try:
            out = FakeRayDataset([{"path": "x/1.bin", "data": b"one"}, {"path": "x/2.bin", "data": b"two"}])
            ospray.write_snapshot(out, self.api.client, "cleaned", parents=["v1"], transformation={"name": "dedupe", "version": "1"},
                                  batch_size=1)
        finally:
            ospray.ingest_folder = real
        self.assertEqual(sorted(seen), [("x/1.bin", b"one"), ("x/2.bin", b"two")])
        created = self.api.posted[0][1]
        self.assertEqual(created["derived_from"], {"version_ids": ["v1"], "copy_members": False,
                                                   "transformation": {"name": "dedupe", "version": "1"}})
        self.assertTrue(self.api.posted[-1][0].endswith("/snapshots"))


class ValidateTest(unittest.TestCase):
    def test_a_job_that_ends_with_warnings_is_done(self):
        from osp import snapshots

        class C:
            def post(self, path, body):
                return {"id": "j1", "state": "QUEUED"}

            def get(self, path):
                if path.endswith("/j1"):
                    return {"id": "j1", "state": "COMPLETED_WITH_WARNINGS", "report_id": "r1"}
                return {"id": "r1", "findings": []}

        report, findings = snapshots.validate(C(), "ds", "v1", poll=0)
        self.assertEqual((report["id"], findings), ("r1", []))


if __name__ == "__main__":
    unittest.main()
