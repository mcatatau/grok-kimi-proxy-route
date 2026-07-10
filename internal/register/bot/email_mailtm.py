"""Mail.tm temporary email backend (https://api.mail.tm)."""

from __future__ import annotations

import json
import random
import re
import string
import time
import urllib.error
import urllib.request
from typing import Any

API = "https://api.mail.tm"
OTP_RE = re.compile(r"([A-Z0-9]{3})[-\s]([A-Z0-9]{3})")
XAI_HINT = re.compile(r"x\.ai|verification|verify|code|grok|confirmation", re.I)


class MailTmProvider:
    name = "mailtm"

    def __init__(self, base_url: str = API, timeout: float = 30) -> None:
        self.base_url = base_url.rstrip("/")
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
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if token:
            headers["Authorization"] = f"Bearer {token}"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
                if not raw:
                    return None
                return json.loads(raw.decode("utf-8"))
        except urllib.error.HTTPError as e:
            err_body = e.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"mail.tm {method} {path}: HTTP {e.code} {err_body}") from e
        except urllib.error.URLError as e:
            raise RuntimeError(f"mail.tm {method} {path}: {e}") from e

    def create_inbox(self) -> dict:
        raw = self._req("GET", "/domains")
        if isinstance(raw, list):
            members = raw
        else:
            members = raw.get("hydra:member") or raw.get("member") or []
        active = [
            d.get("domain")
            for d in members
            if d.get("isActive", True) and d.get("domain")
        ]
        if not active:
            raise RuntimeError("mail.tm: no active domains")
        domain = random.choice(active)
        local = "gp" + "".join(random.choices(string.ascii_lowercase + string.digits, k=10))
        address = f"{local}@{domain}"
        password = "".join(random.choices(string.ascii_letters + string.digits, k=16))

        acc = self._req("POST", "/accounts", {"address": address, "password": password})
        acc_id = acc.get("id") or ""
        tok = self._req("POST", "/token", {"address": address, "password": password})
        token = tok.get("token") or ""
        if not token:
            raise RuntimeError("mail.tm: empty token after create")
        return {
            "address": address,
            "id": acc_id,
            "token": token,
            "password": password,
            "provider": self.name,
        }

    def fetch_code(self, inbox: dict, since_ms: int, timeout: float = 90) -> str | None:
        token = inbox.get("token") or ""
        if not token:
            raise RuntimeError("mail.tm: inbox missing token")
        deadline = time.time() + timeout
        seen: set[str] = set()
        while time.time() < deadline:
            raw = self._req("GET", "/messages", token=token) or {}
            if isinstance(raw, list):
                members = raw
            else:
                members = raw.get("hydra:member") or raw.get("member") or []
            for msg in members:
                mid = msg.get("id") or ""
                if not mid or mid in seen:
                    continue
                # Prefer messages that look like xAI verification
                subj = (msg.get("subject") or "") + " " + (msg.get("intro") or "")
                from_addr = ""
                fr = msg.get("from") or {}
                if isinstance(fr, dict):
                    from_addr = fr.get("address") or ""
                detail = self._req("GET", f"/messages/{mid}", token=token) or {}
                seen.add(mid)
                created = detail.get("createdAt") or msg.get("createdAt") or ""
                # Soft filter by time if parseable; still accept if unclear
                body = _message_text(detail)
                blob = f"{subj}\n{from_addr}\n{body}"
                if not XAI_HINT.search(blob) and "x.ai" not in from_addr.lower():
                    pass
                code = _extract_otp(blob)
                if code:
                    # Prefer codes from recent messages when createdAt present
                    if since_ms and created:
                        try:
                            # 2024-01-01T12:00:00+00:00
                            from datetime import datetime

                            t = datetime.fromisoformat(created.replace("Z", "+00:00"))
                            if int(t.timestamp() * 1000) < since_ms - 60_000:
                                continue
                        except Exception:
                            pass
                    return code
            time.sleep(2.5)
        return None


def _message_text(detail: dict) -> str:
    parts: list[str] = []
    for key in ("text", "html", "intro", "subject"):
        v = detail.get(key)
        if isinstance(v, str) and v:
            parts.append(v)
    return "\n".join(parts)


def _extract_otp(text: str) -> str | None:
    best: str | None = None
    for m in OTP_RE.finditer(text):
        code = m.group(0)
        if code in ("000-000", "123-456"):
            continue
        start = max(0, m.start() - 80)
        window = text[start : m.end() + 80]
        if XAI_HINT.search(window):
            return code
        if best is None:
            best = code
    return best


if __name__ == "__main__":
    p = MailTmProvider()
    inbox = p.create_inbox()
    print(json.dumps({k: v for k, v in inbox.items() if k != "password"}, indent=2))
    print("inbox ready; waiting 15s for any mail (manual send test)…")
    code = p.fetch_code(inbox, since_ms=int(time.time() * 1000) - 1000, timeout=15)
    print("code:", code)
