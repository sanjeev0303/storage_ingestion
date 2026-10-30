"""Access governance: API tokens, share links, egress, classification and
security alerts.

An API token is a credential for scripts and vendors. Its permissions are
capped by its member's role; datasets and networks confine it further.
Use one in place of a session:

    client = Client(url, access_token=os.environ["OSP_API_TOKEN"])

    created = create_token(client, "vendor-acme", ["assets.read", "assets.download"],
                           datasets=["scans"], networks=["203.0.113.0/24"], expires_in="720h")
    print(created["token"])  # shown once

    revoke_links(client, asset_ids=[asset_id])
    for a in alerts(client, state="OPEN"):
        print(a["kind"], a["detail"])
"""


def create_token(client, name, permissions, datasets=None, networks=None, expires_in="", allow_links=False):
    """A new API token. The response's "token" is the secret, shown once."""
    body = {"name": name, "permissions": list(permissions), "datasets": list(datasets or []),
            "networks": list(networks or []), "allow_links": allow_links}
    if expires_in:
        body["expires_in"] = expires_in
    return client.post("/api/v1/api-tokens", body)


def tokens(client, everyone=False):
    """Your tokens, or every member's with members.manage."""
    params = {"all": "true"} if everyone else {}
    return client.get("/api/v1/api-tokens", **params)["tokens"]


def revoke_token(client, token_id):
    client.request("DELETE", f"/api/v1/api-tokens/{token_id}")


def access_policy(client):
    return client.get("/api/v1/access-policy")


def set_access_policy(client, **policy):
    """Replaces the policy: links_enabled, max_link_ttl ("1h"),
    egress_monthly_bytes, egress_daily_member_bytes, egress_action (WARN or
    BLOCK), mass_link_threshold, anomaly_action (ALERT or REVOKE_LINKS)."""
    return client.request("PUT", "/api/v1/access-policy", policy)


def links(client, everyone=False, active=True):
    """Share links: yours, or everyone's with assets.delete."""
    params = {"active": "true" if active else "false"}
    if everyone:
        params["all"] = "true"
    return client.get("/api/v1/links", **params)["links"]


def revoke_links(client, link_ids=None, asset_ids=None, principal_id="", everything=False):
    """Revokes live share links; returns how many."""
    body = {"link_ids": list(link_ids or []), "asset_ids": list(asset_ids or []),
            "principal_id": principal_id, "all": everything}
    return client.post("/api/v1/links/revoke", body)["revoked"]


def egress(client):
    """This month's downloaded bytes, the budgets, and each member's use."""
    return client.get("/api/v1/egress")


def classify(client, asset_id, classification):
    """PUBLIC, INTERNAL, CONFIDENTIAL or RESTRICTED."""
    return client.request("PUT", f"/api/v1/assets/{asset_id}/classification", {"classification": classification})


def alerts(client, state=""):
    """Security alerts, newest first; state OPEN or ACKNOWLEDGED."""
    params = {"state": state} if state else {}
    return client.get("/api/v1/security/alerts", **params)["alerts"]


def acknowledge(client, alert_id):
    return client.post(f"/api/v1/security/alerts/{alert_id}/acknowledge")
