"""Edge devices (docs/edge.md): register a device, watch its heartbeat and its
runs. The device itself runs osp-agent (sdk/go).

    from osp import edge
    device, token = edge.register(client, "novaseq-1", dataset_id)
    for d in edge.devices(client):
        print(d["name"], d["pending_files"], d.get("last_seen"))
"""

import json


def register(client, name, dataset_id, path_prefix=""):
    """Makes a device and its credential: upload-only, under its prefix in
    the dataset's namespace. The token is shown once."""
    out = client.post("/api/v1/edge-devices", {"name": name, "dataset_id": dataset_id, "path_prefix": path_prefix})
    return out["device"], out["token"]


def devices(client):
    return client.get("/api/v1/edge-devices")["devices"]


def revoke(client, device_id):
    return client.request("DELETE", f"/api/v1/edge-devices/{device_id}")


def recordings(client, device_id=None):
    return client.get("/api/v1/edge-recordings", device_id=device_id)["recordings"]


def recording(client, recording_id):
    """A run or recording with its chunks and the gaps between them."""
    return client.get(f"/api/v1/edge-recordings/{recording_id}")


def agent_config(api, token_file, state_dir, path, mode="run", marker="CopyComplete.txt", delete_after_confirm=False, **more):
    """A starting configuration for osp-agent, as text."""
    watch = {"path": path, "mode": mode, "delete_after_confirm": delete_after_confirm}
    if mode == "run":
        watch["marker"] = marker
    watch.update(more)
    return json.dumps({"api": api, "token_file": token_file, "state_dir": state_dir, "watches": [watch]}, indent=2)
