"""DuckMail self-hosted temporary email backend (optional)."""

from __future__ import annotations

import json
import re
import time
import urllib.error
import urllib.request
from typing import Any

OTP_RE = re.compile(r"\b(\d{6})\b")
XAI_HINT = re.compile(r"x\.ai|verification|verify|code|grok", re.I)


class DuckMailProvider:
    """
    Minimal client for a DuckMail-compatible HTTP API.

    Expected (configurable paths if your fork differs):
      POST {base}/api/mailboxes  Authorization: Bearer {key}
        → { id, address|email, token? }
      GET  {base}/api/mailboxes/{id}/messages
        → { messages: [ { id, subject, from, body|text|html, date } ] }
    """

    name = "duckmail"

    def __init__(
        self,
        base_url: str = "http://127.0.0.1:3000",
        api_key: str = "",
        timeout: float = 30,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def _req(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        token: str | None = None,
    ) -> Any:
        url = self.base_url + path
        data = None
        headers = {"Accept": "application/json"}
        key = token or self.api_key
        if key:
            headers["Authorization"] = f"Bearer {key}"
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
                if not raw:
                    return None
                return json.loads(raw.decode("utf-8"))
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"duckmail {method} {path}: HTTP {e.code} {err_body}") from e
        except urllib.error.URLError as e:
            raise RuntimeError(f"duckmail {method} {path}: {e}") from e

    def create_inbox(self) -> dict:
        if not self.api_key and not self.base_url:
            raise RuntimeError("duckmail: DUCKMAIL_URL / DUCKMAIL_KEY required")
        # Try common create endpoints
        last_err: Exception | None = None
        for path, body in (
            ("/api/mailboxes", {}),
            ("/api/mailbox", {}),
            ("/mailboxes", {}),
        ):
            try:
                acc = self._req("POST", path, body) or {}
                address = acc.get("address") or acc.get("email") or acc.get("mailbox")
                mid = acc.get("id") or acc.get("mailbox_id") or ""
                token = acc.get("token") or self.api_key
                if address:
                    return {
                        "address": address,
                        "id": str(mid),
                        "token": token,
                        "provider": self.name,
                    }
            except Exception as e:
                last_err = e
                continue
        raise RuntimeError(f"duckmail: create failed ({last_err})")

    def fetch_code(self, inbox: dict, since_ms: int, timeout: float = 90) -> str | None:
        mid = inbox.get("id") or ""
        token = inbox.get("token") or self.api_key
        if not mid:
            raise RuntimeError("duckmail: inbox missing id")
        deadline = time.time() + timeout
        paths = (
            f"/api/mailboxes/{mid}/messages",
            f"/api/mailbox/{mid}/messages",
            f"/mailboxes/{mid}/messages",
        )
        while time.time() < deadline:
            for path in paths:
                try:
                    listing = self._req("GET", path, token=token) or {}
                except Exception:
                    continue
                msgs = (
                    listing.get("messages")
                    or listing.get("hydra:member")
                    or listing.get("data")
                    or []
                )
                if isinstance(listing, list):
                    msgs = listing
                for msg in msgs:
                    if not isinstance(msg, dict):
                        continue
                    blob = " ".join(
                        str(msg.get(k) or "")
                        for k in ("subject", "from", "text", "body", "html", "intro")
                    )
                    m = OTP_RE.search(blob)
                    if m and (XAI_HINT.search(blob) or True):
                        code = m.group(1)
                        if code not in ("000000", "123456"):
                            return code
            time.sleep(2.5)
        return None
