"""Email provider protocol, factory, and create_inbox fallback chain."""

from __future__ import annotations

import time
from typing import Protocol, runtime_checkable


@runtime_checkable
class EmailProvider(Protocol):
    name: str

    def create_inbox(self) -> dict:
        """Return {address, id, token, provider?, ...} for OTP polling."""

    def fetch_code(self, inbox: dict, since_ms: int, timeout: float = 90) -> str | None:
        """Poll until a 6-digit xAI code is found, or timeout → None."""


def build_providers(
    names: list[str] | None = None,
    *,
    duckmail_url: str = "",
    duckmail_key: str = "",
) -> list[EmailProvider]:
    """Build providers in order. Default: mailtm only."""
    if not names:
        names = ["duckmail", "mailtm"]

    out: list[EmailProvider] = []
    for raw in names:
        name = (raw or "").strip().lower()
        if not name:
            continue
        if name in ("mailtm", "mail.tm", "mail_tm"):
            from email_mailtm import MailTmProvider

            out.append(MailTmProvider())
        elif name in ("duckmail", "duck"):
            from email_duckmail import DuckMailProvider

            out.append(
                DuckMailProvider(
                    base_url=duckmail_url or "http://127.0.0.1:3000",
                    api_key=duckmail_key,
                )
            )
        else:
            raise ValueError(f"unknown email provider: {raw!r}")
    if not out:
        raise ValueError("no email providers configured")
    return out


def create_inbox_with_fallback(providers: list[EmailProvider]) -> dict:
    """Try each provider until create_inbox succeeds."""
    errors: list[str] = []
    for p in providers:
        try:
            inbox = p.create_inbox()
            inbox = dict(inbox)
            inbox["provider"] = getattr(p, "name", "unknown")
            print(f"__STEP__ email {inbox['provider']}", flush=True)
            return inbox
        except Exception as e:
            errors.append(f"{getattr(p, 'name', '?')}: {e}")
            print(
                f"__STEP__ email_fallback {getattr(p, 'name', '?')} failed",
                flush=True,
            )
    raise RuntimeError("no email provider worked: " + "; ".join(errors))


def fetch_code_with_provider(
    provider: EmailProvider,
    inbox: dict,
    since_ms: int | None = None,
    timeout: float = 90,
) -> str | None:
    """Fetch OTP using the same provider that created the inbox (no mid-switch)."""
    if since_ms is None:
        since_ms = int(time.time() * 1000) - 5_000
    return provider.fetch_code(inbox, since_ms=since_ms, timeout=timeout)


def provider_for_inbox(providers: list[EmailProvider], inbox: dict) -> EmailProvider:
    name = (inbox.get("provider") or "").lower()
    for p in providers:
        if getattr(p, "name", "") == name:
            return p
    if len(providers) == 1:
        return providers[0]
    raise RuntimeError(f"no provider matching inbox provider={name!r}")
