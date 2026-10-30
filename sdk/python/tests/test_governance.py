import argparse
import base64
import contextlib
import hashlib
import io
import unittest

from osp import cli, governance


def cert(signed, signature=""):
    return {"signed_body": signed, "body_sha256": hashlib.sha256(signed.encode()).hexdigest(), "signature": signature,
            "body": {"file_name": "old.txt"}}


class FakeClient:
    def __init__(self):
        self.calls = []

    def request(self, method, path, body=None, params=None, **kwargs):
        self.calls.append((method, path, body, params))
        if body and body.get("dry_run"):
            return {"dry_run": True, "impact": {"assets": 3}}
        return {"status": "PENDING_APPROVAL", "approval_id": "ap-1"}

    def get(self, path, **params):
        self.calls.append(("GET", path, None, params))
        if path.endswith("/proof"):
            return {"truncated": False, "assets": [{"asset_id": "a1", "file_name": "x", "lock_applied_at": "t",
                                                    "lock_legal_hold": True, "provider": "MISMATCH: legal hold is off"}]}
        return {"approvals": [], "holds": [], "templates": []}

    def post(self, path, body=None, headers=None):
        self.calls.append(("POST", path, body, None))
        if path.endswith("/bulk-delete"):
            return {"matched": 2, "bytes": 10, "blocked": {"legal_hold": 1}, "checked": 2, "scheduled": 0, "sample": []}
        return {"state": "EXECUTED", "summary": "release legal hold h-1", "result": "hold h-1 released"}


class GovernanceTest(unittest.TestCase):
    def run_cli(self, fn, **kwargs):
        client = FakeClient()
        original = cli.client_from_config
        cli.client_from_config = lambda: client
        out = io.StringIO()
        try:
            with contextlib.redirect_stdout(out):
                code = fn(argparse.Namespace(**kwargs))
        finally:
            cli.client_from_config = original
        return client, out.getvalue(), code

    def test_set_policy_sends_only_what_is_set(self):
        client = FakeClient()
        answer = governance.set_policy(client, "tenant", "17a-4", template="sec-17a-4", dry_run=True)
        self.assertEqual(answer["impact"]["assets"], 3)
        self.assertEqual(client.calls[0][2], {"scope": "TENANT", "reason": "17a-4", "template": "sec-17a-4", "dry_run": True})

    def test_hold_proof_fails_on_a_provider_mismatch(self):
        _, out, code = self.run_cli(cli.cmd_hold_proof, hold="h-1", verify=True)
        self.assertEqual(code, 1)
        self.assertIn("MISMATCH", out)

    def test_bulk_delete_is_a_dry_run_unless_yes(self):
        client, out, _ = self.run_cli(cli.cmd_bulk_delete, reason="old", asset=["a1", "a2"], dataset=None, prefix=None, yes=False)
        self.assertTrue(client.calls[0][2]["dry_run"])
        self.assertIn("1 by legal hold", out)
        self.assertIn("dry run", out)

    def test_approve(self):
        client, out, code = self.run_cli(cli.cmd_decide, command="approve", approval="ap-1", note="ok")
        self.assertEqual((client.calls[0][1], code), ("/api/v1/approvals/ap-1/approve", 0))
        self.assertIn("EXECUTED", out)

    def test_verify_certificate(self):
        self.assertEqual(governance.verify_certificate(cert("{}")), (True, "unsigned"))
        tampered = cert("{}")
        tampered["signed_body"] = '{"x":1}'
        self.assertFalse(governance.verify_certificate(tampered)[0])
        try:
            from cryptography.hazmat.primitives import serialization
            from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        except ImportError:
            self.skipTest("cryptography is not installed")
        key = Ed25519PrivateKey.generate()
        signed = '{"type":"disposition-certificate/v1"}'
        sig = base64.b64encode(key.sign(signed.encode())).decode()
        pub = base64.b64encode(key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)).decode()
        self.assertEqual(governance.verify_certificate(cert(signed, sig), pub), (True, "valid"))
        forged = cert(signed + " ", sig)
        self.assertEqual(governance.verify_certificate(forged, pub)[1], "invalid")


if __name__ == "__main__":
    unittest.main()
