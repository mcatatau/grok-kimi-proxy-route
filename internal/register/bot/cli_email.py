#!/usr/bin/env python3
"""CLI smoke: create inbox via provider list and optionally wait for OTP."""

from __future__ import annotations

import argparse
import json
import os
import time

from email_provider import (
    build_providers,
    create_inbox_with_fallback,
    fetch_code_with_provider,
    provider_for_inbox,
)


def main() -> int:
    ap = argparse.ArgumentParser(description="Test temporary email providers")
    ap.add_argument(
        "--providers",
        default=os.environ.get("EMAIL_PROVIDERS", "mailtm"),
        help="comma list: mailtm,duckmail",
    )
    ap.add_argument("--duckmail-url", default=os.environ.get("DUCKMAIL_URL", ""))
    ap.add_argument("--duckmail-key", default=os.environ.get("DUCKMAIL_KEY", ""))
    ap.add_argument("--wait-otp", type=float, default=0, help="seconds to poll for OTP")
    args = ap.parse_args()

    names = [x.strip() for x in args.providers.split(",") if x.strip()]
    providers = build_providers(
        names,
        duckmail_url=args.duckmail_url,
        duckmail_key=args.duckmail_key,
    )
    inbox = create_inbox_with_fallback(providers)
    safe = {k: v for k, v in inbox.items() if k != "password"}
    print(json.dumps(safe, indent=2), flush=True)

    if args.wait_otp > 0:
        p = provider_for_inbox(providers, inbox)
        since = int(time.time() * 1000) - 2_000
        print(f"__STEP__ otp waiting {args.wait_otp}s on {p.name}", flush=True)
        code = fetch_code_with_provider(p, inbox, since_ms=since, timeout=args.wait_otp)
        print(json.dumps({"code": code}), flush=True)
        return 0 if code else 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
