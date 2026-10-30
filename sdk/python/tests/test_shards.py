import base64
import io
import json
import tarfile
import unittest

from osp import shards, snapshots
from osp.client import Client
from osp.stream import SnapshotStream

from test_sdk import Server, sha


def tar_stream(files, cut_after=None):
    """A shard stream as the server writes it: PAX records carry each
    file's SHA-256 and ordinal. cut_after ends it early, as a cut stream."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w", format=tarfile.PAX_FORMAT) as tar:
        for path, data, ordinal in files:
            info = tarfile.TarInfo(path)
            info.size = len(data)
            info.pax_headers = {"OSP.sha256": sha(data), "OSP.ordinal": str(ordinal)}
            tar.addfile(info, io.BytesIO(data))
    raw = buf.getvalue()
    return raw[:cut_after] if cut_after else raw


class ShardStreamTest(unittest.TestCase):
    def setUp(self):
        self.files = {"a/0001.cls.txt": b"cat", "a/0001.jpg": b"J1", "a/0002.cls.txt": b"dog", "a/0002.jpg": b"J2",
                      "b/x.bin": b"x" * 300}
        self.paths = sorted(self.files)
        self.members = [{"ordinal": i + 1, "object_id": f"o{i}", "version_id": f"asset{i}", "object_version": 1,
                         "path": p, "size_bytes": len(self.files[p]), "content_type": "application/octet-stream",
                         "checksum_sha256": sha(self.files[p]), "availability": "AVAILABLE",
                         "split": "train", "label": "", "metadata": {}} for i, p in enumerate(self.paths)]
        self.tamper, self.cut, self.requests, self.posted = set(), False, [], []
        by_ordinal = {m["ordinal"]: m for m in self.members}

        def api(method, path, body, headers):
            if path.startswith("/api/v1/datasets/ds/versions/v1/shards"):
                return 200, {"shards": [{"ordinals": [1, 2, 3, 4], "files": 4, "bytes": 10, "packs": 1},
                                        {"ordinals": [5], "files": 1, "bytes": 300, "packs": 0}]}, None
            if path.endswith("/stream"):
                ordinals = json.loads(body)["ordinals"]
                self.requests.append(ordinals)
                files = []
                for o in ordinals:
                    p = by_ordinal[o]["path"]
                    files.append((p, b"tampered" if p in self.tamper else self.files[p], o))
                raw = tar_stream(files, cut_after=700 if self.cut else None)
                if p in self.tamper:
                    # The server's own header follows the bytes it served.
                    pass
                return 200, raw, {"Content-Type": "application/x-tar"}
            if path.endswith("/verifications"):
                self.posted.append(json.loads(body))
                return 201, {"id": "rv1"}, None
            return 404, {"error": "no"}, None

        self.api = Server(api)
        members = self.members

        class FakeClient(Client):
            def get(self, path, **params):
                if path.endswith("/members"):
                    return {"members": members}
                return super().get(path, **params)

        self.client = FakeClient(self.api.url)

    def tearDown(self):
        self.api.close()

    def test_files_come_a_shard_at_a_time_and_each_is_checked(self):
        s = SnapshotStream(self.client, "ds", "v1")
        got = {x["path"]: x["data"] for x in s.samples()}
        self.assertEqual(got, self.files)
        self.assertEqual(sorted(map(len, self.requests)), [1, 4], "one request per shard, not per file")
        self.assertEqual(s.verifier.checked, 5)

    def test_samples_group_files_by_stem(self):
        s = SnapshotStream(self.client, "ds", "v1", prefetch=1)
        got = list(s.samples("samples"))
        self.assertEqual([(x["__key__"], x.get("jpg"), x.get("cls.txt")) for x in got[:2]],
                         [("a/0001", b"J1", b"cat"), ("a/0002", b"J2", b"dog")])

    def test_workers_share_shards_and_split_filters_them(self):
        s = SnapshotStream(self.client, "ds", "v1", shuffle_seed=3)
        a, b = s.share_shards(0, 2), s.share_shards(1, 2)
        self.assertEqual(sorted(o for x in a + b for o in x["ordinals"]), [1, 2, 3, 4, 5])
        self.members[4]["split"] = "test"
        s = SnapshotStream(self.client, "ds", "v1", split="train")
        self.assertEqual([x["ordinals"] for x in s.shard_plan()], [[1, 2, 3, 4]])

    def test_a_corrupt_file_raises_or_is_left_out_and_reported(self):
        self.tamper.add("a/0002.jpg")
        with self.assertRaises(snapshots.IntegrityError):
            list(SnapshotStream(self.client, "ds", "v1", prefetch=1).samples())
        s = SnapshotStream(self.client, "ds", "v1", on_corrupt="skip")
        got = [x["path"] for x in s.samples()]
        self.assertNotIn("a/0002.jpg", got)
        self.assertEqual((s.verifier.failed, s.verifier.failures[0]["reason"]), (1, "size 8, pinned 2"))
        s.report("run1", host="gpu-7")
        self.assertEqual((self.posted[0]["host"], self.posted[0]["failed"], self.posted[0]["checked"]), ("gpu-7", 1, 5))

    def test_sampled_verification_is_deterministic(self):
        v1, v2 = shards.Verifier(0.5, seed=1), shards.Verifier(0.5, seed=1)
        paths = [f"f{i}" for i in range(200)]
        picked = [p for p in paths if v1.wants(p)]
        self.assertEqual(picked, [p for p in paths if v2.wants(p)])
        self.assertTrue(60 < len(picked) < 140, len(picked))
        self.assertTrue(v1.check("not-sampled", b"x", "wrong") or v1.wants("not-sampled"))

    def test_a_cut_stream_is_never_taken_as_complete(self):
        self.cut = True
        with self.assertRaises(shards.StreamCut):
            list(SnapshotStream(self.client, "ds", "v1", prefetch=1).samples())


class ManifestTest(unittest.TestCase):
    def test_canonical_line_matches_go(self):
        # The server's own line for this member (model.TestCanonicalLineIsDeterministic).
        m = {"object_id": "o1", "version_id": "a1", "object_version": 2, "path": "x/<a&b>.txt", "size_bytes": 5,
             "content_type": "text/plain", "checksum_sha256": "ab", "label": "cät", "split": "train",
             "metadata": {"z": "1", "a": "2 "}}
        self.assertEqual(shards.canonical_line(m),
                         '{"object_id":"o1","version_id":"a1","version":2,"path":"x/\\u003ca\\u0026b\\u003e.txt",'
                         '"size_bytes":5,"content_type":"text/plain","sha256":"ab","label":"cät","split":"train",'
                         '"metadata":{"a":"2\\u2028","z":"1"}}\n'.encode("utf-8"))

    @unittest.skipUnless(__import__("importlib").util.find_spec("cryptography"), "cryptography is not installed")
    def test_signatures_are_checked_against_the_published_key(self):
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        key = Ed25519PrivateKey.generate()
        public = base64.b64encode(key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)).decode()

        class C:
            def get(self, path):
                return {"key_id": "k1", "public_key": public}

        body = '{"manifest_sha256":"ab"}'
        signed = {"body": body, "key_id": "k1", "signature": base64.b64encode(key.sign(body.encode())).decode()}
        self.assertEqual(shards.check_signature(C(), signed)[:2], (True, True))
        self.assertEqual(shards.check_signature(C(), dict(signed, body=body + " "))[:2], (True, False))
        self.assertEqual(shards.check_signature(C(), dict(signed, signature=""))[:2], (False, False))


if __name__ == "__main__":
    unittest.main()
