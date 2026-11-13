import inspect
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

from osp import ingest
from osp.client import Client

from test_sdk import Server


def finishes(test, work, seconds=10):
    """Runs work in a thread and fails the test if it is still running after
    seconds, so a wait that never ends fails instead of hanging the suite."""
    result = {}

    def run():
        try:
            result["value"] = work()
        except BaseException as err:
            result["error"] = err

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    thread.join(seconds)
    test.assertFalse(thread.is_alive(), f"still waiting after {seconds}s")
    if "error" in result:
        raise result["error"]
    return result.get("value")


def local_file(folder, name, data):
    disk = os.path.join(folder, name)
    with open(disk, "wb") as f:
        f.write(data)
    local = ingest.LocalFile(name, disk, len(data), "application/octet-stream")
    local.sha256 = "abc"
    return local


def serve(test, api):
    server = Server(api)
    test.addCleanup(server.close)
    return server, Client(server.url, access_token="t")


class SingleUploadTest(unittest.TestCase):
    def test_a_changed_file_whose_session_ended_starts_once_more_under_a_new_key(self):
        keys, completed = [], []

        def api(method, path, body, headers):
            if path == "/api/v1/uploads":
                keys.append(json.loads(body)["idempotency_key"])
                if len(keys) == 1:
                    return 409, {"code": "session_ended", "error": "upload session is not active: it has ended; "
                                                                    "start again with a new idempotency key"}, None
                return 201, {"upload_id": "new", "status": "PENDING", "upload_url": server.url + "/put/new"}, None
            if method == "PUT":
                return 200, b"", {"ETag": '"e"'}
            if path.endswith("/complete"):
                completed.append(path)
            return 200, {}, None

        server, client = serve(self, api)
        with tempfile.TemporaryDirectory() as d:
            failed = ingest._upload_new_versions(client, "d", [local_file(d, "a.bin", b"x" * 10)], 1, False,
                                                 lambda message: None)
        self.assertEqual(failed, [])
        self.assertEqual(len(keys), 2)
        self.assertNotEqual(keys[0], keys[1])
        self.assertEqual(completed, ["/api/v1/uploads/new/complete"])

    def test_a_changed_file_whose_upload_already_completed_is_not_sent_again(self):
        calls = []

        def api(method, path, body, headers):
            calls.append(f"{method} {path}")
            if path == "/api/v1/uploads":
                return 200, {"upload_id": "done", "status": "COMPLETED", "upload_url": ""}, None
            return 400, {"error": "unexpected"}, None

        server, client = serve(self, api)
        with tempfile.TemporaryDirectory() as d:
            failed = ingest._upload_new_versions(client, "d", [local_file(d, "a.bin", b"x" * 10)], 1, False,
                                                 lambda message: None)
        self.assertEqual(failed, [])
        self.assertEqual(calls, ["POST /api/v1/uploads"])


class WaitTest(unittest.TestCase):
    def test_an_entry_that_failed_is_claimed_and_sent_again(self):
        # The entry goes back to PENDING once its lease runs out, after the
        # client has stopped claiming, and only a claim sends it again.
        state = {"pending": 1, "puts": 0, "job": "RUNNING", "lease": False}
        lock = threading.Lock()

        def api(method, path, body, headers):
            with lock:
                if path == "/api/v1/ingestion/jobs":
                    return 201, {"id": "j1", "state": "RUNNING", "manifest_complete": True}, None
                if path.endswith("/claims"):
                    if state["pending"] == 0:
                        return 200, {"grants": [], "limited_by": "no_pending_entries"}, None
                    state["pending"] -= 1
                    entry = {"position": 0, "path": "a.bin", "strategy": "SINGLE", "content_type": "text/plain"}
                    return 200, {"grants": [{"entry": entry, "upload_url": server.url + "/put/a"}]}, None
                if method == "PUT":
                    state["puts"] += 1
                    if state["puts"] == 1:
                        state["lease"] = True
                        return 400, b"refused", None
                    return 200, b"", {"ETag": '"e"'}
                if path.endswith("/entries/0/complete"):
                    state["job"] = "COMPLETED"
                    return 200, {}, None
                if path == "/api/v1/ingestion/jobs/j1":
                    if state["lease"]:
                        state["lease"], state["pending"] = False, 1
                    return 200, {"id": "j1", "state": state["job"], "progress": {"pending": state["pending"]}}, None
                return 404, {"error": "no route"}, None

        server, client = serve(self, api)
        with tempfile.TemporaryDirectory() as d, mock.patch("time.sleep"):
            local = local_file(d, "a.bin", b"hello")
            job = finishes(self, lambda: ingest._ingest_new(client, {"id": "ds"}, [local], 2, True, lambda m: None))
        self.assertEqual(job["state"], "COMPLETED")
        self.assertEqual(state["puts"], 2)

    def test_waiting_for_a_new_version_stops_at_blocked(self):
        def api(method, path, body, headers):
            if path == "/api/v1/uploads":
                return 201, {"upload_id": "u1", "upload_url": server.url + "/put/u1"}, None
            if method == "PUT":
                return 200, b"", {"ETag": '"e"'}
            if method == "GET":
                return 200, {"status": "VERIFIED", "asset": {"status": "BLOCKED"}}, None
            return 200, {}, None

        server, client = serve(self, api)
        with tempfile.TemporaryDirectory() as d, mock.patch("time.sleep"):
            local = local_file(d, "a.bin", b"x")
            failed = finishes(self, lambda: ingest._upload_new_versions(client, "d", [local], 1, True, lambda m: None))
        self.assertEqual(failed, ["a.bin: BLOCKED"])

    def test_waiting_for_a_new_version_gives_up_at_its_deadline(self):
        def api(method, path, body, headers):
            if path == "/api/v1/uploads":
                return 201, {"upload_id": "u1", "upload_url": server.url + "/put/u1"}, None
            if method == "PUT":
                return 200, b"", {"ETag": '"e"'}
            if method == "GET":
                return 200, {"status": "VERIFIED", "asset": {"status": "PROCESSING"}}, None
            return 200, {}, None

        server, client = serve(self, api)
        with tempfile.TemporaryDirectory() as d, mock.patch("time.sleep"):
            local = local_file(d, "a.bin", b"x")
            failed = finishes(self, lambda: ingest._upload_new_versions(client, "d", [local], 1, True, lambda m: None,
                                                                         timeout=0))
        self.assertEqual(len(failed), 1)
        self.assertIn("still PROCESSING", failed[0])

    def test_pinning_members_gives_up_at_its_deadline(self):
        def api(method, path, body, headers):
            if path.startswith("/api/v1/catalog/objects"):
                version = {"asset_id": "v1", "checksum_sha256": "abc"}
                return 200, {"objects": [{"path": "a.bin", "current_version": version}]}, None
            return 200, {"id": "i1", "state": "RUNNING"}, None

        server, client = serve(self, api)
        with tempfile.TemporaryDirectory() as d, mock.patch("time.sleep"), self.assertRaises(TimeoutError):
            local = local_file(d, "a.bin", b"x")
            finishes(self, lambda: ingest.pin_members(client, {"id": "ds", "name": "d"}, [local], timeout=0))

    def test_waiting_has_a_deadline_by_default(self):
        self.assertIsNotNone(getattr(ingest, "WAIT_TIMEOUT", None))
        for fn in (ingest.ingest_folder, ingest.wait_for_job, ingest.pin_members):
            self.assertEqual(inspect.signature(fn).parameters["timeout"].default, ingest.WAIT_TIMEOUT, fn.__name__)


class PartsTest(unittest.TestCase):
    def multipart_api(self, refuse_part=None, slow_part=None):
        puts = []

        def api(method, path, body, headers):
            if path.endswith("/parts/urls"):
                numbers = json.loads(body)["part_numbers"]
                return 200, {"parts": [{"part_number": n, "url": f"{server.url}/put/{n}"} for n in numbers]}, None
            if method == "PUT":
                n = int(path.rsplit("/", 1)[1])
                puts.append(n)
                if n == slow_part:
                    time.sleep(0.5)
                if n == refuse_part:
                    return 400, b"refused", None
                return 200, b"", {"ETag": f'"e{n}-{len(puts)}"'}
            return 200, {}, None

        server, client = serve(self, api)
        return client, puts

    def test_a_reclaimed_entry_does_not_reuse_the_parts_of_its_old_upload(self):
        with tempfile.TemporaryDirectory() as d, mock.patch.object(ingest, "PART_SIZE", 4):
            local = local_file(d, "a.bin", b"12345678")
            resume = ingest.ResumeFile(os.path.join(d, "uploads.json"))
            entry = {"position": 0, "path": "a.bin", "strategy": "MULTIPART"}
            client, puts = self.multipart_api(refuse_part=2)
            with self.assertRaises(RuntimeError):
                ingest._upload_one(client, "/jobs/j1", {"a.bin": local}, {"entry": {**entry, "upload_id": "old"}}, resume)

            client, puts = self.multipart_api()
            ingest._upload_one(client, "/jobs/j1", {"a.bin": local}, {"entry": {**entry, "upload_id": "new"}}, resume)
        self.assertEqual(sorted(puts), [1, 2], "a new upload session sends every part")

    def test_the_first_failed_part_stops_the_parts_not_yet_started(self):
        with tempfile.TemporaryDirectory() as d, mock.patch.object(ingest, "PART_SIZE", 1):
            local = local_file(d, "a.bin", b"x" * 40)
            client, puts = self.multipart_api(refuse_part=2, slow_part=1)
            with self.assertRaises(RuntimeError):
                ingest._send_parts(client, "/api/v1/uploads/u1", local, {})
        self.assertLess(len(puts), 20, f"{len(puts)} of 40 parts were sent after the first failed")


class ResumeFileTest(unittest.TestCase):
    def test_processes_saving_at_once_keep_every_part(self):
        sdk = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        code = ("import sys; from osp.ingest import ResumeFile\n"
                "r = ResumeFile(sys.argv[1])\n"
                "for n in range(1, 31): r.save_part(sys.argv[2], n, 'e', 1)\n")
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "uploads.json")
            procs = [subprocess.Popen([sys.executable, "-c", code, path, f"key{i}"], cwd=sdk,
                                      stderr=subprocess.PIPE) for i in range(4)]
            errors = [p.communicate(timeout=60)[1].decode() for p in procs]
            self.assertEqual([p.returncode for p in procs], [0] * 4, errors)
            resume = ingest.ResumeFile(path)
            for i in range(4):
                self.assertEqual(len(resume.parts(f"key{i}")), 30, f"key{i}")


if __name__ == "__main__":
    unittest.main()
