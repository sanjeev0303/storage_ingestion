import io
import json
import sys
import tarfile
import tempfile
import types
import unittest

from osp import snapshots
from osp.client import Client
from osp.stream import SnapshotStream

from test_sdk import Server, sha


def tar_of(entries):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        for name, data in entries:
            info = tarfile.TarInfo(name)
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


class StreamTest(unittest.TestCase):
    def setUp(self):
        self.files = {
            "img/0.bin": b"zero", "img/1.bin": b"one", "img/2.bin": b"two", "img/3.bin": b"three",
            "shards/a.tar": tar_of([("0001.jpg", b"J1"), ("0001.cls", b"cat"), ("0002.jpg", b"J2"), ("0002.cls", b"dog")]),
            "table.csv": b"id,label\n1,cat\n2,dog\n",
            "rows.jsonl": b'{"id": 3}\n{"id": 4}\n',
        }
        self.corrupt = set()
        self.fetches = []
        names = sorted(self.files)

        def storage(method, path, body, headers):
            name = names[int(path.lstrip("/"))]
            self.fetches.append(name)
            return 200, b"tampered" if name in self.corrupt else self.files[name], None

        self.storage = Server(storage)
        self.members = [{"path": p, "version_id": f"asset{i}", "size_bytes": len(self.files[p]),
                         "checksum_sha256": sha(self.files[p]), "availability": "AVAILABLE",
                         "split": "train" if p.startswith("img/") else "", "label": "x", "metadata": {"k": p}}
                        for i, p in enumerate(names)]
        members, storage_url = self.members, self.storage.url

        class FakeClient(Client):
            """The real client's fetch, with the API's answers faked."""

            def __init__(self):
                super().__init__("http://api")

            def get(self, path, **params):
                return {"members": members}

            def post(self, path, body=None, headers=None):
                return {"url": storage_url + "/" + path.split("/")[4][len("asset"):]}

        self.client = FakeClient()

    def tearDown(self):
        self.storage.close()

    def stream(self, **kw):
        # These read file by file; ShardStreamTest reads shard streams.
        return SnapshotStream(self.client, "ds", "v1", **{"read": "files", **kw})

    def test_shares_cover_every_member_once_and_shuffle_alike(self):
        s = self.stream(split="train", shuffle_seed=7)
        shares = [[m["path"] for m in s.share(i, 3)] for i in range(3)]
        flat = sorted(p for share in shares for p in share)
        self.assertEqual(flat, ["img/0.bin", "img/1.bin", "img/2.bin", "img/3.bin"])
        self.assertEqual(shares, [[m["path"] for m in self.stream(split="train", shuffle_seed=7).share(i, 3)] for i in range(3)],
                         "every worker computes the same order")
        s.set_epoch(1)
        self.assertNotEqual([m["path"] for m in s.share()], [m["path"] for m in self.stream(split="train", shuffle_seed=7).share()])

    def test_files_are_checked_and_erased_ones_left_out(self):
        self.members[0]["availability"] = "ERASED"
        s = self.stream(split="train")
        self.assertEqual((len(s), s.erased), (3, 1))
        got = {x["path"]: x["data"] for x in s.samples()}
        self.assertEqual(got, {"img/1.bin": b"one", "img/2.bin": b"two", "img/3.bin": b"three"})
        self.corrupt.add("img/2.bin")
        with self.assertRaises(snapshots.IntegrityError):
            list(s.samples())

    def test_cache_is_read_on_later_epochs(self):
        with tempfile.TemporaryDirectory() as cache:
            s = self.stream(split="train", cache_dir=cache)
            list(s.samples())
            fetched = len(self.fetches)
            list(s.samples())
            self.assertEqual(len(self.fetches), fetched, "a checked cache entry is not fetched again")

    def test_webdataset_shards_and_records(self):
        s = self.stream()
        tars = [x for m in s.members if m["path"].endswith(".tar") for x in s.samples_of(m, "tar")]
        self.assertEqual([(x["__key__"], x["jpg"], x["cls"]) for x in tars], [("0001", b"J1", b"cat"), ("0002", b"J2", b"dog")])
        rows = [r for m in s.members if m["path"].endswith((".csv", ".jsonl")) for r in s.samples_of(m, "records")]
        self.assertEqual(rows, [{"id": 3}, {"id": 4}, {"id": "1", "label": "cat"}, {"id": "2", "label": "dog"}])

    @unittest.skipUnless(__import__("importlib").util.find_spec("pyarrow"), "pyarrow is not installed")
    def test_parquet_records(self):
        import pyarrow as pa
        import pyarrow.parquet as pq
        buf = pa.BufferOutputStream()
        pq.write_table(pa.table({"id": [1, 2, 3], "label": ["a", "b", "c"]}), buf)
        data = buf.getvalue().to_pybytes()
        member = {"path": "t.parquet", "version_id": "assetX", "checksum_sha256": sha(data)}
        s = self.stream()
        s._fetch_to = lambda m, target: open(target, "wb").write(data)
        self.assertEqual(list(s.samples_of(member, "records")), [{"id": 1, "label": "a"}, {"id": 2, "label": "b"}, {"id": 3, "label": "c"}])

    def test_tensorflow_adapter_feeds_from_generator(self):
        calls = {}

        class Dataset:
            @staticmethod
            def from_generator(generate, output_signature):
                calls["signature"] = output_signature
                calls["rows"] = list(generate())
                return "dataset"

        fake = types.SimpleNamespace(data=types.SimpleNamespace(Dataset=Dataset))
        sys.modules["tensorflow"] = fake
        try:
            from osp.tf import as_tf_dataset
            out = as_tf_dataset(self.stream(split="train"), "sig", transform=lambda x: x["path"], shard=1, shards=2)
        finally:
            del sys.modules["tensorflow"]
        self.assertEqual(out, "dataset")
        self.assertEqual(calls["rows"], ["img/1.bin", "img/3.bin"])

    @unittest.skipUnless(__import__("importlib").util.find_spec("datasets"), "Hugging Face datasets is not installed")
    def test_hugging_face_iterable_dataset(self):
        from osp.hf import as_hf_dataset
        ds = as_hf_dataset(self.stream(split="train"))
        rows = list(ds)
        self.assertEqual(sorted(r["path"] for r in rows), ["img/0.bin", "img/1.bin", "img/2.bin", "img/3.bin"])
        self.assertEqual(ds.n_shards, 4, "each member is a shard, so workers share them out")
        self.assertEqual(json.loads(rows[0]["metadata"])["k"], rows[0]["path"])

    @unittest.skipUnless(__import__("importlib").util.find_spec("numpy"), "NumPy is not installed")
    def test_jax_batches_stack_and_drop_the_ragged_end(self):
        from osp.jax import batches
        out = list(batches(self.stream(split="train"), 3, transform=lambda x: {"n": len(x["data"])}, shard=0, shards=1))
        # Four training files make one batch of three; the fourth is dropped.
        self.assertEqual(len(out), 1)
        self.assertEqual(sorted(out[0]["n"].tolist()), [3, 3, 4])


if __name__ == "__main__":
    unittest.main()
