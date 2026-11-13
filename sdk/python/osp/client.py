"""HTTP client for the platform API: sign-in, token refresh, retries."""

import http.client
import io
import json
import random
import re
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

# Methods that do the same thing however often they are sent.
SAFE_TO_REPEAT = {"GET", "HEAD", "PUT", "DELETE", "OPTIONS"}


def session_gone(err):
    """Whether the API said an upload session has ended or never existed:
    404, or 409 "multipart upload is not active". Other 409s (a start still
    pending, an idempotency conflict) are not this."""
    if not isinstance(err, APIError):
        return False
    return err.status in (404, 410) or (err.status == 409 and "not active" in err.body)


def backoff(attempt, rand=random.random):
    """Seconds to wait before try attempt + 1: exponential with full jitter,
    capped at 30, so clients that failed together do not return together."""
    return rand() * min(30.0, 2.0 ** attempt)


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
            # Sent once only: the refresh token is single use, and presenting
            # it again after a lost answer revokes the session. A network
            # error is raised for the caller to sign in again.
            tokens = self._send("POST", "/api/v1/auth/refresh", {"refresh_token": self.refresh_token}, auth=False,
                                retry=False)
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

    def request(self, method, path, body=None, params=None, headers=None, raw=None, content_type=None, retry=None):
        """Sends one API request and returns the decoded JSON (None for 204).

        A 401 refreshes the session once and sends again. Network errors,
        429 and 5xx gateway answers are retried with backoff, but only for a
        request that cannot take effect twice: a GET, PUT or DELETE, one with
        an Idempotency-Key header or an idempotency_key in its body, or one
        the caller marks with retry=True.
        """
        if params:
            query = {k: v for k, v in params.items() if v not in (None, "")}
            if query:
                path = path + "?" + urllib.parse.urlencode(query)
        try:
            return self._send(method, path, body, headers=headers, raw=raw, content_type=content_type, retry=retry)
        except APIError as err:
            if err.status != 401 or not self.refresh_token:
                raise
        self.refresh()
        return self._send(method, path, body, headers=headers, raw=raw, content_type=content_type, retry=retry)

    def get_text(self, path, **params):
        """GETs a route that answers text (an export) and returns it."""
        return self.request("GET", path, params=params, headers={"Accept": "*/*"})

    def _send(self, method, path, body=None, auth=True, headers=None, raw=None, content_type=None, retry=None):
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
        if retry is None:
            retry = (method in SAFE_TO_REPEAT or "Idempotency-Key" in all_headers
                     or (isinstance(body, dict) and bool(body.get("idempotency_key"))))
        attempts = self.max_attempts if retry else 1

        for attempt in range(1, attempts + 1):
            try:
                resp = self._roundtrip(method, self.base_url + path, data, all_headers, self.timeout)
                payload = resp.read()
            except (http.client.HTTPException, OSError):
                if attempt == attempts:
                    raise
                time.sleep(backoff(attempt - 1))
                continue
            if resp.status in RETRY_STATUSES and attempt < attempts:
                time.sleep(_retry_after(resp.headers) or backoff(attempt - 1))
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
    def fetch(self, url, timeout=600, headers=None):
        """GETs url, a download link from the API or storage, over a
        kept-alive connection, and yields the response to read from. A
        response not read to its end closes its connection."""
        key = (urllib.parse.urlsplit(url).scheme, urllib.parse.urlsplit(url).netloc)
        resp = self._roundtrip("GET", url, None, headers or {}, timeout)
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
        refreshed = False
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
                time.sleep(_retry_after(resp.headers) or backoff(attempt - 1))
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

    def post(self, path, body=None, headers=None, retry=None):
        return self.request("POST", path, body=body, headers=headers, retry=retry)

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


# A PUT with no progress for this long is stalled: it is dropped and tried
# again. The bytes go up 8 KiB at a time and the socket timeout applies to
# each piece, so a slow link that keeps moving is never cut off.
STALL_SECONDS = 30

# What S3, GCS and Azure say when a presigned URL has expired (or the clock
# is off). S3 sends an expired URL as AccessDenied, so the code alone means a
# real refusal.
_EXPIRED = re.compile(
    r"Request has expired|ExpiredToken|token has expired|signature (has )?expired"
    r"|not valid in the specified time frame|RequestTimeTooSkewed",
    re.IGNORECASE,
)


def put_bytes(url, body, size, content_type=None, timeout=STALL_SECONDS, attempts=5, headers=None, refresh=None):
    """PUTs to a presigned storage URL and returns the ETag. body is bytes
    or a callable returning a fresh file-like object for each attempt.
    headers are the upload_headers the API asked for (Azure's blob type,
    or a connection's customer-managed key).

    timeout is how long the PUT may go without progress. Network errors,
    stalls, 408, 429 and 5xx are tried again with backoff (Retry-After
    wins); refresh, when given, returns a new URL for one that expired. Any
    other refusal would fail the same way again and is raised at once."""
    headers = {**(headers or {}), "Content-Length": str(size)}
    if content_type:
        headers["Content-Type"] = content_type
    last = None
    for attempt in range(attempts):
        data = body() if callable(body) else io.BytesIO(body)
        req = urllib.request.Request(url, data=data, method="PUT", headers=headers)
        wait = None
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                resp.read()
                return resp.headers.get("ETag", "")
        except urllib.error.HTTPError as err:
            text = err.read().decode("utf-8", "replace")
            if err.code == 403 and refresh and _EXPIRED.search(text):
                url = refresh()
                last = "the URL expired"
                continue
            if err.code not in (408, 429) and err.code < 500:
                raise RuntimeError(f"storage refused the upload: HTTP {err.code}: {text[:200]}") from None
            last = f"HTTP {err.code}"
            wait = _retry_after(err.headers)
        except (OSError, http.client.HTTPException) as err:
            last = err  # a dropped connection or a stall
        finally:
            if hasattr(data, "close"):
                data.close()
        if attempt < attempts - 1:
            time.sleep(wait or backoff(attempt))
    raise RuntimeError(f"storage PUT failed: {last}")
