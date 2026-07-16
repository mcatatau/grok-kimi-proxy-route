"""Teste manual: StartDevice OAuth + grok_signup.py

Usage:
    python3 test_flow.py [--headless false]

Faz o device flow, pega verification_uri, chama grok_signup.py.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.parse
import urllib.request

CLIENT_ID = "b1a00492-073a-47ea-816f-4c329264a828"
ISSUER = "https://auth.x.ai"
SCOPES = "openid profile email offline_access api:access grok-cli:access conversations:read conversations:write"


def start_device() -> dict:
    url = f"{ISSUER}/oauth2/device/code"
    data = urllib.parse.urlencode({
        "client_id": CLIENT_ID,
        "scope": SCOPES,
    }).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.loads(resp.read())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--headless", default="true")
    args = parser.parse_args()

    print("[*] Starting device login...")
    dev = start_device()
    print(f"  user_code:       {dev.get('user_code')}")
    print(f"  verification_uri: {dev.get('verification_uri_complete') or dev.get('verification_uri')}")
    print(f"  device_code:     {dev.get('device_code')[:20]}...")
    print()

    url = dev.get("verification_uri_complete") or dev.get("verification_uri")
    user_code = dev.get("user_code", "")

    script = os.path.join(os.path.dirname(__file__), "grok_signup.py")
    venv_python = os.path.join(os.path.dirname(__file__), "..", ".venv", "bin", "python3")
    python_exe = venv_python if os.path.exists(venv_python) else sys.executable
    cmd = [
        python_exe, script,
        "--verification-url", url,
        "--headless", args.headless,
        "--user-code", user_code,
    ]

    print(f"[*] Running: {' '.join(cmd)}")
    print()

    env = os.environ.copy()
    env["PYTHONUNBUFFERED"] = "1"

    proc = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=env,
        text=True,
        bufsize=1,
    )

    for line in proc.stdout:
        line = line.rstrip()
        print(line)
        if line.startswith("__RESULT__"):
            result = json.loads(line[10:])
            if result.get("status") == "success":
                print("\n✅ Signup concluído!")
            else:
                print(f"\n❌ Falha: {result}")
            break

    proc.wait()


if __name__ == "__main__":
    main()