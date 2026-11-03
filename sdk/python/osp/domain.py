"""Domain metadata and publishing (docs/domain.md): STAC export, what recordings
hold, segments inside files, embeddings you computed, upstream copies, and a
snapshot published with a landing page and a DOI.

    from osp import domain
    index = domain.create_index(client, dataset_id, version_id, "clip", 512, model="clip-vit-b32")
    domain.put_vectors(client, index["id"], [{"path": "a.jpg", "vector": vec}, ...])
    for m in domain.query(client, index["id"], vec, top=5):
        print(m["path"], m["score"])
"""

import re


def _version(dataset_id, version_id):
    return f"/api/v1/datasets/{dataset_id}/versions/{version_id}"


def stac_collection(client, dataset_id, version_id):
    return client.get(_version(dataset_id, version_id) + "/stac/collection.json")


def stac_items(client, dataset_id, version_id):
    return client.get(_version(dataset_id, version_id) + "/stac/items.json")["features"]


def recordings(client, dataset_id, version_id, topic=None, from_ns=None, to_ns=None):
    """MCAP files and rosbag2 folders in a snapshot, indexed when it is profiled."""
    return client.get(_version(dataset_id, version_id) + "/recordings", topic=topic, from_ns=from_ns, to_ns=to_ns)["recordings"]


def add_segments(client, dataset_id, version_id, segments):
    """Time ranges inside files, as dicts of path, start_ns, end_ns and
    optionally label, note and attributes. All are recorded, or none."""
    return client.post(_version(dataset_id, version_id) + "/segments", {"segments": list(segments)})["segments"]


def segments(client, dataset_id, version_id, path=None, label=None, from_ns=None, to_ns=None):
    """Every matching segment, following the pages."""
    after = None
    while True:
        params = {"path": path, "label": label, "from_ns": from_ns, "to_ns": to_ns, "after": after, "limit": 1000}
        page = client.request("GET", _version(dataset_id, version_id) + "/segments", params=params)["segments"]
        yield from page
        if len(page) < 1000:
            return
        after = page[-1]["id"]


def delete_segment(client, dataset_id, version_id, segment_id):
    return client.request("DELETE", _version(dataset_id, version_id) + f"/segments/{segment_id}")


def create_index(client, dataset_id, version_id, name, dimensions, model="", metric="COSINE"):
    return client.post(_version(dataset_id, version_id) + "/embedding-indexes", {"name": name, "dimensions": dimensions, "model": model, "metric": metric})


def indexes(client, dataset_id, version_id):
    return client.get(_version(dataset_id, version_id) + "/embedding-indexes")["indexes"]


def delete_index(client, index_id):
    return client.request("DELETE", f"/api/v1/embedding-indexes/{index_id}")


def put_vectors(client, index_id, vectors, batch=1000):
    """Sends vectors in batches: dicts of path, vector and optionally
    segment_id. A batch is all or none. Answers how many were written."""
    written = 0
    pending = []
    for v in vectors:
        vector = v["vector"]
        pending.append({**v, "vector": vector.tolist() if hasattr(vector, "tolist") else list(vector)})
        if len(pending) == batch:
            written += client.post(f"/api/v1/embedding-indexes/{index_id}/vectors", {"vectors": pending})["written"]
            pending = []
    if pending:
        written += client.post(f"/api/v1/embedding-indexes/{index_id}/vectors", {"vectors": pending})["written"]
    return written


def query(client, index_id, vector, top=10, path=""):
    """The nearest vectors, the highest score first."""
    vector = vector.tolist() if hasattr(vector, "tolist") else list(vector)
    return client.post(f"/api/v1/embedding-indexes/{index_id}/query", {"vector": vector, "top": top, "path": path})["matches"]


def upstream(client, dataset_id=None, state=None):
    """Where imports came from, and whether each copy changed or went away."""
    return client.get("/api/v1/upstream-sources", dataset_id=dataset_id, state=state)["sources"]


def parse_creator(text):
    """Reads "Name", "Name (Affiliation)" or "Name (Affiliation) 0000-0002-1825-0097"."""
    orcid = re.search(r"\d{4}-\d{4}-\d{4}-\d{3}[\dX]\s*$", text)
    out = {}
    if orcid:
        out["orcid"] = orcid.group().strip()
        text = text[:orcid.start()]
    text = text.strip()
    match = re.fullmatch(r"(.+?)\s*\((.+)\)", text)
    if match:
        text, out["affiliation"] = match.group(1), match.group(2)
    out["name"] = text
    return out


def publish(client, dataset_id, version_id, title, creators, publisher, description="", license="", mint_doi=False):
    """Publishes a snapshot: a public landing page with its metadata, and a
    DOI when asked. The data stays private. Answers the publication and its
    landing_url."""
    return client.post(_version(dataset_id, version_id) + "/publish", {"title": title, "description": description, "publisher": publisher,
                                                                       "creators": creators, "license": license, "mint_doi": mint_doi})


def publication(client, dataset_id, version_id):
    return client.get(_version(dataset_id, version_id) + "/publication")


def mint_doi(client, dataset_id, version_id):
    """Registers the DOI, or tries again after a failure."""
    return client.post(_version(dataset_id, version_id) + "/publication/doi", {})


def withdraw(client, dataset_id, version_id, reason):
    return client.post(_version(dataset_id, version_id) + "/publication/withdraw", {"reason": reason})


def public_record(client, slug):
    """What anyone may read: metadata, citation, BibTeX, RIS, DataCite, schema.org."""
    return client.get(f"/api/v1/public/publications/{slug}")


def retention_cost(client, dataset_id, version_id, years=10, price_per_gb_month=None):
    return client.get(_version(dataset_id, version_id) + "/retention-cost", years=years, price_per_gb_month=price_per_gb_month)
