import hashlib
import http.server
import json
import os
import tempfile
import threading
import unittest

from osp import ingest, snapshots
from osp.client import APIError, Client


def sha(data):
    return hashlib.sha256(data).hexdigest()


class Server:
    """A local HTTP server answering with handler(method, path, body, headers)."""

    def __init__(self, handler):
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def _do(self):
                length = int(self.headers.get("Content-Length") or 0)
                body = self.rfile.read(length) if length else b""
                status, payload, extra = handler(self.command, self.path, body, self.headers)
                self.send_response(status)
                for k, v in (extra or {}).items():
                    self.send_header(k, v)
                data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = do_PUT = _do

            def log_message(self, *args):
                pass

        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        outer.calls = []

    def close(self):
        self.httpd.shutdown()


class IngestTest(unittest.TestCase):
    def test_scan_and_manifest(self):
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(os.path.join(d, "a", "b"))
            with open(os.path.join(d, "a", "b", "x.csv"), "wb") as f:
                f.write(b"id\n1\n")
            open(os.path.join(d, "empty.txt"), "wb").close()
            files = ingest.scan_folder(d)
            self.assertEqual([f.path for f in files], ["a/b/x.csv"], "empty files are skipped, paths use /")
            ingest.hash_files(files)
            line = json.loads(ingest.build_manifest(files).decode().splitlines()[0])
            self.assertEqual(line["checksum"], "sha256:" + sha(b"id\n1\n"))
            self.assertEqual(line["size"], 5)
            self.assertEqual(line["content_type"], "text/csv")


class SnapshotTest(unittest.TestCase):
    def setUp(self):
        self.files = {"a.txt": b"alpha", "dir/b.txt": b"bravo"}
        self.corrupt = set()

        names = sorted(self.files)

        def storage(method, path, body, headers):
            name = names[int(path.lstrip("/"))]
            data = b"tampered" if name in self.corrupt else self.files[name]
            return 200, data, None

        self.storage = Server(storage)
        members = [{"path": p, "version_id": f"asset{i}", "size_bytes": len(self.files[p]),
                    "checksum_sha256": sha(self.files[p]), "availability": "AVAILABLE"} for i, p in enumerate(names)]
        self.members = members
        storage_url = self.storage.url

        class FakeClient:
            base_url = "http://api"

            def get(self, path, **params):
                return {"members": members}

            def post(self, path, body=None, headers=None):
                asset = path.split("/")[4]  # /api/v1/assets/<id>/download-url
                return {"url": storage_url + "/" + asset[len("asset"):]}

        self.client = FakeClient()

    def tearDown(self):
        self.storage.close()

    def test_verify_reports_every_difference(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "a.txt"), "wb") as f:
                f.write(b"ALPHA")  # changed
            with open(os.path.join(d, "extra.txt"), "wb") as f:
                f.write(b"x")
            r = snapshots.verify_local(self.client, "ds", "v1", d)
            self.assertEqual(r["changed"], ["a.txt"])
            self.assertEqual(r["missing"], ["dir/b.txt"])
            self.assertEqual(r["extra"], ["extra.txt"])
            self.assertEqual(r["matched"], [])

    def test_download_verifies_and_resumes(self):
        with tempfile.TemporaryDirectory() as d:
            counts = snapshots.download(self.client, "ds", "v1", d)
            self.assertEqual((counts["downloaded"], counts["kept"]), (2, 0))
            with open(os.path.join(d, "dir", "b.txt"), "rb") as f:
                self.assertEqual(f.read(), b"bravo")
            counts = snapshots.download(self.client, "ds", "v1", d)
            self.assertEqual((counts["downloaded"], counts["kept"]), (0, 2), "files already present with the pinned hash are kept")
            r = snapshots.verify_local(self.client, "ds", "v1", d)
            self.assertEqual(sorted(r["matched"]), ["a.txt", "dir/b.txt"])

    def test_erased_members_are_reported_and_local_copies_removed(self):
        with tempfile.TemporaryDirectory() as d:
            snapshots.download(self.client, "ds", "v1", d)
            self.members[0]["availability"] = "ERASED"
            r = snapshots.verify_local(self.client, "ds", "v1", d)
            self.assertEqual(r["erased_but_present"], ["a.txt"], "a local copy still holds erased data")
            self.assertEqual(r["missing"], [])
            counts = snapshots.download(self.client, "ds", "v1", d, remove_erased=True)
            self.assertEqual(counts["removed"], 1)
            self.assertFalse(os.path.exists(os.path.join(d, "a.txt")))
            r = snapshots.verify_local(self.client, "ds", "v1", d)
            self.assertEqual((r["erased"], r["erased_but_present"], r["missing"]), (["a.txt"], [], []))

    def test_download_refuses_tampered_bytes(self):
        self.corrupt.add("a.txt")
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(snapshots.IntegrityError):
                snapshots.download(self.client, "ds", "v1", d)
            self.assertFalse(os.path.exists(os.path.join(d, "a.txt")), "a file that fails its hash is never written")
            self.assertEqual([n for n in os.listdir(d) if n.startswith(".osp-")], [], "no temporary file is left")


class ClientTest(unittest.TestCase):
    def test_refreshes_once_on_401_and_retries_429(self):
        state = {"refreshes": 0, "busy": 1}

        def api(method, path, body, headers):
            if path == "/api/v1/auth/refresh":
                state["refreshes"] += 1
                return 200, {"access_token": "new", "refresh_token": "r2"}, None
            if headers.get("Authorization") != "Bearer new":
                return 401, {"error": "expired"}, None
            if state["busy"]:
                state["busy"] -= 1
                return 429, {"error": "slow down"}, {"Retry-After": "0"}
            return 200, {"ok": True}, None

        server = Server(api)
        saved = []
        try:
            c = Client(server.url, access_token="old", refresh_token="r1", on_tokens=saved.append)
            self.assertEqual(c.get("/api/v1/thing"), {"ok": True})
            self.assertEqual(state["refreshes"], 1)
            self.assertEqual(c.refresh_token, "r2")
            self.assertEqual(len(saved), 1, "new tokens are handed to on_tokens")
        finally:
            server.close()

    def test_errors_carry_status_and_code(self):
        server = Server(lambda *a: (404, {"error": "not found", "code": "dataset_not_found"}, None))
        try:
            with self.assertRaises(APIError) as ctx:
                Client(server.url, access_token="t").get("/api/v1/datasets/x")
            self.assertEqual(ctx.exception.status, 404)
            self.assertEqual(ctx.exception.code, "dataset_not_found")
        finally:
            server.close()

class SafePathTest(unittest.TestCase):
    def test_paths_that_could_leave_the_folder_are_refused(self):
        self.assertEqual(snapshots.safe_parts("a/b.txt"), ["a", "b.txt"])
        for bad in ("../x", "a/../../x", "/etc/passwd", "a//b", "C:/x", r"a\..\x", "./a"):
            with self.assertRaises(ValueError, msg=bad):
                snapshots.safe_parts(bad)


if __name__ == "__main__":
    unittest.main()
