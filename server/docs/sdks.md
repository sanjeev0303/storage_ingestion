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

The file's SHA-256 is computed piece by piece, so a large file is not held in memory. Run `upload` again with the same journal after a closed tab or a lost connection: parts already sent are not sent again. In Node, `@osp/sdk/node` has `fileSource(path)` and `fileJournal(path)`.

## Go

```go
client := osp.New(baseURL, token)
result, err := client.UploadFile(ctx, "run.tar", osp.UploadOptions{
	Dataset: "scans", Path: "2026/run.tar", Limiter: osp.NewLimiter(10 << 20),
})
```

Give `Store` to keep each part as it is sent, and `Resume` to continue. `Limiter` caps every upload sharing it.
