# Access governance

API tokens for scripts and vendors, share-link control, download (egress) budgets, asset classification, and alerts on unusual reads. Migration `000032_access_governance.sql`.

## API tokens

A member makes a token from a signed-in session (never from another token):

```
POST /api/v1/api-tokens
{"name": "vendor-acme", "permissions": ["assets.read", "assets.download"],
 "datasets": ["scans"], "networks": ["203.0.113.0/24"], "expires_in": "720h", "allow_links": false}
```

- The response carries `token` (`osp_pat_…`) once. Only its SHA-256 is stored. Send it as `Authorization: Bearer osp_pat_…`.
- `permissions` must be ones the member holds (403 `escalation` otherwise). Each request also intersects them with the member's current role, so a demotion or removal narrows every token at once.
- `datasets` confines the token to those dataset namespaces: any other asset answers 404, as if it did not exist. This is a vendor workspace. A dataset-confined token can never create share links.
- `networks` (CIDRs) binds it to source addresses; from anywhere else it answers 401. Behind the console proxy set `AUTH_TRUST_PROXY_HEADERS=true` so the real client address is used.
- `expires_in` is 1h to 8760h, 90 days by default. Last use (time and address) is recorded at most once a minute.
- A token is pinned to its organization and never carries platform roles. It cannot mint tokens, change the access policy or reach SSO and SCIM settings.
- `GET /api/v1/api-tokens` lists your tokens (`?all=true` with `members.manage`). `DELETE /api/v1/api-tokens/{id}` revokes at once.
- Every read by a token is in the delivery audit with its `credential_id` and source address.

## Access policy

`GET`/`PUT /api/v1/access-policy` (`org.manage` to change):

| Field | Meaning |
|---|---|
| `links_enabled` | Members may create share links (token mode). |
| `max_link_ttl` | Longest link, such as `1h`. `0s` keeps the platform limit. |
| `egress_monthly_bytes` | Organization budget for the calendar month (UTC). 0 is none. |
| `egress_daily_member_bytes` | Per member per day. 0 is none. |
| `egress_action` | `WARN`: serve and raise an `EGRESS_BUDGET` alert (once a day). `BLOCK`: refuse with 429 `egress_budget_exceeded` and `Retry-After`. |
| `mass_link_threshold` | Share links by one member in five minutes before a `MASS_LINKS` alert (10 to 100,000, default 300). |
| `anomaly_action` | `ALERT`, or `REVOKE_LINKS`: also revoke that member's live links. |

Egress counts presigned links at the asset's size when issued, and proxied and share-link downloads at the bytes of the requested range.

## Classification

`PUT /api/v1/assets/{id}/classification` with `PUBLIC`, `INTERNAL`, `CONFIDENTIAL` or `RESTRICTED` (`catalog.write`). The asset response shows `classification`.

- `CONFIDENTIAL`: every link lasts at most 1 hour.
- `RESTRICTED`: no share links (403 `link_policy`), presigned links at most 15 minutes.
- Content checks raise it on their own and never lower it: a `secret.*` finding makes the version RESTRICTED, an external engine's `external.pii.*` or `external.phi.*` finding makes it CONFIDENTIAL (docs/content-checks.md).

## Share links

- `GET /api/v1/links` lists your live links with use counts (`?all=true` with `assets.delete`, `?active=false` to include expired and revoked).
- `POST /api/v1/links/revoke` with `link_ids`, `asset_ids`, `principal_id` or `"all": true` revokes in bulk. Members revoke their own; `assets.delete` revokes anyone's.

## Alerts

The cleanup worker runs the detector every five minutes. Each member is compared with their own history:

- `READ_SPIKE`: reads in the last hour above the member's 14-day hourly mean plus four standard deviations, with at least 50 reads and seven days of history.
- `MASS_LINKS`: share links in five minutes over the policy threshold.
- `NEW_NETWORK`: reads from a /24 (IPv6 /48) the member has not used in 30 days.
- `EGRESS_BUDGET`: a budget was exceeded.

An alert is raised once per member, kind and hour. `GET /api/v1/security/alerts` (`audit.read`, `?state=OPEN`) and `POST /api/v1/security/alerts/{id}/acknowledge`. Each alert is also the webhook `security.alert`.

## SDK and CLI

```python
from osp import Client, access
client = Client(url, access_token=os.environ["OSP_API_TOKEN"])
access.revoke_links(client, asset_ids=[asset_id])
```

```
OSP_URL=https://… OSP_API_TOKEN=osp_pat_… osp datasets
osp token-create vendor-acme -p assets.read -p assets.download --dataset scans --network 203.0.113.0/24
osp tokens
osp token-revoke ID
osp links
osp revoke-links --member USER
osp alerts [--ack ID]
osp egress
osp classify ASSET restricted
osp access-policy --set egress_action=BLOCK --set egress_monthly_bytes=1099511627776
```

The console has all of this under Operations, Access, and classification on each asset.
