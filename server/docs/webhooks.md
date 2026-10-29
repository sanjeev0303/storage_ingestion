# Webhooks

An organization registers HTTPS endpoints and the events it wants. The webhook worker (`cmd/webhook-worker`) signs each event and POSTs it to every subscribed endpoint, retrying failures.

## Events

| Type | When | `data` |
|---|---|---|
| `object.available` | an upload passed verification and scanning and was published | `asset_id`, `object_id`, `version`, `namespace`, `file_name`, `size_bytes`, `checksum_sha256` |
| `object.rejected` | processing rejected an upload | `asset_id`, `object_id`, `namespace`, `file_name`, `reason` |
| `object.erased` | a deletion finished: the bytes are gone from storage | `asset_id`, `deletion_id`, `reason` |
| `ingestion.finished` | a bulk ingestion job ended | `job_id`, `dataset_id`, `state`, `total_entries`, `published_entries`, `failed_entries`, `stored_bytes` |
| `dataset.snapshot_created` | a snapshot froze a dataset version | `dataset_id`, `version_id`, `version`, `member_count`, `total_bytes`, `manifest_sha256` |
| `dataset.validation_finished` | a validation job ended | `dataset_id`, `version_id`, `job_id`, `profile_id`, `state`, `report_id`, `passed_items`, `warning_items`, `failed_items` |
| `webhook.test` | `POST /webhooks/{id}/test` | `endpoint_id`, `requested_by` |

Events are written by database triggers (migration `000021_webhooks.sql`) in the transaction that makes the change. An event exists exactly when its change committed, whichever code path made it. Payloads carry identifiers and states only: never file content, storage keys or URLs.

## Delivery

```
POST <your url>
Content-Type: application/json
webhook-id: <event id>
webhook-timestamp: <unix seconds>
webhook-signature: v1,<base64 HMAC-SHA256>

{"id":"…","type":"object.available","created_at":"…","data":{…}}
```

The signature follows [Standard Webhooks](https://www.standardwebhooks.com/). It is HMAC-SHA256 over `id.timestamp.body`, keyed with the bytes of the secret after `whsec_` (base64). Standard Webhooks libraries verify it as is. Reject a timestamp more than a few minutes old to stop replays, and deduplicate on `webhook-id`: delivery is at least once.

- **Success:** any 2xx within 10 seconds.
- **Retries:** after 10s, 1m, 5m, 30m, 2h, 6h, 12h and 24h; then the delivery is `FAILED`. `POST /webhooks/{id}/deliveries/{delivery}/retry` sends it again.
- **Redirects:** not followed. A 3xx counts as a failure.
- **Order:** not guaranteed between events.

**Network guard:** every connection goes through `netguard`, the same as customer storage endpoints. That means:

- HTTPS only;
- no loopback, private, link-local or cloud-metadata addresses, checked again on each dial against the resolved address;
- no proxy from the environment.

`STORAGE_ALLOW_PRIVATE_ENDPOINTS=true` (development only) allows local receivers.

## API (`org.manage`)

| Route | |
|---|---|
| `GET /api/v1/webhooks` | endpoints, and the event types available |
| `POST /api/v1/webhooks` | `{url, description, event_types[]}` → the endpoint with its `secret`, shown this once |
| `GET`, `PATCH`, `DELETE /api/v1/webhooks/{id}` | read; change `url`, `description`, `event_types`, `enabled`; remove (with its deliveries) |
| `POST /api/v1/webhooks/{id}/rotate-secret` | a new secret, effective at once |
| `POST /api/v1/webhooks/{id}/test` | queues a `webhook.test` event |
| `GET /api/v1/webhooks/{id}/deliveries?status=&before=` | newest first, 50 a page (`next_before`) |
| `POST /api/v1/webhooks/{id}/deliveries/{delivery}/retry` | send again from a fresh attempt count |

At most 20 endpoints per organization. Secrets are sealed with the credential keyring (`STORAGE_CREDENTIAL_KEYS`). Without it, webhooks answer 501 and the worker does not start. The audit log records changes with the URL's host only, since a path or query can carry a receiver's token.

## Worker

`webhook-worker` claims due deliveries under a one-minute lease (`FOR UPDATE SKIP LOCKED`), so several can run. A `NOTIFY` from the trigger wakes it as soon as a delivery commits; it also polls every `WEBHOOK_POLL_INTERVAL` (5s). `WEBHOOK_CONCURRENCY` (16) bounds sends in flight.
