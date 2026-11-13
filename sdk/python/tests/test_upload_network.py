import json
import os
import tempfile
import threading
import time
import unittest
from unittest import mock

from osp import ingest
from osp.client import APIError, Client, backoff, put_bytes

from test_sdk import Server


class PutBytesTest(unittest.TestCase):
    def serve(self, answers):
        """Answers each PUT with the next (status, body, headers), then 200."""
        calls = []

        def handler(method, path, body, headers):
            calls.append(path)
            if answers:
                status, payload, extra, pause = answers.pop(0)
                time.sleep(pause)
                return status, payload, extra
            return 200, b"", {"ETag": '"ok"'}

        server = Server(handler)
        self.addCleanup(server.close)
        return server, calls

    def test_a_stalled_put_is_dropped_and_sent_again(self):
        server, calls = self.serve([(200, b"", {"ETag": '"late"'}, 1.0)])
        with mock.patch("osp.client.time"):
            etag = put_bytes(server.url + "/p", b"abc", 3, timeout=0.3)
        self.assertEqual(etag, '"ok"')
        self.assertEqual(len(calls), 2)

    def test_an_expired_url_is_replaced_without_waiting(self):
        server, calls = self.serve([(403, b"<Message>Request has expired</Message>", None, 0)])
        with mock.patch("osp.client.time") as clock:
            put_bytes(server.url + "/old", b"abc", 3, refresh=lambda: server.url + "/new")
        self.assertEqual(calls, ["/old", "/new"])
        clock.sleep.assert_not_called()

    def test_retry_after_is_respected_and_a_refusal_is_not_retried(self):
        server, calls = self.serve([(503, b"slow down", {"Retry-After": "7"}, 0)])
        with mock.patch("osp.client.time") as clock:
            put_bytes(server.url + "/p", b"abc", 3)
        clock.sleep.assert_called_once_with(7.0)

        server, calls = self.serve([(400, b"bad digest", None, 0)])
        with mock.patch("osp.client.time") as clock, self.assertRaises(RuntimeError):
            put_bytes(server.url + "/p", b"abc", 3)
        self.assertEqual(len(calls), 1)
        clock.sleep.assert_not_called()

    def test_backoff_is_full_jitter_up_to_thirty_seconds(self):
        self.assertEqual(backoff(0, rand=lambda: 0.0), 0.0)
        self.assertEqual(backoff(3, rand=lambda: 0.5), 4.0)
        self.assertLess(backoff(20, rand=lambda: 0.999), 30.0)


class ResumeTest(unittest.TestCase):
    def test_a_rerun_sends_only_the_parts_not_confirmed(self):
        data = os.urandom(ingest.PART_SIZE * 3 + 100)
        state = {"broken": True, "puts": [], "confirmed": [], "completed": None}
        lock = threading.Lock()

        def api(method, path, body, headers):
            if path.endswith("/parts/urls"):
                numbers = json.loads(body)["part_numbers"]
                return 200, {"parts": [{"part_number": n, "url": f"{server.url}/put/{n}"} for n in numbers]}, None
            if method == "PUT":
                n = int(path.rsplit("/", 1)[1])
                with lock:
                    state["puts"].append(n)
                if state["broken"] and n == 3:
                    return 500, b"", None
                return 200, b"", {"ETag": f'"e{n}"'}
            if path.endswith("/parts/confirm"):
                state["confirmed"] += [p["part_number"] for p in json.loads(body)["parts"]]
            if path.endswith("/complete"):
                state["completed"] = json.loads(body)["parts"]
            return 200, {}, None

        server = Server(api)
        self.addCleanup(server.close)
        with tempfile.TemporaryDirectory() as d:
            disk = os.path.join(d, "big.bin")
            with open(disk, "wb") as f:
                f.write(data)
            local = ingest.LocalFile("big.bin", disk, len(data), "application/octet-stream")
            local.sha256 = "abc"
            resume = ingest.ResumeFile(os.path.join(d, "state", "uploads.json"))
            client = Client(server.url, access_token="t")

            with mock.patch("osp.client.time"), self.assertRaises(RuntimeError):
                ingest._send_parts(client, "/api/v1/uploads/u1", local, {}, resume)
            self.assertEqual(sorted(resume.parts("/api/v1/uploads/u1:abc:" + str(ingest.PART_SIZE))), ["1", "2", "4"])

            state["broken"] = False
            state["puts"].clear()
            ingest._send_parts(client, "/api/v1/uploads/u1", local, {}, ingest.ResumeFile(resume.path))
            self.assertEqual(state["puts"], [3], "only the part that never went through")
            self.assertEqual([p["part_number"] for p in state["completed"]], [1, 2, 3, 4])
            self.assertEqual(resume.parts("/api/v1/uploads/u1:abc:" + str(ingest.PART_SIZE)), {})


    def test_a_saved_session_that_ended_is_forgotten_when_its_links_are_refused(self):
        def api(method, path, body, headers):
            if path.endswith("/parts/urls"):
                return 409, {"error": "multipart upload is not active"}, None
            return 200, {}, None

        server = Server(api)
        self.addCleanup(server.close)
        with tempfile.TemporaryDirectory() as d:
            disk = os.path.join(d, "big.bin")
            with open(disk, "wb") as f:
                f.write(os.urandom(ingest.PART_SIZE + 10))
            local = ingest.LocalFile("big.bin", disk, ingest.PART_SIZE + 10, "application/octet-stream")
            local.sha256 = "abc"
            resume = ingest.ResumeFile(os.path.join(d, "uploads.json"))
            key = "/api/v1/uploads/u1:abc:" + str(ingest.PART_SIZE)
            resume.save_part(key, 1, '"e1"', ingest.PART_SIZE)

            with self.assertRaises(APIError):
                ingest._send_parts(Client(server.url, access_token="t"), "/api/v1/uploads/u1", local, {}, resume)
            self.assertEqual(resume.parts(key), {})

    def test_a_new_version_whose_session_ended_starts_once_more_under_a_new_key(self):
        starts = []
        completed = []

        def api(method, path, body, headers):
            if path == "/api/v1/uploads/multipart":
                key = json.loads(body)["idempotency_key"]
                starts.append(key)
                return 201, {"upload_id": "old" if len(starts) == 1 else "new"}, None
            if path.startswith("/api/v1/uploads/old/"):
                return 409, {"error": "multipart upload is not active"}, None
            if path.endswith("/parts/urls"):
                numbers = json.loads(body)["part_numbers"]
                return 200, {"parts": [{"part_number": n, "url": f"{server.url}/put/{n}"} for n in numbers]}, None
            if method == "PUT":
                return 200, b"", {"ETag": '"e"'}
            if path.endswith("/complete"):
                completed.append(path)
            return 200, {}, None

        server = Server(api)
        self.addCleanup(server.close)
        with tempfile.TemporaryDirectory() as d:
            disk = os.path.join(d, "big.bin")
            with open(disk, "wb") as f:
                f.write(b"x" * 100)
            local = ingest.LocalFile("big.bin", disk, 100, "application/octet-stream")
            local.sha256 = "abc"
            with mock.patch.object(ingest, "SINGLE_UPLOAD_LIMIT", 10):
                failed = ingest._upload_new_versions(Client(server.url, access_token="t"), "d", [local], 1, False,
                                                     lambda message: None)

        self.assertEqual(failed, [])
        self.assertEqual(len(starts), 2)
        self.assertNotEqual(starts[0], starts[1])
        self.assertEqual(completed, ["/api/v1/uploads/new/complete"])


class RetryTest(unittest.TestCase):
    def test_the_refresh_call_is_never_sent_twice(self):
        client = Client("http://api.test", refresh_token="once")
        with mock.patch.object(client, "_roundtrip", side_effect=ConnectionResetError("reset")) as roundtrip,                 mock.patch("osp.client.time"), self.assertRaises(OSError):
            client.refresh()
        self.assertEqual(roundtrip.call_count, 1)
        self.assertEqual(client.refresh_token, "once")

    def test_only_requests_that_cannot_take_effect_twice_are_retried(self):
        client = Client("http://api.test", access_token="t", max_attempts=3)

        def tries(*args, **kwargs):
            with mock.patch.object(client, "_roundtrip", side_effect=ConnectionResetError("reset")) as roundtrip,                     mock.patch("osp.client.time"), self.assertRaises(OSError):
                client.request(*args, **kwargs)
            return roundtrip.call_count

        self.assertEqual(tries("POST", "/api/v1/ingestion/jobs/j/claims", {"max": 4}), 1)
        self.assertEqual(tries("GET", "/api/v1/uploads/u1"), 3)
        self.assertEqual(tries("POST", "/api/v1/datasets/d/manifests", headers={"Idempotency-Key": "k"}), 3)
        self.assertEqual(tries("POST", "/api/v1/uploads/multipart", {"idempotency_key": "k"}), 3)
        self.assertEqual(tries("POST", "/api/v1/uploads/u1/complete", retry=True), 3)

    def test_a_storage_access_denied_is_a_refusal_not_an_expired_url(self):
        calls = []

        def handler(method, path, body, headers):
            calls.append(path)
            return 403, b"<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>", None

        server = Server(handler)
        self.addCleanup(server.close)
        with mock.patch("osp.client.time"), self.assertRaises(RuntimeError):
            put_bytes(server.url + "/p", b"abc", 3, refresh=lambda: server.url + "/new")
        self.assertEqual(calls, ["/p"])


if __name__ == "__main__":
    unittest.main()
