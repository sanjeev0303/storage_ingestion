"""Training-run lineage: which dataset versions a model was trained on.

    with track_run(client, "resnet50", versions=[version_id], params={"lr": "0.01"}) as run:
        ...train...
        run.log_metrics(accuracy=0.91)
        run.log_model("resnet50", "3", uri="s3://models/resnet50/3")

The run is recorded when the block starts and finished when it ends: as
FINISHED, or as FAILED if the block raises. Inside an active MLflow or
Weights & Biases run, that run's id and URL are recorded with it, so the
same run is never recorded twice and the console links to it.

record_run and finish_run are the same calls without the context manager.
"""

from contextlib import contextmanager


def version_id(version):
    """A dataset version id from an id, a version dict or a SnapshotStream."""
    if isinstance(version, str):
        return version
    if isinstance(version, dict):
        return version["id"]
    return version.version_id


def active_tracker():
    """(source, external_id, url) of an active MLflow or W&B run, else ("SDK", "", "")."""
    try:
        import mlflow
        run = mlflow.active_run()
        if run is not None:
            url = ""
            tracking = mlflow.get_tracking_uri() or ""
            if tracking.startswith(("http://", "https://")):
                url = f"{tracking.rstrip('/')}/#/experiments/{run.info.experiment_id}/runs/{run.info.run_id}"
            return "MLFLOW", run.info.run_id, url
    except ImportError:
        pass
    try:
        import wandb
        if wandb.run is not None:
            return "WANDB", wandb.run.path, wandb.run.url or ""
    except ImportError:
        pass
    return "SDK", "", ""


def record_run(client, name, versions, params=None, metrics=None, source=None, external_id=None, url=None,
               status="RUNNING", model=None, roles=None):
    """Records a run that trained on versions. model is {"name", "version", "uri"}.
    roles maps a version to TRAIN, VALIDATION or TEST; a run with both a
    training and an evaluation version is checked for leakage."""
    detected = active_tracker() if source is None else (source, "", "")
    body = {
        "name": name,
        "source": detected[0],
        "external_id": external_id if external_id is not None else detected[1],
        "url": url if url is not None else detected[2],
        "status": status,
        "params": {str(k): str(v) for k, v in (params or {}).items()},
        "metrics": {str(k): float(v) for k, v in (metrics or {}).items()},
        "dataset_version_ids": [version_id(v) for v in versions],
    }
    if model:
        body["model"] = model
    if roles:
        body["roles"] = {version_id(v): r.upper() for v, r in roles.items()}
    return client.post("/api/v1/training-runs", body)


def finish_run(client, run_id, status="FINISHED", metrics=None, model=None):
    """Finishes a run, adding metrics and the model it produced."""
    body = {"status": status, "metrics": {str(k): float(v) for k, v in (metrics or {}).items()}}
    if model:
        body["model"] = model
    return client.request("PATCH", f"/api/v1/training-runs/{run_id}", body=body)


def runs(client, version=None):
    """Recorded runs, newest first; with version, those that trained on it."""
    params = {"dataset_version_id": version_id(version)} if version is not None else {}
    return list(client.pages("/api/v1/training-runs", "runs", **params))


class TrackedRun:
    def __init__(self, client, run):
        self.client = client
        self.run = run
        self.id = run["id"]
        self.metrics = {}
        self.models = []

    def log_metrics(self, **metrics):
        self.metrics.update(metrics)

    def log_model(self, name, version, uri=""):
        self.models.append({"name": name, "version": str(version), "uri": uri})

    def finish(self, status):
        # One model per call; the metrics go with the first.
        first = self.models[0] if self.models else None
        self.run = finish_run(self.client, self.id, status, self.metrics, first)
        for model in self.models[1:]:
            self.run = finish_run(self.client, self.id, status, None, model)
        return self.run


@contextmanager
def track_run(client, name, versions, params=None, **kwargs):
    run = TrackedRun(client, record_run(client, name, versions, params=params, **kwargs))
    try:
        yield run
    except BaseException:
        run.finish("FAILED")
        raise
    run.finish("FINISHED")
