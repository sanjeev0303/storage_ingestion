# SDKs

| SDK | Where | For |
|---|---|---|
| Python | `sdk/python` | Everything: ingest, datasets, streaming, the `osp` command |
| TypeScript | `sdk/js` | Uploads from browsers and Node, with no dependencies |
| Go | `sdk/go` | Uploads from services and small devices, standard library only; also `osp-agent` (docs/edge.md) |

## Why uploads do not hit proxy limits

Cloudflare's Free and Pro plans stop request bodies at 100 MB, and other proxies have their own limits. Upload bytes never pass through the API, so those limits do not apply: the SDK asks the API for presigned URLs in small JSON requests, and sends the bytes straight to storage.

| Size | How it goes |
|---|---|
| Up to 64 MiB | One PUT |
| Larger | Parts of 8 MiB, up to 10,000; the part size grows only for files over about 78 GiB |

If you do proxy storage itself, pass a part size under the proxy's limit.

## Bad networks

Every SDK uploader, and the console, treats the network the same way:

| Situation | What happens |
|---|---|
| A request to storage sends nothing for the stall timeout | It is dropped and the part is sent again. A slow link that keeps moving is never cut off; there is no timeout on the request as a whole. |
| Network error, 408, 429 or 5xx | Tried again with exponential backoff and full jitter (a random wait from 0 up to 1 s, 2 s, 4 s ... capped at 30 s). `Retry-After` wins when the answer has one (the SDKs cap it at a minute). |
| A part URL expired (403 naming an expired request or token, a signature outside its time frame, or a skewed clock) | A fresh URL for that part alone is fetched from the API and the part goes again at once. A plain `AccessDenied` is a refusal, not an expiry. |
| Any other refusal (400, 401, 403, 404, 413, 422) | Not retried: it would fail the same way. |
| The run stops (crash, closed tab, lost connection) | Each part is confirmed with the API as soon as it is in, and the saved state is updated. A re-run with the same state sends only the parts not confirmed. Saved state is used only for the same file (SHA-256 and size). |
| The saved session has ended (the API answers 404, or 409 "multipart upload is not active"; for a single upload, 409 `session_ended`) | The saved state is dropped and the upload starts again once from the beginning, under a new idempotency key (the old key would hand back the ended session). |
| A single upload's start hands back a session already `COMPLETED` or `VERIFIED` (no upload URL) | Nothing is sent again: the file is already stored, and the call goes on to the wait. |
| One part fails for good | The other parts stop too, and the call returns once they have, so nothing writes the saved state afterwards. |

The checksum rules do not change: parts are confirmed against the provider's own list, completion sends the whole file's SHA-256, and an upload is done only once the API says so.

| | Stall timeout | Attempts | Saved state |
|---|---|---|---|
| Console | 20 s, from upload progress | 6 per part and per API call; none spent while offline | IndexedDB (see multipart-reliability.md) |
| TypeScript | `stallMs`, default 30 s. fetch cannot report upload progress, so a PUT counts as stalled when it runs that much longer than 16 KiB/s would need for its bytes | `attempts` per PUT and `Client({ attempts })` per API call, default 5 | `journal` (`localStorageJournal`, `fileJournal` in Node) |
| Python | `put_bytes(timeout=30)`: the socket timeout, applied to every 8 KiB the PUT sends | `attempts=5` per PUT; `Client(max_attempts=6)` per API call | `ingest_folder(..., resume_file=path)`; `osp ingest` keeps it in `~/.osp/uploads.json` |
| Go | `UploadOptions.StallTimeout`, default 30 s, reset on every read of the part (time held back by a `Limiter` does not count) | `UploadOptions.Attempts`, default 5, for storage and the upload routes | `Store` and `Resume`; `OpenProgressFile(path)` is a ready-made one |

## TypeScript

```ts
import { Client, upload, localStorageJournal } from "@osp/sdk";

const client = new Client({ baseUrl, token });
const result = await upload(client, file, {
  dataset: "scans",
  path: "2026/a.dcm",
  journal: localStorageJournal(window.localStorage),
  onProgress: ({ phase, bytes, total }) => render(phase, bytes / total),
  wait: true,
});
```

The file's SHA-256 is computed piece by piece, so a large file is not held in memory. Run `upload` again with the same journal after a closed tab or a lost connection: parts already sent are not sent again. In Node, `@osp/sdk/node` has `fileSource(path)` and `fileJournal(path)`. `stallMs` and `attempts` tune the retries above; `new Client({ baseUrl, token, attempts })` sets them for API calls. With `wait: true` it checks every `pollMs` for at most `waitTimeoutMs` (default 10 minutes), then answers the status so far; `signal` stops the wait at once.

## Go

```go
client := osp.New(baseURL, token)
result, err := client.UploadFile(ctx, "run.tar", osp.UploadOptions{
	Dataset: "scans", Path: "2026/run.tar", Limiter: osp.NewLimiter(10 << 20),
})
```

Give `Store` to keep each part as it is sent, and `Resume` to continue. `Resume` is used only when its `SHA256` and `Size` match the file; otherwise a new upload starts. `Limiter` caps every upload sharing it.

```go
progress, err := osp.OpenProgressFile("run.tar.progress.json")
result, err := client.UploadFile(ctx, "run.tar", osp.UploadOptions{
	Dataset: "scans", Path: "2026/run.tar", Store: progress, Resume: progress.Resume(),
})
if err == nil {
	progress.Remove() // done: the next run starts clean
}
```

Run the same code again after a crash or a lost connection and it continues from the confirmed parts. If the saved session has ended, `UploadFile` starts a new upload once by itself and the progress file follows it.
