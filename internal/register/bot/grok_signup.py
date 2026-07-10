"""Grok/xAI signup via DrissionPage + turnstilePatch extension for antiblock.

Usage:
    python3 grok_signup.py \
        --verification-url https://accounts.x.ai/oauth2/device?user_code=XXXXXX \
        [--headless false]

Stdout protocol:
    __STEP__ <step>
    __CREDS__ {"email":"...","name":"...","password":"...","provider":"..."}
    __RESULT__ {"status":"success|error","reason":"...","step":"..."}
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time

from DrissionPage import Chromium, ChromiumOptions

from creds import CredsStore, random_name, random_password


def log(msg: str) -> None:
    print(msg, flush=True)


def fail(step: str, reason: str) -> None:
    log(f"__RESULT__ {json.dumps({'status': 'error', 'step': step, 'reason': reason})}")
    sys.exit(1)


def submit_via_js(tab) -> None:
    try:
        tab.run_js("""
const btn = document.querySelector('button[type="submit"]');
if (btn && !btn.disabled) btn.click();
""")
    except Exception:
        pass


def dismiss_cookies(tab) -> None:
    for text in ("Accept All Cookies", "Reject All", "Accept all cookies", "Accept cookies"):
        try:
            el = tab.ele(f"tag:button@@text()={text}", timeout=2)
            if el and el.states.is_displayed:
                el.click()
                return
        except Exception:
            pass
    try:
        tab.run_js("""
document.querySelectorAll('button').forEach(b => {
    const t = (b.textContent||'').trim().toLowerCase();
    if (t.includes('accept all') || t.includes('accept cookies') || t.includes('reject all')) {
        b.click();
    }
});
""")
    except Exception:
        pass


def dump_dom(tab) -> str:
    try:
        return tab.run_js("""
const r = {url: location.href, inputs:[], buttons:[]};
document.querySelectorAll('input').forEach(el => {
    r.inputs.push({name:el.name,type:el.type,testid:el.getAttribute('data-testid'),placeholder:el.placeholder,autocomplete:el.autocomplete,visible:!!(el.offsetParent||el.getClientRects().length)});
});
document.querySelectorAll('button,a,[role=button]').forEach(el => {
    const t = (el.textContent||'').trim().slice(0,60);
    if (t) r.buttons.push({tag:el.tagName,text:t,visible:!!(el.offsetParent||el.getClientRects().length)});
});
return JSON.stringify(r);
""")
    except Exception as e:
        return f'{{"err":"{e}"}}'


def click_by_labels(tab, labels: list[str], *, exact: bool = False) -> str | None:
    labels_js = json.dumps([x.lower() for x in labels])
    exact_js = "true" if exact else "false"
    try:
        return tab.run_js(f"""
const labels = {labels_js};
const exact = {exact_js};
const nodes = Array.from(document.querySelectorAll('button,a,[role=button],input[type=submit]'));
const btn = nodes.find(b => {{
  const t = (b.textContent || b.value || '').trim().toLowerCase().replace(/\\s+/g, ' ');
  if (!t) return false;
  return labels.some(l => exact ? (t === l) : (t === l || t.includes(l) || t.startsWith(l + ' ')));
}});
if (btn) {{
  btn.click();
  return (btn.textContent || btn.value || '').trim().slice(0, 80);
}}
return null;
""")
    except Exception:
        return None


def page_blob(tab) -> str:
    try:
        url = ""
        try:
            url = tab.url or ""
        except Exception:
            pass
        body = tab.run_js(
            "return ((document.body && document.body.innerText) || '').slice(0, 4000);"
        ) or ""
        return f"{url}\n{body}".lower()
    except Exception:
        return ""


def dismiss_x_login_gate(tab) -> bool:
    """Leave 'logged in to X' wall via email/signup; never click Continue with X."""
    blob = page_blob(tab)
    hints = (
        "logged in to x",
        "log in to x",
        "login to x",
        "continue with x",
        "sign in with x",
        "twitter",
    )
    if not any(h in blob for h in hints):
        return False
    log("gate: X login wall detected — switching to email/signup path")
    for labels in (
        ["sign up with email", "cadastrar com e-mail", "cadastrar com email", "register with email"],
        ["sign up", "criar conta", "create account", "register"],
        ["log in with email", "login with email", "entrar com e-mail", "entrar com email", "use email"],
        ["continue with email", "continuar com e-mail", "continuar com email"],
        ["sign in with email", "entrar com email"],
    ):
        hit = click_by_labels(tab, labels)
        if hit:
            log(f"gate: clicked {hit!r}")
            time.sleep(2)
            return True
    try:
        hit = tab.run_js("""
const a = Array.from(document.querySelectorAll('a,button')).find(el => {
  const t = (el.textContent||'').trim().toLowerCase();
  const href = (el.href || el.getAttribute('href') || '').toLowerCase();
  if (href.includes('twitter.com') || href.includes('x.com/i/oauth') || href.includes('api.x.com')) return false;
  if (t.includes('with x') || t === 'x' || t.includes('twitter')) return false;
  return t.includes('sign up') || t.includes('email') || href.includes('sign-up') || href.includes('signup');
});
if (a) { a.click(); return (a.textContent||'').trim().slice(0,80); }
return null;
""")
        if hit:
            log(f"gate: fallback click {hit!r}")
            time.sleep(2)
            return True
    except Exception as e:
        log(f"gate: fallback error {e}")
    log("gate: could not leave X wall (no email/signup control found)")
    return False


def run_signup(
    verification_url: str,
    headless: bool = True,
    creds_dir: str | None = None,
    email_providers: list[str] | None = None,
    duckmail_url: str = "",
    duckmail_key: str = "",
) -> None:
    creds_store = CredsStore(creds_dir or os.environ.get("CREDS_DIR", ""))

    ext_path = os.path.abspath(
        os.path.join(os.path.dirname(__file__), "turnstilePatch")
    )

    co = ChromiumOptions(read_file=False)
    # set_user_data_path clears auto_port; pick a free debug port ourselves so
    # connect_browser can do ip, port = address.split(':') safely.
    import socket
    import tempfile

    def _free_port() -> int:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
            s.bind(("127.0.0.1", 0))
            return int(s.getsockname()[1])

    _dbg_port = _free_port()
    co.set_local_port(_dbg_port)
    _tmp_dir = tempfile.mkdtemp(prefix="grok_signup_")
    co.set_user_data_path(_tmp_dir)
    co.set_tmp_path(_tmp_dir)
    log(f"chrome: debug port={_dbg_port} user_data={_tmp_dir}")

    def _find_browser() -> str:
        candidates = [
            # Windows — Chrome
            os.path.expandvars(r"%ProgramFiles%\Google\Chrome\Application\chrome.exe"),
            os.path.expandvars(r"%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe"),
            os.path.expandvars(r"%LocalAppData%\Google\Chrome\Application\chrome.exe"),
            # Windows — Edge
            os.path.expandvars(r"%ProgramFiles%\Microsoft\Edge\Application\msedge.exe"),
            os.path.expandvars(r"%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe"),
            os.path.expandvars(r"%LocalAppData%\Microsoft\Edge\Application\msedge.exe"),
            # Linux
            "/usr/bin/google-chrome",
            "/usr/bin/google-chrome-stable",
            "/usr/bin/chromium",
            "/usr/bin/chromium-browser",
            "/snap/bin/chromium",
        ]
        for p in candidates:
            if p and os.path.isfile(p):
                return p
        # PATH / where
        for name in ("chrome", "chrome.exe", "msedge", "msedge.exe", "google-chrome", "chromium"):
            from shutil import which
            w = which(name)
            if w and os.path.isfile(w):
                return w
        # Windows registry (App Paths)
        if os.name == "nt":
            try:
                import winreg
                for key_path, value_name in (
                    (r"SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe", None),
                    (r"SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe", None),
                    (r"SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\chrome.exe", None),
                    (r"SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe", None),
                ):
                    try:
                        with winreg.OpenKey(winreg.HKEY_LOCAL_MACHINE, key_path) as k:
                            res = winreg.QueryValueEx(k, value_name if value_name else "")
                            val = res[0] if isinstance(res, (tuple, list)) and res else res
                            if val and os.path.isfile(str(val)):
                                return str(val)
                    except OSError:
                        continue
                    except (ValueError, TypeError, IndexError):
                        continue

            except Exception:
                pass
        return ""

    browser_path = _find_browser()
    if browser_path:
        log(f"chrome: using {browser_path}")
        co.set_browser_path(browser_path)
    else:
        log("chrome: no known path — relying on DrissionPage auto-detect")

    co.set_argument("--no-sandbox")
    # Force English UI so button labels match automation (keep multi-lang selectors as fallback).
    co.set_argument("--lang=en-US")
    co.set_argument("--accept-lang=en-US,en")
    try:
        co.set_pref("intl.accept_languages", "en-US,en")
    except Exception:
        pass
    # Keep GPU on Windows so a real window paints; headless path still disables below.

    if os.name != "nt":
        co.set_argument("--disable-gpu")
    co.set_argument("--disable-dev-shm-usage")
    if os.name != "nt":
        co.set_argument("--disable-software-rasterizer")
        co.set_argument("--allow_root")
    # Suppress "Save password?" bubble / password manager UI
    co.set_argument("--disable-features=PasswordManagerOnboarding,PasswordLeakDetection")
    co.set_pref("credentials_enable_service", False)
    co.set_pref("profile.password_manager_enabled", False)
    co.set_pref("profile.password_manager_leak_detection", False)
    if os.path.isdir(ext_path):
        co.add_extension(ext_path)
    else:
        log(f"chrome: turnstile extension missing at {ext_path}")
    co.set_timeouts(base=10)

    if headless:
        log("__STEP__ xvfb")
        try:
            from pyvirtualdisplay import Display
            _vd = Display(visible=0, size=(1920, 1080))
            _vd.start()
        except Exception:
            # Windows has no Xvfb; use Chrome headless
            co.set_argument("--headless=new")
            co.set_argument("--disable-gpu")
    else:
        if not os.environ.get("DISPLAY") and os.name != "nt":
            log("__STEP__ xvfb")
            try:
                from pyvirtualdisplay import Display
                _vd = Display(visible=0, size=(1920, 1080))
                _vd.start()
            except Exception:
                pass
        # Force visible window on Windows (avoid accidental headless from env)
        if os.name == "nt":
            co.set_argument("--start-maximized")
            # Explicitly not headless
            try:
                co.headless(False)
            except Exception:
                pass

    log("__STEP__ launching")
    try:
        browser = Chromium(co)
    except Exception as e:
        import traceback

        fail(
            "launching",
            f"failed to start browser: {type(e).__name__}: {e}\n"
            f"browser_path={browser_path!r} address={getattr(co, 'address', None)!r}\n"
            f"{traceback.format_exc()}"
            "Install Google Chrome or Microsoft Edge.",
        )
    tab = browser.latest_tab
    if tab is None:
        try:
            tab = browser.new_tab(verification_url)
        except Exception as e:
            fail("launching", f"no browser tab: {e}")
    try:
        log(f"chrome: tab ready url={getattr(tab, 'url', '')}")
    except Exception:
        pass

    try:
        log("__STEP__ device")
        log(f"device: navigating {verification_url}")
        try:
            tab.get(verification_url)
        except Exception as e:
            log(f"device: tab.get error: {e}")
            try:
                tab = browser.new_tab(verification_url)
            except Exception as e2:
                fail("device", f"navigate failed: {e}; new_tab: {e2}")
        try:
            tab.wait.doc_loaded(timeout=30)
        except TypeError:
            try:
                tab.wait.doc_loaded()
            except Exception as e:
                log(f"device: doc_loaded: {e}")
        except Exception as e:
            log(f"device: doc_loaded: {e}")
        time.sleep(2)
        try:
            log(f"device: url={tab.url}")
        except Exception:
            pass

        dom = dump_dom(tab)
        log(f"DOM: {dom}")

        dismiss_cookies(tab)
        time.sleep(1)
        if dismiss_x_login_gate(tab):
            time.sleep(2)
            try:
                tab.wait.doc_loaded()
            except Exception:
                pass
            dismiss_cookies(tab)
            dom = dump_dom(tab)
            log(f"DOM_after_x_gate: {dom}")

        log("__STEP__ continue")
        continue_clicked = False
        for attempt in range(20):
            log(f"continue: attempt {attempt}")
            try:
                for label in ("Continue", "Continuar", "Next", "Avançar"):
                    btn = tab.ele(f"tag:button@@text()={label}", timeout=1)
                    if btn and getattr(btn, "states", None) and btn.states.is_displayed:
                        log(f"found {label}, clicking...")
                        btn.click()
                        time.sleep(2)
                        continue_clicked = True
                        break
                    if btn:
                        log(f"found {label} (click anyway)")
                        try:
                            btn.click()
                            time.sleep(2)
                            continue_clicked = True
                            break
                        except Exception:
                            pass
            except Exception as e:
                log(f"continue_ele_{attempt}: {e}")
            if not continue_clicked:
                try:
                    clicked = tab.run_js("""
const labels = ['continue', 'continuar', 'next', 'avançar', 'avancar'];
const btn = Array.from(document.querySelectorAll('button,[role=button],a')).find(b => {
  const t = (b.textContent||'').trim().toLowerCase();
  return labels.some(l => t === l || t.startsWith(l + ' '));
});
if (btn) { btn.click(); return true; }
return false;
""")
                    if clicked:
                        log("continue clicked via JS")
                        continue_clicked = True
                        time.sleep(2)
                except Exception as e:
                    log(f"continue_js_{attempt}: {e}")
            if continue_clicked:
                # If Continue led to X login wall, leave it before treating as done.
                dismiss_x_login_gate(tab)
                time.sleep(1)
                dom_after = dump_dom(tab)
                log(f"after_continue: {dom_after}")
                url_now = ""
                try:
                    url_now = tab.url or ""
                except Exception:
                    pass
                blob = (dom_after or "") + "\n" + url_now
                if "logged in to x" in blob.lower() or "continue with x" in blob.lower():
                    log("continue: still on X wall, retry")
                    continue_clicked = False
                    time.sleep(1)
                    continue
                if "sign-up" in blob or "Sign up" in blob or "sign-in" in url_now or "email" in blob.lower():
                    break
                if attempt >= 2:
                    break
            time.sleep(1)

        log("__STEP__ signup")
        dismiss_x_login_gate(tab)
        try:
            for label in ("Sign up", "Criar conta", "Register", "Create account"):
                link = tab.ele(f"tag:a@@text()={label}", timeout=2)
                if link:
                    log(f"signup link: {label}")
                    link.click()
                    time.sleep(2)
                    break
        except Exception as e:
            log(f"signup link: {e}")
        for attempt in range(15):
            dismiss_x_login_gate(tab)
            try:
                clicked = tab.run_js("""
const texts = [
  'sign up with email', 'cadastrar com e-mail', 'cadastrar com email',
  'create account', 'sign up', 'criar conta', 'register with email'
];
const bad = ['with x', 'twitter', 'x.com'];
const btn = Array.from(document.querySelectorAll('button,a,[role=button]')).find(b => {
  const t = (b.textContent||'').trim().toLowerCase();
  if (!t) return false;
  if (bad.some(x => t.includes(x))) return false;
  return texts.some(x => t.includes(x));
});
if (btn) { btn.click(); return (btn.textContent||'').trim().slice(0,60); }
return false;
""")
                if clicked:
                    log(f"signup_click: {clicked}")
                    break
            except Exception as e:
                log(f"signup_js_{attempt}: {e}")
            time.sleep(0.8)

        time.sleep(3)
        dom_email = dump_dom(tab)
        log(f"email_dom: {dom_email}")

        from email_provider import build_providers, create_inbox_with_fallback

        names = email_providers or None
        if not names:
            env_names = os.environ.get("EMAIL_PROVIDERS", "").strip()
            if env_names:
                names = [x.strip() for x in env_names.split(",") if x.strip()]
        if not names:
            names = ["mailtm", "duckmail"]

        d_url = duckmail_url or os.environ.get("DUCKMAIL_URL", "")
        d_key = duckmail_key or os.environ.get("DUCKMAIL_KEY", "")
        providers = build_providers(
            names=names,
            duckmail_url=d_url,
            duckmail_key=d_key,
        )
        inbox = create_inbox_with_fallback(providers)
        email_addr = inbox["address"]
        log(f"__STEP__ email {inbox.get('provider', '?')} {email_addr}")

        for attempt in range(10):
            try:
                filled = tab.run_js(f"""
const inp = document.querySelector('input[data-testid="email"], input[name="email"], input[type="email"]');
if (!inp) return 'no-input';
const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (nativeSetter) nativeSetter.call(inp, '{email_addr}');
else inp.value = '{email_addr}';
inp.dispatchEvent(new Event('input', {{ bubbles: true }}));
inp.dispatchEvent(new Event('change', {{ bubbles: true }}));
return 'ok';
""")
                if filled == 'ok':
                    log("email filled via JS")
                    break
            except Exception:
                pass
            time.sleep(0.5)
        time.sleep(1)

        log("clicking Sign up button")
        try:
            tab.run_js("""
const btn = Array.from(document.querySelectorAll('button')).find(b => b.textContent.trim() === 'Sign up');
if (btn) { btn.click(); return 'clicked'; }
return 'not-found';
""")
        except Exception:
            pass
        time.sleep(4)

        dom_otp = dump_dom(tab)
        log(f"otp_dom: {dom_otp}")

        log("__STEP__ otp")
        from email_provider import provider_for_inbox as pfi
        provider = pfi(providers, inbox)
        since_ms = int(time.time() * 1000)

        code = provider.fetch_code(inbox, since_ms=since_ms, timeout=120)
        if not code:
            fail("otp", "timeout waiting for OTP")

        log(f"code extracted: {code}")
        otp_raw = code.replace("-", "").replace(" ", "")[:6].upper()
        log(f"code stripped: {otp_raw}")

        for attempt in range(20):
            try:
                filled = tab.run_js(f"""
const inp = document.querySelector('input[name="code"], input[autocomplete="one-time-code"], input[data-input-otp="true"], input[type="text"]');
if (!inp) return 'no-input';
const nativeSetter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (nativeSetter) nativeSetter.call(inp, '{otp_raw}');
else inp.value = '{otp_raw}';
inp.dispatchEvent(new Event('input', {{ bubbles: true }}));
inp.dispatchEvent(new Event('change', {{ bubbles: true }}));
return 'ok';
""")
                if filled == 'ok':
                    log("code filled via JS")
                    break
            except Exception:
                pass
            time.sleep(0.5)
        submit_via_js(tab)
        time.sleep(2)

        log("__STEP__ profile")
        name = random_name()
        password = random_password()

        for attempt in range(5):
            try:
                res = tab.run_js(f"""
const given = document.querySelector('input[name="givenName"], input[autocomplete="given-name"], input[data-testid="givenName"]');
if (!given) return 'no-given';
const ns1 = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (ns1) ns1.call(given, '{name}'); else given.value = '{name}';
given.dispatchEvent(new Event('input', {{ bubbles: true }}));
given.dispatchEvent(new Event('change', {{ bubbles: true }}));
const family = document.querySelector('input[name="familyName"], input[autocomplete="family-name"], input[data-testid="familyName"]');
if (family) {{
const ns2 = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (ns2) ns2.call(family, '{name.split(' ')[0]}'); else family.value = '{name.split(' ')[0]}';
family.dispatchEvent(new Event('input', {{ bubbles: true }}));
family.dispatchEvent(new Event('change', {{ bubbles: true }}));
}}
const pwd = document.querySelector('input[name="password"], input[type="password"], input[data-testid="password"]');
if (!pwd) return 'no-pwd';
const ns3 = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (ns3) ns3.call(pwd, '{password}'); else pwd.value = '{password}';
pwd.dispatchEvent(new Event('input', {{ bubbles: true }}));
pwd.dispatchEvent(new Event('change', {{ bubbles: true }}));
return 'ok';
""")
                if res == 'ok':
                    log("profile filled via JS")
                    break
            except Exception:
                pass
            time.sleep(0.5)

        time.sleep(0.3)
        submit_via_js(tab)
        time.sleep(0.5)
        submit_via_js(tab)
        time.sleep(1)

        log("__STEP__ turnstile")
        time.sleep(5)

        # After signup, the page redirects to the device auth page.
        # Click Continue, then sign in with the new account, then Allow.
        log(f"__STEP__ authorize")
        time.sleep(3)
        after_url = tab.url
        log(f"post_signup_url: {after_url}")

        # Navigate to verification URL if needed
        if "/oauth2/device" not in tab.url:
            tab.get(verification_url)
            tab.wait.doc_loaded()
            time.sleep(3)

        log("__STEP__ continue_after_signup")
        for attempt in range(15):
            try:
                btn = tab.ele("tag:button@@text()=Continue", timeout=2)
                if btn and btn.states.is_displayed:
                    log("clicking Continue")
                    btn.click()
                    time.sleep(4)
                    break
            except Exception:
                pass
            time.sleep(1)

        dom_after = dump_dom(tab)
        log(f"after_continue_dom: {dom_after}")

        # If we're on the sign-in page, log in with the new account
        if "sign-in" in tab.url or "Login with email" in dom_after:
            log("__STEP__ login")
            # Click "Login with email"
            for attempt in range(10):
                try:
                    clicked = tab.run_js("""
var btn = Array.from(document.querySelectorAll('button')).find(b => b.textContent.trim().includes('Login with email'));
if (btn) { btn.click(); return true; }
return false;
""")
                    if clicked:
                        log("login_with_email clicked")
                        break
                except Exception:
                    pass
                time.sleep(0.5)
            time.sleep(2)

            # Fill email
            for attempt in range(10):
                try:
                    filled = tab.run_js(f"""
var inp = document.querySelector('input[type="email"], input[name="email"]');
if (!inp) return 'no-input';
var ns = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (ns) ns.call(inp, '{email_addr}'); else inp.value = '{email_addr}';
inp.dispatchEvent(new Event('input', {{ bubbles: true }}));
inp.dispatchEvent(new Event('change', {{ bubbles: true }}));
return 'ok';
""")
                    if filled == 'ok':
                        log("login email filled")
                        break
                except Exception:
                    pass
                time.sleep(0.5)
            time.sleep(1)

            # Click Continue / Next after email
            for attempt in range(10):
                try:
                    btn = tab.ele("tag:button@@text()=Continue", timeout=2)
                    if btn and btn.states.is_displayed:
                        btn.click()
                        log("continue after email clicked")
                        time.sleep(2)
                        break
                except Exception:
                    pass
                try:
                    clicked = tab.run_js("""
var btn = Array.from(document.querySelectorAll('button')).find(b => b.textContent.trim() === 'Continue');
if (btn) { btn.click(); return true; }
return false;
""")
                    if clicked:
                        log("continue after email clicked via JS")
                        time.sleep(2)
                        break
                except Exception:
                    pass
                time.sleep(0.5)
            time.sleep(1)

            # Fill password
            for attempt in range(10):
                try:
                    filled = tab.run_js(f"""
var inp = document.querySelector('input[type="password"], input[name="password"]');
if (!inp) return 'no-input';
var ns = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value')?.set;
if (ns) ns.call(inp, '{password}'); else inp.value = '{password}';
inp.dispatchEvent(new Event('input', {{ bubbles: true }}));
inp.dispatchEvent(new Event('change', {{ bubbles: true }}));
return 'ok';
""")
                    if filled == 'ok':
                        log("login password filled")
                        break
                except Exception:
                    pass
                time.sleep(0.5)
            time.sleep(1)

            # Submit login
            submit_via_js(tab)
            time.sleep(3)

            log(f"post_login_url: {tab.url}")
            dom_post_login = dump_dom(tab)
            log(f"post_login_dom: {dom_post_login}")

        # Now look for Allow button
        log("__STEP__ allow")
        time.sleep(3)
        dom_allow = dump_dom(tab)
        log(f"allow_page: {dom_allow}")
        allowed = False
        for _round in range(8):
            for text in ("Allow", "Permitir", "Autorizar"):
                try:
                    el = tab.ele(f"tag:button@@text()={text}", timeout=3)
                    if el and el.states.is_displayed:
                        log(f"allow_clicked: {text}")
                        el.click()
                        time.sleep(3)
                        allowed = True
                        break
                except Exception:
                    pass
            if allowed:
                break
            try:
                clicked = tab.run_js("""
const labels = ['allow', 'permitir', 'autorizar'];
const btn = Array.from(document.querySelectorAll('button,[role=button],a')).find(b => {
  const t = (b.textContent||'').trim().toLowerCase();
  return labels.some(l => t === l || t.startsWith(l + ' '));
});
if (btn) { btn.click(); return true; }
return false;
""")
                if clicked:
                    log("allow_clicked: js")
                    time.sleep(3)
                    allowed = True
                    break
            except Exception:
                pass
            time.sleep(1)

        if not allowed:
            fail("allow", "Allow button not found — device grant not authorized")

        time.sleep(2)
        log("__STEP__ done")
        entry = creds_store.save(email_addr, name, password, inbox.get("provider", ""))
        log(f"__CREDS__ {json.dumps(entry)}")
        log(f'__RESULT__ {json.dumps({"status": "success"})}')

    except Exception as e:
        import traceback

        fail("runtime", f"{type(e).__name__}: {e}\n{traceback.format_exc()}")

    finally:
        browser.quit()
        import shutil
        try:
            shutil.rmtree(_tmp_dir, ignore_errors=True)
        except Exception:
            pass


def main() -> None:
    parser = argparse.ArgumentParser(description="xAI signup via device login")
    parser.add_argument("--verification-url", required=True)
    parser.add_argument("--user-code")
    parser.add_argument("--headless", default="true")
    parser.add_argument("--creds-dir", default="")
    parser.add_argument("--email-providers", default="", help="comma list e.g. duckmail,mailtm")
    parser.add_argument("--duckmail-url", default="")
    parser.add_argument("--duckmail-key", default="")
    args = parser.parse_args()

    headless = args.headless.lower() not in ("false", "0", "no")
    providers = [x.strip() for x in args.email_providers.split(",") if x.strip()] or None

    run_signup(
        verification_url=args.verification_url,
        headless=headless,
        creds_dir=args.creds_dir,
        email_providers=providers,
        duckmail_url=args.duckmail_url,
        duckmail_key=args.duckmail_key,
    )


if __name__ == "__main__":
    # Ensure uncaught import/runtime errors still emit __RESULT__ for the Go runner.
    try:
        main()
    except SystemExit:
        raise
    except Exception as e:
        import traceback

        fail("startup", f"{type(e).__name__}: {e}\n{traceback.format_exc()}")

