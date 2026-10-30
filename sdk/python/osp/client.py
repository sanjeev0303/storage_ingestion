"""HTTP client for the platform API: sign-in, token refresh, retries."""

import http.client
import json
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from contextlib import contextmanager


class APIError(Exception):
    """A non-2xx answer from the API."""

    def __init__(self, status, method, path, body):
        self.status = status
        self.method = method
        self.path = path
        self.body = body
        try:
            detail = json.loads(body)
        except ValueError:
            detail = {}
        if not isinstance(detail, dict):
            detail = {}
        self.code = detail.get("code", "")
        message = detail.get("error") or body[:200]
        super().__init__(f"{method} {path}: HTTP {status}: {message}")


# Answers worth sending again: rate limited, or the server or a proxy
# could not answer this time.
RETRY_STATUSES = {429, 502, 503, 504}


class Client:
    """One signed-in session against the API.

    Sign in with email and password, or pass tokens saved from an earlier
    session. The access token is refreshed when it expires; refresh tokens
    are single use, so refreshes are serialised.
    """

    def __init__(self, base_url, access_token="", refresh_token="", organization_id="",
                 timeout=60, max_attempts=6, on_tokens=None):
        self.base_url = base_url.rstrip("/")
        self.access_token = access_token
        self.refresh_token = refresh_token
        self.organization_id = organization_id
        self.timeout = timeout
        self.max_attempts = max_attempts
        # Called with the new tokens after a sign-in or refresh, so a CLI
        # can save them.
        self.on_tokens = on_tokens
        self._refresh_lock = threading.Lock()
        # Kept-alive connections, one per host for each thread: a file
        # costs two requests (its link, then its bytes), and a new TLS
        # handshake for each would dominate small files.
        self._local = threading.local()

    # DataLoader workers get a copy of the client; a lock cannot be copied
    # across processes, so each copy makes its own.
    def __getstate__(self):
        state = self.__dict__.copy()
        state.pop("_refresh_lock", None)
        state.pop("_local", None)
        state["on_tokens"] = None
        return state

    def __setstate__(self, state):
        self.__dict__.update(state)
        self._refresh_lock = threading.Lock()
        self._local = threading.local()

    # ─── Authentication ───────────────────────────────────────────────

    def login(self, email, password):
        tokens = self._send("POST", "/api/v1/auth/login", {"email": email, "password": password}, auth=False)
        self._take(tokens)
        return tokens["user"]

    def refresh(self):
        with self._refresh_lock:
            if not self.refresh_token:
                raise APIError(401, "POST", "/api/v1/auth/refresh", '{"error":"not signed in"}')
            tokens = self._send("POST", "/api/v1/auth/refresh", {"refresh_token": self.refresh_token}, auth=False)
            self._take(tokens)

    def logout(self):
        if self.access_token:
            self.request("POST", "/api/v1/auth/logout")
        self.access_token = self.refresh_token = ""

    def _take(self, tokens):
        self.access_token = tokens["access_token"]
        self.refresh_token = tokens["refresh_token"]
        if self.on_tokens:
            self.on_tokens(tokens)

    # ─── Requests ─────────────────────────────────────────────────────

    def request(self, method, path, body=None, params=None, headers=None, raw=None, content_type=None):
        """Sends one API request and returns the decoded JSON (None for 204).

        A 401 refreshes the session once and sends again. 429 and 5xx
        gateway answers are retried with backoff.
        """
        if params:
            query = {k: v for k, v in params.items() if v not in (None, "")}
            if query:
                path = path + "?" + urllib.parse.urlencode(query)
        try:
            return self._send(method, path, body, headers=headers, raw=raw, content_type=content_type)
        except APIError as err:
            if err.status != 401 or not self.refresh_token:
                raise
        self.refresh()
        return self._send(method, path, body, headers=headers, raw=raw, content_type=content_type)

    def get_text(self, path, **params):
        """GETs a route that answers text (an export) and returns it."""
        return self.request("GET", path, params=params, headers={"Accept": "*/*"})

    def _send(self, method, path, body=None, auth=True, headers=None, raw=None, content_type=None):
        data = raw
        all_headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            all_headers["Content-Type"] = "application/json"
        if content_type:
            all_headers["Content-Type"] = content_type
        if auth and self.access_token:
            all_headers["Authorization"] = "Bearer " + self.access_token
        if self.organization_id:
            all_headers["X-Organization-ID"] = self.organization_id
        all_headers.update(headers or {})

        delay = 1.0
        for attempt in range(1, self.max_attempts + 1):
            try:
                resp = self._roundtrip(method, self.base_url + path, data, all_headers, self.timeout)
                payload = resp.read()
            except (http.client.HTTPException, OSError):
                if attempt == self.max_attempts:
                    raise
                time.sleep(delay)
                delay = min(delay * 2, 30)
                continue
            if resp.status in RETRY_STATUSES and attempt < self.max_attempts:
                time.sleep(_retry_after(resp.headers) or delay)
                delay = min(delay * 2, 30)
                continue
            if resp.status >= 300:
                raise APIError(resp.status, method, path, payload.decode("utf-8", "replace"))
            if resp.status == 204 or not payload:
                return None
            if all_headers.get("Accept") != "application/json":
                return payload.decode("utf-8")
            return json.loads(payload)
        raise RuntimeError("unreachable")

    # ─── Connections ──────────────────────────────────────────────────

    def _connection(self, key, timeout):
        """This thread's connection to scheme://netloc, and whether it was
        used before."""
        conns = getattr(self._local, "conns", None)
        if conns is None:
            conns = self._local.conns = {}
        conn = conns.get(key)
        if conn is not None:
            conn.timeout = timeout
            return conn, True
        scheme, netloc = key
        kind = http.client.HTTPSConnection if scheme == "https" else http.client.HTTPConnection
        conn = conns[key] = kind(netloc, timeout=timeout)
        return conn, False

    def _drop(self, key):
        conn = getattr(self._local, "conns", {}).pop(key, None)
        if conn is not None:
            conn.close()

    def _roundtrip(self, method, url, body, headers, timeout):
        """Sends one request over a kept-alive connection and returns the
        response, whose body the caller reads in full."""
        parts = urllib.parse.urlsplit(url)
        key = (parts.scheme, parts.netloc)
        target = (parts.path or "/") + ("?" + parts.query if parts.query else "")
        while True:
            conn, reused = self._connection(key, timeout)
            try:
                conn.request(method, target, body=body, headers=headers)
                return conn.getresponse()
            except (http.client.HTTPException, OSError):
                self._drop(key)
                if reused:
                    continue  # the server had closed the idle connection: once more, on a new one
                raise

    @contextmanager
    def fetch(self, url, timeout=600):
        """GETs url, a download link from the API or storage, over a
        kept-alive connection, and yields the response to read from. A
        response not read to its end closes its connection."""
        key = (urllib.parse.urlsplit(url).scheme, urllib.parse.urlsplit(url).netloc)
        resp = self._roundtrip("GET", url, None, {}, timeout)
        try:
            if resp.status >= 300:
                raise APIError(resp.status, "GET", urllib.parse.urlsplit(url).path, resp.read().decode("utf-8", "replace"))
            yield resp
            if not resp.isclosed():
                self._drop(key)
        except BaseException:
            self._drop(key)
            raise

    @contextmanager
    def stream(self, method, path, body=None, timeout=600, headers=None):
        """Sends an API request and yields the response to read as it
        arrives (a shard stream). Like request, it refreshes the session
        on a 401 and waits out 429 and gateway answers before the body
        starts."""
        url = self.base_url + path
        parts = urllib.parse.urlsplit(url)
        key = (parts.scheme, parts.netloc)
        data = json.dumps(body).encode() if body is not None else None
        extra = headers or {}
        delay, refreshed = 1.0, False
        for attempt in range(1, self.max_attempts + 1):
            headers = {"Accept": "*/*", "Content-Type": "application/json"}
            if self.access_token:
                headers["Authorization"] = "Bearer " + self.access_token
            if self.organization_id:
                headers["X-Organization-ID"] = self.organization_id
            headers.update(extra)
            resp = self._roundtrip(method, url, data, headers, timeout)
            if resp.status == 401 and self.refresh_token and not refreshed:
                resp.read()
                self.refresh()
                refreshed = True
                continue
            if resp.status in RETRY_STATUSES and attempt < self.max_attempts:
                resp.read()
                time.sleep(_retry_after(resp.headers) or delay)
                delay = min(delay * 2, 30)
                continue
            if resp.status >= 300:
                raise APIError(resp.status, method, path, resp.read().decode("utf-8", "replace"))
            try:
                yield resp
                if not resp.isclosed():
                    self._drop(key)
            except BaseException:
                self._drop(key)
                raise
            return
        raise RuntimeError("unreachable")

    def get(self, path, **params):
        return self.request("GET", path, params=params)

    def post(self, path, body=None, headers=None):
        return self.request("POST", path, body=body, headers=headers)

    def pages(self, path, key, cursor_param="cursor", cursor_key="next_cursor", **params):
        """Yields every item of a paged list, following its cursor."""
        cursor = None
        while True:
            page = self.get(path, **{**params, cursor_param: cursor})
            for item in page.get(key) or []:
                yield item
            cursor = page.get(cursor_key)
            if not cursor:
                return


def _retry_after(headers):
    value = headers.get("Retry-After") if headers else None
    try:
        return min(float(value), 60.0)
    except (TypeError, ValueError):
        return None


def put_bytes(url, body, size, content_type=None, timeout=600, attempts=3, headers=None):
    """PUTs to a presigned storage URL and returns the ETag. body is bytes
    or a callable returning a fresh file-like object for each attempt.
    headers are the upload_headers the API asked for (Azure's blob type,
    or a connection's customer-managed key)."""
    headers = {**(headers or {}), "Content-Length": str(size)}
    if content_type:
        headers["Content-Type"] = content_type
    delay = 1.0
    for attempt in range(1, attempts + 1):
        data = body() if callable(body) else body
        req = urllib.request.Request(url, data=data, method="PUT", headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                resp.read()
                return resp.headers.get("ETag", "")
        except (urllib.error.URLError, ConnectionError) as err:
            if attempt == attempts:
                raise RuntimeError(f"storage PUT failed: {err}") from None
            time.sleep(delay)
            delay *= 2
        finally:
            if callable(body) and hasattr(data, "close"):
                data.close()
    raise RuntimeError("unreachable")
