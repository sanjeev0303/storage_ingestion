import email.message
import hashlib
import io
import json
import os
import tempfile
import unittest
import zipfile

from osp import connectors


def sha(data):
    return hashlib.sha256(data).hexdigest()


class Readers(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = self.tmp.name
        self.out = os.path.join(self.dir, "out")
        os.makedirs(self.out)

    def test_comms_jsonl_checks_each_message(self):
        with open(os.path.join(self.dir, "m3.eml"), "wb") as f:
            f.write(b"file body")
        path = os.path.join(self.dir, "export.jsonl")
        with open(path, "w", encoding="utf-8") as f:
            f.write(json.dumps({"id": "m1", "from": "a", "body": "hello", "sha256": sha(b"hello")}) + "\n")
            f.write(json.dumps({"id": "m/2", "body": "tampered", "sha256": sha(b"original")}) + "\n")
            f.write(json.dumps({"id": "m3", "file": "m3.eml", "sha256": sha(b"file body")}) + "\n")
            f.write(json.dumps({"id": "m4", "file": "gone.eml", "sha256": "x"}) + "\n")
            f.write(json.dumps({"id": "m5", "body": "no hash"}) + "\n")
        r = connectors.read_comms_jsonl(path, self.out)
        self.assertEqual((r.items, r.verified), (5, 2))
        self.assertEqual(len(r.mismatches), 3)
        self.assertIn("m_2: differs", r.mismatches[0])
        kept = sorted(os.listdir(os.path.join(self.out, "messages")))
        self.assertIn("m_2.txt", kept)  # a mismatch is still preserved
        self.assertIn("m1.meta.json", kept)

    def test_md5_and_sha1_are_accepted(self):
        path = os.path.join(self.dir, "f.bin")
        with open(path, "wb") as f:
            f.write(b"x")
        self.assertTrue(connectors._matches(path, hashlib.md5(b"x").hexdigest()))
        self.assertTrue(connectors._matches(path, hashlib.sha1(b"x").hexdigest()))
        self.assertTrue(connectors._matches(path, "sha256:" + sha(b"x")))
        self.assertFalse(connectors._matches(path, "abc"))

    def rsmf(self, attachments=("photo.jpg",), header=True):
        buf = io.BytesIO()
        with zipfile.ZipFile(buf, "w") as z:
            manifest = {"version": "1.0.0", "events": [{"id": "e1", "attachments": [{"id": a} for a in attachments]}, {"id": "e2"}]}
            z.writestr("rsmf_manifest.json", json.dumps(manifest))
            z.writestr("photo.jpg", b"jpeg")
        msg = email.message.EmailMessage()
        if header:
            msg["X-RSMF-Version"] = "2.0.0"
        msg.set_content("slack export")
        msg.add_attachment(buf.getvalue(), maintype="application", subtype="zip", filename="rsmf.zip")
        path = os.path.join(self.dir, "chat.rsmf")
        with open(path, "wb") as f:
            f.write(bytes(msg))
        return path

    def test_rsmf(self):
        r = connectors.read_rsmf(self.rsmf(), self.out)
        self.assertEqual((r.items, r.verified, r.mismatches), (3, 3, []))
        self.assertTrue(os.path.isfile(os.path.join(self.out, "chat.rsmf")))
        self.assertTrue(os.path.isfile(os.path.join(self.out, "extracted", "photo.jpg")))
        self.assertFalse(os.path.exists(os.path.join(self.out, ".rsmf.zip")))

    def test_rsmf_finds_a_missing_attachment(self):
        r = connectors.read_rsmf(self.rsmf(("photo.jpg", "gone.jpg")), self.out)
        self.assertEqual(len(r.mismatches), 1)
        self.assertIn("gone.jpg", r.mismatches[0])

    def test_not_rsmf(self):
        with self.assertRaises(ValueError):
            connectors.read_rsmf(self.rsmf(header=False), self.out)

    def test_ufdr(self):
        path = os.path.join(self.dir, "phone.ufdr")
        report = f"""<project xmlns="http://pa.cellebrite.com/report/2.0"><taggedFiles>
          <file fs="x"><metadata section="File"><item name="Local Path">files\\Image\\a.jpg</item><item name="SHA256">{sha(b'aa')}</item></metadata></file>
          <file fs="x"><metadata section="File"><item name="Local Path">files/Image/b.jpg</item><item name="MD5">{hashlib.md5(b'other').hexdigest()}</item></metadata></file>
          <file fs="x"><metadata section="File"><item name="Local Path">files/Image/c.jpg</item><item name="SHA256">abc</item></metadata></file>
        </taggedFiles></project>"""
        with zipfile.ZipFile(path, "w") as z:
            z.writestr("report.xml", report)
            z.writestr("files/Image/a.jpg", b"aa")
            z.writestr("files/Image/b.jpg", b"bb")
        r = connectors.read_ufdr(path, self.out)
        self.assertEqual((r.items, r.verified), (3, 1))
        self.assertEqual(len(r.mismatches), 2)
        self.assertTrue(os.path.isfile(os.path.join(self.out, "files", "Image", "b.jpg")))
        self.assertTrue(os.path.isfile(os.path.join(self.out, "report.xml")))

    def test_manifest_csv(self):
        os.makedirs(os.path.join(self.dir, "evidence"))
        with open(os.path.join(self.dir, "evidence", "a.bin"), "wb") as f:
            f.write(b"aa")
        path = os.path.join(self.dir, "hashes.csv")
        with open(path, "w", encoding="utf-8", newline="") as f:
            f.write("Path,SHA256\nevidence/a.bin," + sha(b"aa") + "\nevidence/gone.bin,abc\n")
        r = connectors.read_manifest(path, self.out)
        self.assertEqual((r.items, r.verified), (2, 1))
        self.assertIn("not there", r.mismatches[0])

    def test_paths_stay_inside(self):
        self.assertEqual(connectors._safe("../../etc/passwd"), os.path.join("etc", "passwd"))
        with self.assertRaises(ValueError):
            connectors._safe("../..")


class Promote(unittest.TestCase):
    def test_lfs_files_parses_git_lfs(self):
        orig = connectors.subprocess.run
        class Done:
            stdout = "a" * 64 + " * art/hero.psd\n" + "b" * 64 + " - build/game.pak\nnoise\n"
        connectors.subprocess.run = lambda *a, **k: Done
        try:
            self.assertEqual(connectors.lfs_files(), [("art/hero.psd", "a" * 64), ("build/game.pak", "b" * 64)])
        finally:
            connectors.subprocess.run = orig


if __name__ == "__main__":
    unittest.main()
