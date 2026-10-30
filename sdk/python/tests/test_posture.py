import contextlib
import io
import unittest
from unittest import mock

from osp import cli, posture


class FakeClient:
    def __init__(self):
        self.calls = []

    def request(self, method, path, body=None, params=None, **kwargs):
        self.calls.append((method, path, body))
        return {"id": "ds1", "name": "scans", "data_class": (body or {}).get("data_class", ""), "credential_rotation": 1}

    def get(self, path, **params):
        self.calls.append(("GET", path, params))
        if path.endswith("/posture"):
            return {"worst": "FAIL", "checked_at": "2026-10-30T10:00:00Z", "phi_safe": False,
                    "checks": [{"name": "sse_c_denied", "status": "FAIL", "detail": "accepted"}]}
        if "/datasets" in path and "/versions" not in path:
            return {"datasets": [{"id": "ds1", "name": "scans", "revision": 4}]}
        return {"findings": [{"id": "f1", "detected_at": "2026-10-30T10:00:00Z", "kind": "OVERWRITTEN",
                              "object_key": "ready/t/a", "detail": "replaced"}]}

    def post(self, path, body=None, headers=None):
        self.calls.append(("POST", path, body))
        return {"connection": {"id": "c1", "credential_rotation": 2}}


class PostureTest(unittest.TestCase):
    def test_paths_and_bodies(self):
        c = FakeClient()
        posture.check(c, "c1", probe=True)
        self.assertEqual(c.calls[-1], ("POST", "/api/v1/storage-connections/c1/posture", {"probe": True}))
        posture.stage_credentials(c, "c1", access_key_id="AK", secret_access_key="s")
        self.assertEqual(c.calls[-1][2], {"credentials": {"access_key_id": "AK", "secret_access_key": "s"}})
        posture.drift(c)
        self.assertEqual(c.calls[-1][1], "/api/v1/storage-drift")
        posture.drift(c, "c1", everything=True)
        self.assertEqual(c.calls[-1], ("GET", "/api/v1/storage-connections/c1/drift", {"all": "true"}))

    def test_cli_exit_codes(self):
        c = FakeClient()
        with mock.patch.object(cli, "client_from_config", return_value=c), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(cli.main(["posture", "c1"]), 1)
            self.assertEqual(cli.main(["drift", "c1"]), 1)

    def test_stage_reads_the_secret_without_echo(self):
        c = FakeClient()
        with mock.patch.object(cli, "client_from_config", return_value=c), \
                mock.patch.dict("os.environ", {"OSP_STORAGE_SECRET": "from-env"}), \
                contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()):
            cli.main(["credentials", "stage", "c1", "--access-key-id", "AK"])
        self.assertEqual(c.calls[-1][2]["credentials"]["secret_access_key"], "from-env")
        self.assertNotIn("from-env", out.getvalue())



class ResidencyTest(unittest.TestCase):
    def test_residency_body_and_exit_code(self):
        c = FakeClient()
        posture.set_residency(c, "ds1", 4, ["ap-south-1"], require_customer_key=True)
        self.assertEqual(c.calls[-1], ("PATCH", "/api/v1/datasets/ds1",
                                       {"revision": 4, "residency": ["ap-south-1"], "require_customer_key": True}))
        posture.set_residency(c, "ds1", 5, [])
        self.assertEqual(c.calls[-1][2], {"revision": 5, "residency": []})

    def test_upload_headers_are_sent(self):
        from osp import client as osp_client
        sent = {}

        class Resp:
            headers = {"ETag": '"e"'}

            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

            def read(self):
                return b""

        def fake_urlopen(req, timeout=0):
            sent.update(dict(req.header_items()))
            return Resp()

        with mock.patch.object(osp_client.urllib.request, "urlopen", fake_urlopen):
            osp_client.put_bytes("https://store/x", b"abc", 3, "text/plain",
                                 headers={"x-amz-server-side-encryption": "aws:kms"})
        self.assertEqual(sent.get("X-amz-server-side-encryption"), "aws:kms")
        self.assertEqual(sent.get("Content-length"), "3")


if __name__ == "__main__":
    unittest.main()
