import contextlib
import io
import os
import unittest
from unittest import mock

from osp import access, cli


class FakeClient:
    def __init__(self):
        self.calls = []

    def request(self, method, path, body=None, params=None, **kwargs):
        self.calls.append((method, path, body))
        return body or {}

    def get(self, path, **params):
        self.calls.append(("GET", path, params))
        if path == "/api/v1/access-policy":
            return {"links_enabled": True, "egress_action": "WARN", "mass_link_threshold": 300, "updated_by": "u"}
        return {"tokens": [], "links": [], "alerts": []}

    def post(self, path, body=None, headers=None):
        self.calls.append(("POST", path, body))
        if path.endswith("/revoke"):
            return {"revoked": 3}
        return {"id": "t1", "name": body["name"], "token": "osp_pat_secret", "expires_at": "2027-01-01T00:00:00Z"}


class AccessTest(unittest.TestCase):
    def test_bodies(self):
        c = FakeClient()
        access.create_token(c, "vendor", ["assets.read"], datasets=["scans"], expires_in="720h")
        self.assertEqual(c.calls[-1], ("POST", "/api/v1/api-tokens", {"name": "vendor", "permissions": ["assets.read"],
                         "datasets": ["scans"], "networks": [], "allow_links": False, "expires_in": "720h"}))
        self.assertEqual(access.revoke_links(c, asset_ids=["a1"]), 3)
        access.links(c, everyone=True)
        self.assertEqual(c.calls[-1][2], {"active": "true", "all": "true"})
        access.alerts(c, state="OPEN")
        self.assertEqual(c.calls[-1][2], {"state": "OPEN"})

    def test_policy_cli_keeps_unchanged_fields(self):
        c = FakeClient()
        with mock.patch.object(cli, "client_from_config", return_value=c), contextlib.redirect_stdout(io.StringIO()):
            cli.main(["access-policy", "--set", "egress_action=BLOCK", "--set", "egress_monthly_bytes=1000",
                      "--set", "links_enabled=false"])
        method, path, body = c.calls[-1]
        self.assertEqual((method, path), ("PUT", "/api/v1/access-policy"))
        self.assertEqual(body, {"links_enabled": False, "egress_action": "BLOCK", "mass_link_threshold": 300,
                                "egress_monthly_bytes": 1000})

    def test_token_is_printed_once_to_stdout(self):
        c = FakeClient()
        out = io.StringIO()
        with mock.patch.object(cli, "client_from_config", return_value=c), contextlib.redirect_stdout(out), \
                contextlib.redirect_stderr(io.StringIO()):
            cli.main(["token-create", "ci", "-p", "assets.read"])
        self.assertEqual(out.getvalue().strip(), "osp_pat_secret")

    def test_api_token_from_environment(self):
        with mock.patch.dict(os.environ, {"OSP_API_TOKEN": "osp_pat_x", "OSP_URL": "https://osp.example"}):
            client = cli.client_from_config()
        self.assertEqual((client.base_url, client.access_token, client.refresh_token), ("https://osp.example", "osp_pat_x", ""))


if __name__ == "__main__":
    unittest.main()
