"""Playwright automation for xAI signup via OAuth Device Login flow.

Usage:
    python3 grok_signup.py \\
        --verification-url https://auth.x.ai/activate?user_code=XXXXXX \\
        [--headless]

Stdout protocol (parsed by Go bridge):
    __STEP__ <step>
    __RESULT__ {"status":"success|error","reason":"...","step":"..."}
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time

from playwright.sync_api import Page, Playwright, sync_playwright, expect

from creds import CredsStore, random_name, random_password


def log(msg: str) -> None:
    print(msg, flush=True)


def resolve(
    page: Page,
    selectors: list[str],
    timeout: int = 10000,
) -> Any | None:
    """Try selectors in order; return first visible locator or None."""
    from playwright.sync_api import Locator

    last_err: Exception | None = None
    for sel in selectors:
        try:
            loc = page.locator(sel)
            if loc.first.is_visible(timeout=timeout):
                return loc
        except Exception as e:
            last_err = e
            continue
    return None


def fail(step: str, reason: str, page: Page | None = None) -> None:
    info = {"status": "error", "step": step, "reason": reason}
    if page:
        try:
            info["screenshot"] = page.screenshot(type="png", full_page=True).hex()
        except Exception:
            pass
    log(f"__RESULT__ {json.dumps(info)}")
    sys.exit(1)


def wait_and_click(
    page: Page,
    selectors: list[str],
    step: str,
    timeout: int = 15000,
) -> None:
    loc = resolve(page, selectors, timeout=timeout)
    if loc is None:
        fail(step, f"no selector matched: {selectors}", page)
    try:
        loc.first.click(timeout=5000)
    except Exception as e:
        fail(step, f"click failed: {e}", page)


def wait_and_fill(
    page: Page,
    selectors: list[str],
    value: str,
    step: str,
    timeout: int = 10000,
) -> None:
    loc = resolve(page, selectors, timeout=timeout)
    if loc is None:
        fail(step, f"no selector matched: {selectors}", page)
    try:
        loc.first.fill(value, timeout=5000)
    except Exception as e:
        fail(step, f"fill failed: {e}", page)


def run_signup(
    pw: Playwright,
    verification_url: str,
    headless: bool = True,
    proxy_url: str | None = None,
    user_code: str | None = None,
    creds_dir: str | None = None,
) -> None:
    log("__STEP__ launching")

    launch_opts: dict = {
        "headless": headless,
        "args": [
            "--no-sandbox",
            "--disable-setuid-sandbox",
            "--disable-dev-shm-usage",
            "--disable-blink-features=AutomationControlled",
        ],
    }
    if proxy_url:
        launch_opts["proxy"] = {"server": proxy_url}

    browser = pw.chromium.launch(**launch_opts)
    context = browser.new_context(
        viewport={"width": 1280, "height": 900},
        user_agent=(
            "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
        ),
    )
    page = context.new_page()

    creds_store = CredsStore(creds_dir or os.environ.get("CREDS_DIR", ""))

    try:
        log("__STEP__ device")
        page.goto(verification_url, wait_until="domcontentloaded", timeout=30000)
        page.wait_for_load_state("networkidle", timeout=20000)
        time.sleep(1.5)

        # 1. Click "Continuar" to acknowledge device pairing
        log("__STEP__ continue")
        wait_and_click(
            page,
            [
                "text=Continuar",
                "button:has-text('Continuar')",
                "a:has-text('Continuar')",
                "[data-testid=continue]",
                "button:has-text('Continue')",
                "text=Continue",
            ],
            "continue",
        )
        time.sleep(1.5)

        # 2. Click "Sign up" / "Criar conta"
        log("__STEP__ signup")
        wait_and_click(
            page,
            [
                "text=Criar conta",
                "button:has-text('Criar conta')",
                "text=Sign up",
                "button:has-text('Sign up')",
                "[data-testid=signup-btn]",
                "a:has-text('Registrar')",
            ],
            "signup",
        )
        time.sleep(1)

        # 3. Fill email from provider
        from email_provider import build_providers, create_inbox_with_fallback

        providers = build_providers(
            names=["duckmail", "mailtm"],
            duckmail_url=os.environ.get("DUCKMAIL_URL", ""),
            duckmail_key=os.environ.get("DUCKMAIL_KEY", ""),
        )
        inbox = create_inbox_with_fallback(providers)
        email_addr = inbox["address"]
        log(f"__STEP__ email {inbox.get('provider', '?')} {email_addr}")

        wait_and_fill(
            page,
            ["#email", "input[name=email]", "input[type=email]", "[data-testid=email-input]"],
            email_addr,
            "email",
        )

        # Submit email (press Enter or click next)
        page.keyboard.press("Enter")
        time.sleep(1.5)

        # 4. Wait for OTP via email provider
        log("__STEP__ otp")
        provider = email_provider.provider_for_inbox(providers, inbox)
        since_ms = int(time.time() * 1000)
        code = provider.fetch_code(inbox, since_ms=since_ms, timeout=120)
        if not code:
            fail("otp", "timeout waiting for OTP", page)

        # Fill OTP
        wait_and_fill(
            page,
            ["#otp", "input[name=otp]", "input[data-testid=otp]", "input.otp-input"],
            code,
            "otp",
        )
        page.keyboard.press("Enter")
        time.sleep(1.5)

        # 5. Fill name + password
        log("__STEP__ profile")
        name = random_name()
        password = random_password()

        wait_and_fill(
            page,
            ["#name", "input[name=name]", "[data-testid=name-input]"],
            name,
            "name",
        )
        wait_and_fill(
            page,
            ["#password", "input[name=password]", "[data-testid=password-input]"],
            password,
            "password",
        )

        # Submit profile (might be a button or Enter)
        submit_btn = resolve(
            page,
            [
                "button:has-text('Continuar')",
                "button:has-text('Sign up')",
                "button:has-text('Criar conta')",
                "[type=submit]",
            ],
            timeout=5000,
        )
        if submit_btn:
            submit_btn.first.click(timeout=5000)
        else:
            page.keyboard.press("Enter")
        time.sleep(2)

        # 6. Turnstile — wait for it to resolve (extension handles it)
        log("__STEP__ turnstile")
        try:
            expect(page.locator("iframe[src*=turnstile]").first).to_be_hidden(timeout=15000)
        except Exception:
            pass  # no turnstile shown, that's fine
        time.sleep(1)

        # 7. Allow the device
        log("__STEP__ allow")
        wait_and_click(
            page,
            [
                "text=Allow",
                "button:has-text('Allow')",
                "text=Permitir",
                "button:has-text('Permitir')",
                "[data-testid=allow]",
                "button:has-text('Autorizar')",
            ],
            "allow",
        )
        time.sleep(2)

        # 8. Sign out to clean session
        log("__STEP__ signout")
        signout_selectors = [
            "text=Sair",
            "text=Sign out",
            "button:has-text('Sair')",
            "a:has-text('Sign out')",
            "a:has-text('Logout')",
            "button:has-text('Logout')",
        ]
        signout_btn = resolve(page, signout_selectors, timeout=5000)
        if signout_btn:
            signout_btn.first.click(timeout=5000)
            time.sleep(1.5)

        log("__STEP__ done")
        entry = creds_store.save(email_addr, name, password, inbox.get("provider", ""))
        log(f"__CREDS__ {json.dumps(entry)}")
        log('__RESULT__ {"status":"success"}')

    except Exception as e:
        fail("runtime", str(e), page)
    finally:
        context.close()
        browser.close()


def main() -> None:
    parser = argparse.ArgumentParser(description="xAI signup via device login")
    parser.add_argument("--verification-url", required=True)
    parser.add_argument("--user-code")
    parser.add_argument("--headless", default="true")
    parser.add_argument("--proxy")
    parser.add_argument("--creds-dir", default="", help="directory to save auto_creds.json")
    args = parser.parse_args()

    headless = args.headless.lower() not in ("false", "0", "no")

    with sync_playwright() as pw:
        run_signup(
            pw,
            verification_url=args.verification_url,
            headless=headless,
            proxy_url=args.proxy,
            user_code=args.user_code,
            creds_dir=args.creds_dir,
        )


if __name__ == "__main__":
    main()