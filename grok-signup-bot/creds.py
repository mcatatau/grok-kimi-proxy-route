"""Credential generation and persistent storage for auto-register."""

from __future__ import annotations

import json
import os
import random
import string
import time


def random_password(length: int = 20) -> str:
    """Generate a strong password like s1zV0bQL5UYjff7WNmmp."""
    chars = string.ascii_letters + string.digits + "!@#$%^&*"
    return "".join(random.choice(chars) for _ in range(length))


def random_name(prefix: str = "User") -> str:
    """Generate a random display name like User a8f3k."""
    suffix = "".join(random.choice(string.ascii_lowercase + string.digits) for _ in range(6))
    return f"{prefix} {suffix}"


def random_email(inbox_address: str) -> str:
    """Return the provider email directly (we use the inbox address)."""
    return inbox_address


class CredsStore:
    """Save/load generated credentials to a JSON file."""

    def __init__(self, creds_dir: str) -> None:
        self.path = os.path.join(creds_dir, "auto_creds.json") if creds_dir else ""

    def save(self, email: str, name: str, password: str, provider: str = "") -> dict:
        entry = {
            "email": email,
            "name": name,
            "password": password,
            "provider": provider,
            "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }
        if not self.path:
            return entry
        os.makedirs(os.path.dirname(self.path), exist_ok=True)
        entries: list[dict] = []
        if os.path.exists(self.path):
            try:
                with open(self.path) as f:
                    entries = json.load(f)
            except (json.JSONDecodeError, OSError):
                entries = []
        entries.append(entry)
        with open(self.path, "w") as f:
            json.dump(entries, f, indent=2)
        return entry