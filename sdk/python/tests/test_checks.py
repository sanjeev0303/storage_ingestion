import argparse
import contextlib
import io
import unittest

from osp import checks, cli


class FakeClient:
    """Answers the content-check routes like the API."""

    def __init__(self):
        self.calls = []
        self.policy = {"finding_actions": {}, "allowed_types": [], "denied_types": [], "max_size_bytes": 0,
                       "unknown_type_action": ""}

    def get(self, path, **params):
        self.calls.append(("GET", path))
        if path == "/api/v1/assets/a-bad/findings":
            return {"findings": [{"code": "secret.aws_access_key", "action": "flag", "source": "secrets",
                                  "detail": "an AWS access key ID at byte 9 (1 found)"}]}
        if path.endswith("/findings"):
            return {"findings": []}
        if path.endswith("/blast-radius"):
            return {"checksum_sha256": "ab" * 32, "size_bytes": 5,
                    "assets": [{"asset_id": "a-bad", "status": "AVAILABLE", "file_name": "x.env", "blocked_reason": "malware_detected"}],
                    "snapshots": [{"dataset_name": "pets", "version": 2, "members": 1}],
                    "downloads": [{"actor_type": "USER", "actor_id": "u1", "links": 3, "last": "2026-10-30T10:00:00Z"}]}
        if path == "/api/v1/content-policy":
            return {"policy": self.policy}
        if path.endswith("/members"):
            return {"members": [{"path": "a.env", "version_id": "a-bad"}, {"path": "b.txt", "version_id": "a-ok"}]}
        return {"datasets": [{"id": "ds-1", "name": "pets"}]}

    def pages(self, path, key, **params):
        if path == "/api/v1/datasets":
            return iter([{"id": "ds-1", "name": "pets"}])
        if path.endswith("/versions"):
            return iter([{"id": "v-1", "version": 1, "member_count": 2}])
        return iter([])

    def request(self, method, path, body=None, **kwargs):
        self.calls.append((method, path, body))
        self.policy = body
        return {"policy": body}


class ChecksTest(unittest.TestCase):
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

    def test_version_findings_keeps_only_members_with_findings(self):
        found = checks.version_findings(FakeClient(), "ds-1", "v-1")
        self.assertEqual(list(found), ["a.env"])
        self.assertEqual(found["a.env"][0]["code"], "secret.aws_access_key")

    def test_findings_command_exits_nonzero_when_something_was_found(self):
        _, out, code = self.run_cli(cli.cmd_findings, target="a-bad", version=None)
        self.assertEqual(code, 1)
        self.assertIn("secret.aws_access_key", out)
        _, out, code = self.run_cli(cli.cmd_findings, target="a-ok", version=None)
        self.assertEqual((code, out.strip()), (0, "no findings"))

    def test_blast_radius_command_lists_copies_snapshots_and_downloads(self):
        _, out, _ = self.run_cli(cli.cmd_blast_radius, asset="a-bad")
        self.assertIn("BLOCKED", out)
        self.assertIn("pets v2", out)
        self.assertIn("3 links", out)

    def test_content_policy_set_and_clear_keep_the_rest(self):
        client, out, _ = self.run_cli(cli.cmd_content_policy, set=["secret=reject", "model.pickle_imports=allow"], clear=None)
        method, path, body = client.calls[-1]
        self.assertEqual((method, path), ("PUT", "/api/v1/content-policy"))
        self.assertEqual(body["finding_actions"], {"secret": "reject", "model.pickle_imports": "allow"})
        self.assertIn('"secret": "reject"', out)


if __name__ == "__main__":
    unittest.main()
