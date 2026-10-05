"""Minimal synchronous tool adapter. It does not call a model or execute code."""
from __future__ import annotations
import base64
import ipaddress
import json
import os
import stat
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

class APIError(RuntimeError):
    def __init__(self, status: int, code: str):
        self.status, self.code = status, code
        super().__init__(f"BranchHarbor request failed: HTTP {status} ({code})")

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None

class Client:
    def __init__(self, url: str, token_file: str | Path):
        parts = urllib.parse.urlsplit(url)
        if parts.scheme != "http" or parts.username or parts.password or parts.path not in ("", "/") or parts.query or parts.fragment:
            raise ValueError("Use an explicit loopback HTTP base URL")
        try:
            allowed = ipaddress.ip_address(parts.hostname or "").is_loopback
        except ValueError:
            allowed = False
        if not allowed:
            raise ValueError("Only explicit loopback addresses are supported")
        fd = os.open(token_file, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, "r", encoding="ascii") as f:
            mode = os.fstat(f.fileno()).st_mode
            if not stat.S_ISREG(mode) or stat.S_IMODE(mode) & 0o077:
                raise ValueError("Token file must be owner-only")
            self._token = f.read(4097).strip()
        if len(self._token) > 4096 or not self._token.startswith("BH1."):
            raise ValueError("Invalid token file")
        self.url = url.rstrip("/")
        self._opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def request(self, method: str, route: str, body: Any = None) -> Any:
        if not route.startswith("/") or route.startswith("//"):
            raise ValueError("Expected a relative API path")
        payload = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        req = urllib.request.Request(self.url + route, data=payload, method=method,
            headers={"Authorization": "Bearer " + self._token, "Content-Type": "application/json"})
        try:
            with self._opener.open(req, timeout=15) as response:
                return json.load(response)
        except urllib.error.HTTPError as exc:
            try:
                code = json.load(exc).get("error", {}).get("code", "unknown")
            except (ValueError, AttributeError):
                code = "invalid_response"
            raise APIError(exc.code, code) from None

    def head(self, branch: str) -> dict[str, Any]:
        return self.request("GET", "/v1/head?" + urllib.parse.urlencode({"branch": branch}))

    def commit(self, branch: str, expected: dict[str, Any], request_id: str,
               puts: dict[str, bytes], deletes: list[str] | None = None) -> dict[str, Any]:
        return self.request("POST", "/v1/commit", {"branch": branch, "expected": expected,
            "request_id": request_id, "puts": {p: base64.b64encode(b).decode("ascii") for p, b in puts.items()},
            "deletes": deletes or []})
