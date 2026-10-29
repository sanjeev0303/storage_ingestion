import argparse
import contextlib
import io
import sys
import types
import unittest

from osp import cli, runs


class FakeClient:
    """Answers like the API and keeps every call."""

    def __init__(self):
        self.calls = []

    def post(self, path, body=None, headers=None):
        self.calls.append(("POST", path, body))
        if path == "/api/v1/training-runs":
            return {"id": "run-1", **body}
        if path.endswith("/merges"):
            return {"applied": False, "added": 1, "updated": 0, "removed": 0, "already_same": 0, "conflicts": 0, "conflict_list": []}
        return {"id": "branch-1", "branch": {"name": body["name"]}, "member_count": 2}

    def request(self, method, path, body=None, **kwargs):
        self.calls.append((method, path, body))
        return {"id": path.rsplit("/", 1)[-1], **body}

    def get(self, path, **params):
        self.calls.append(("GET", path, params))
        if path.endswith("/branches"):
            return {"branches": [{"id": "b-1", "member_count": 2, "branch": {"name": "exp", "merge_base_version_id": "v-1"}}]}
        return {"datasets": [{"id": "ds-1", "name": "pets"}]}

    def pages(self, path, key, **params):
        self.calls.append(("PAGES", path, params))
        if path == "/api/v1/datasets":
            return iter([{"id": "ds-1", "name": "pets"}])
        return iter([])


class RunsTest(unittest.TestCase):
    def test_track_run_records_then_finishes_with_metrics_and_model(self):
        client = FakeClient()
        stream = types.SimpleNamespace(version_id="v-2")
        with runs.track_run(client, "resnet", versions=["v-1", stream], params={"lr": 0.01}, source="SDK") as run:
            run.log_metrics(accuracy=0.9)
            run.log_model("resnet", 3, uri="s3://models/resnet/3")
        method, path, body = client.calls[0]
        self.assertEqual((method, path), ("POST", "/api/v1/training-runs"))
        self.assertEqual(body["dataset_version_ids"], ["v-1", "v-2"])
        self.assertEqual(body["params"], {"lr": "0.01"})
        method, path, body = client.calls[1]
        self.assertEqual((method, path), ("PATCH", "/api/v1/training-runs/run-1"))
        self.assertEqual(body["status"], "FINISHED")
        self.assertEqual(body["metrics"], {"accuracy": 0.9})
        self.assertEqual(body["model"], {"name": "resnet", "version": "3", "uri": "s3://models/resnet/3"})

    def test_a_failing_block_finishes_the_run_as_failed(self):
        client = FakeClient()
        with self.assertRaises(ValueError):
            with runs.track_run(client, "broken", versions=["v-1"], source="SDK"):
                raise ValueError("diverged")
        self.assertEqual(client.calls[-1][2]["status"], "FAILED")

    def test_an_active_mlflow_run_is_recorded_as_its_source(self):
        info = types.SimpleNamespace(run_id="abc123", experiment_id="7")
        fake = types.ModuleType("mlflow")
        fake.active_run = lambda: types.SimpleNamespace(info=info)
        fake.get_tracking_uri = lambda: "https://mlflow.example.com/"
        sys.modules["mlflow"] = fake
        try:
            client = FakeClient()
            runs.record_run(client, "from-mlflow", ["v-1"])
        finally:
            del sys.modules["mlflow"]
        body = client.calls[0][2]
        self.assertEqual(body["source"], "MLFLOW")
        self.assertEqual(body["external_id"], "abc123")
        self.assertEqual(body["url"], "https://mlflow.example.com/#/experiments/7/runs/abc123")


class CliBranchCommandsTest(unittest.TestCase):
    """osp.branches is also a function's name; the CLI must call the module's functions."""

    def run_cli(self, fn, **kwargs):
        client = FakeClient()
        original = cli.client_from_config
        cli.client_from_config = lambda: client
        out = io.StringIO()
        try:
            with contextlib.redirect_stdout(out):
                fn(argparse.Namespace(**kwargs))
        finally:
            cli.client_from_config = original
        return client, out.getvalue()

    def test_branch_branches_and_merge_commands_run(self):
        _, out = self.run_cli(cli.cmd_branches, dataset="pets")
        self.assertIn("exp", out)
        client, out = self.run_cli(cli.cmd_merge, target="pets", source="pets", version=None, strategy="FAIL", dry_run=True)
        self.assertTrue(any(path.endswith("/merges") for _, path, _ in client.calls))
        self.assertIn("would change 1 added", out)


if __name__ == "__main__":
    unittest.main()
