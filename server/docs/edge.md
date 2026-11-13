# Edge and instrument ingest

Sequencers stick at "Uploading", robots record faster than their link, and factory links drop for hours. `osp-agent` (sdk/go) runs beside the instrument: it watches a folder, waits until a run is finished, keeps a durable journal, sends what matters first inside upload windows and a bandwidth cap, and deletes a local copy only after the platform has confirmed its SHA-256. Migration `000042_edge.sql`.

## Devices

`POST /api/v1/edge-devices {"name", "dataset_id", "path_prefix"}` (`org.manage`) registers a device and answers its token, once. The token is upload-only (`uploads.write`), confined to the dataset's namespace and to paths under the prefix (`devices/<name>/` by default): any other path answers **403**, and it can read and delete nothing. A stolen device can add files under its own prefix and do nothing else. `DELETE .../{id}` revokes it at once. `GET /api/v1/edge-devices` shows each device's last heartbeat and what waits on it.

These confinements apply to every API token: an upload outside a token's datasets or path prefix is refused.

## The agent

```json
{
  "api": "https://storage.example.org",
  "token_file": "/etc/osp/token",
  "state_dir": "/var/lib/osp-agent",
  "bandwidth_bytes_per_second": 12500000,
  "windows": [{ "from": "19:00", "to": "07:00" }],
  "window_bypass_priority": 9,
  "rules": [
    { "flag_suffix": ".keep", "priority": 9 },
    { "match": "*.mcap", "priority": 5 },
    { "older_than_hours": 72, "priority": 7 }
  ],
  "watches": [
    { "path": "/data/runs", "mode": "run", "marker": "CopyComplete.txt", "incomplete_after_hours": 48 },
    { "path": "/data/logs", "mode": "recording", "recording_pattern": "^(.+)_\\d+\\.mcap$", "chunk_seconds": 60, "delete_after_confirm": true },
    { "path": "/data/exports", "mode": "files", "settle_seconds": 30 }
  ]
}
```

`osp-agent -config agent.json` runs until stopped; `-once` does one pass; `-status` prints what waits. Empty files, as markers usually are, are not sent: the platform stores nothing empty. A file the platform refuses for good (400, 413, 415, 422, or 409 `path_exists`: too large, say) is counted as failed and does not hold the others up. Other refusals (a busy path, a reconciliation in progress, 408) are tried again later, 30 seconds after the first and doubling to at most an hour. A file the agent cannot read (locked by the instrument, say) is skipped for that pass; a lost connection, 401, 403, 429 or 5xx ends the pass's sending and is tried at the next.

| Mode | What counts as ready |
|---|---|
| `run` | Each subfolder is a run. Nothing goes until its `marker` appears (`CopyComplete.txt`, `RTAComplete.txt`). After `incomplete_after_hours` with no marker, what is there is sent and the run is reported incomplete. |
| `recording` | Rotating chunks whose name the pattern's first group groups into one logical recording. A chunk covers `chunk_seconds` up to its modified time. A recording that goes quiet for `close_after_idle_seconds` is closed. Chunks that come after a recording closed `COMPLETE` start the next one, named `<name>~2`, `<name>~3` and so on, because a complete recording takes no more chunks (409 `recording_complete`). |
| `files` | Each file, once it has not changed for `settle_seconds`. |

**The journal** (`state_dir/journal.log`) is synced to disk after every record: each file's hash and state, each upload's ID and each part's ETag. After a power cut the agent continues with the parts not yet sent. A record cut short is ignored, because it was never acknowledged.

**Order.** The highest matching rule sets a file's priority (by name, by a flag file beside it, by how long it has waited); among equals the oldest goes first. Outside the windows only files at or above `window_bypass_priority` go. The bandwidth cap is shared by all uploads.

**Deleting.** With `delete_after_confirm`, a local copy goes only when `GET /api/v1/edge/confirm` answers `AVAILABLE`: the platform computed this SHA-256 from the stored bytes and its checks passed. A rejected file (malware, wrong hash) stays on the device and is counted as failed.

## MQTT

Devices that publish to a broker rather than write files: add `mqtt` to the agent's configuration (with it, `watches` may be empty).

```json
"mqtt": {
  "broker": "tls://broker.factory.local:8883",
  "client_id": "line-3-agent",
  "username": "agent", "password_file": "/etc/osp/mqtt-password",
  "ca_file": "/etc/osp/broker-ca.pem",
  "topics": ["factory/+/telemetry", "factory/+/alarms"],
  "qos": 1,
  "dir": "/var/lib/osp-agent/mqtt",
  "name": "line3",
  "roll_seconds": 60,
  "roll_bytes": 67108864,
  "delete_after_confirm": true
}
```

The agent subscribes with a persistent session (clean session off, under `client_id`), so the broker holds QoS 1 and 2 messages while the device or its link is down. Each message is one JSON line: `topic`, `received_at`, `qos`, `retain`, and `payload` when the payload is JSON, otherwise `payload_base64`. Lines go to `dir/<name>_<unix ns>.jsonl.part`; after `roll_seconds` or `roll_bytes` the file is synced and renamed to `.jsonl`, and a recording watch the agent adds for `dir` sends it as a chunk of the recording `<name>`, covering the roll period, like any other recording chunk.

**Delivery.** A QoS 1 message is acknowledged (PUBACK), and a QoS 2 message received (PUBREC), only once its line is synced to disk, within 200 ms: a power cut loses nothing the broker was told is kept. A QoS 2 message resent before its release is kept once. QoS 1 is at least once: a message whose acknowledgement was lost in a reconnect arrives again, as a second line. QoS 0 is as the broker sends it. A `.part` file left by a stop is cut back to its last whole line and finished at the next start.

`tcp://`, `mqtt://` (1883) and `tls://`, `ssl://`, `mqtts://` (8883, TLS 1.2 or later, the system's CAs or `ca_file`) are accepted. The client speaks MQTT 3.1.1 with the standard library; messages over 64 MiB end the session. The agent reconnects with backoff from 1 second to a minute.

## Runs and recordings

A device's own credential calls these; the agent does so for you:

- `POST /api/v1/edge/recordings {"name", "kind", "gap_seconds"}` opens a `RUN` or `RECORDING`, or answers the one with that name.
- `POST /api/v1/edge/recordings/{id}/chunks {"path", "sha256", "size_bytes", "starts_at", "ends_at"}` appends a file or a time window. It is idempotent: the same path and hash again changes nothing, other bytes at that path answer **409** `chunk_differs`. Chunks may arrive late and in any order. A chunk that closes a gap in the recording's time sends `edge.backfill_closed`.
- `POST /api/v1/edge/recordings/{id}/close {"expected", "missing"}` ends it. Only when nothing is missing, the count is what was expected, and every file is published with exactly its SHA-256 does it become `COMPLETE`: its files join the dataset's draft, a snapshot is taken, and `edge.run_complete` is sent. Otherwise it is `INCOMPLETE`, saying what is missing, with no snapshot, and `edge.run_incomplete` is sent. Closing again later can complete it. A chunk that arrives while a close is under way is never left out of a `COMPLETE` recording's snapshot: the close counts the chunks again under the recording's lock and, if one arrived, closes again with it.

`GET /api/v1/edge-recordings[?device_id=]` and `GET /api/v1/edge-recordings/{id}` (`org.read`) show them, with chunks and gaps.

`POST /api/v1/edge/heartbeat {"pending_files", "pending_bytes", "failed_files", "oldest_pending", "agent_version", "note"}` is sent every minute. The console marks a device silent after three minutes.

## Raw-data vaults

For GxP instrument data, give the device's dataset a `COMPLIANCE` retention policy (docs/governance.md): files are locked as they arrive, no role, tenant admins included, can delete them, and the agent's capture leaves nothing for an analyst to edit before it is sealed. Malware scanning before a file is readable makes this a media-exchange point for USB-borne files too.

## Moving a lot over a slow link

Where the link cannot carry the data at all, send it with Aspera, Signiant or MASV into your own bucket, then bring it in place with `POST /api/v1/adoptions` (docs/adoption.md): nothing is uploaded twice, and every hash is still checked.
