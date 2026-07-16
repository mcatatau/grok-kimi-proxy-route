# grok-signup-bot

Browser automation + temporary email for **Grok Proxy Plus** auto-register.

Parent app: device OAuth (`StartDevice` / `PollDevice`) + this bot. The bot **never returns the OAuth token** — Go polls the device grant.

Plan: [`plan/executed/auto-register-plan-v1.md`](../plan/executed/auto-register-plan-v1.md).

## Status (implemented)

| Piece | Status |
|-------|--------|
| Email providers (Mail.tm, DuckMail) + fallback | ✅ |
| `grok_signup.py` (device URL → signup → OTP → profile → Allow) | ✅ **DrissionPage** (not Playwright) |
| `creds.py` (`__CREDS__`, `auto_creds.json`) | ✅ |
| Go bridge `internal/register/bot.go` | ✅ |
| Batch / auto-register loop in desktop app | ✅ |
| Turnstile extension `turnstilePatch/` | Present; plan marks Turnstile step as deferred if no iframe |

## Setup

```bash
cd /path/to/grok-proxy-plus

# Linux / macOS
python3 -m venv .venv
.venv/bin/pip install -r grok-signup-bot/requirements.txt

# Windows (PowerShell) — use real python.org install, not the Store stub
python -m venv .venv
.\.venv\Scripts\pip install -r grok-signup-bot\requirements.txt
```

Deps: `DrissionPage`, `curl_cffi`, optional `pyvirtualdisplay` (Linux), `playwright-captcha` (see `requirements.txt`).

**Browser:** Chrome or Edge must be installed (Windows paths auto-detected in `grok_signup.py`).

Desktop path resolution (`app.go` / settings):

| | Dev monorepo | Bare release .exe | Portable zip |
|--|--------------|-------------------|--------------|
| Python | `.venv` | `python` / `py` on PATH or settings | same |
| Bot dir | `<repo>/grok-signup-bot` | **embedded** → `%LOCALAPPDATA%\GrokDesktop\signup-bot\<ver>\` | sibling `grok-signup-bot\` |

Source of embed: `internal/register/bot/` (synced from this folder at build time).

On cancel/timeout the Go runner kills the **process tree** (Windows `taskkill /T`; Unix process group) so Chrome does not stay orphaned.

### Email providers

| Name | Env | Notes |
|------|-----|--------|
| `duckmail` | `DUCKMAIL_URL`, `DUCKMAIL_KEY` | Tried first when configured |
| `mailtm` | none | Public API fallback |

Fallback applies only to **create_inbox**. OTP always uses the provider that created the inbox.

Smoke:

```bash
cd grok-signup-bot
python3 email_mailtm.py
python3 -c "
from email_provider import build_providers, create_inbox_with_fallback
ps = build_providers(['mailtm'])
inbox = create_inbox_with_fallback(ps)
print(inbox['address'], inbox['provider'])
"
```

## CLI

```bash
python3 grok_signup.py \
  --verification-url 'https://accounts.x.ai/oauth2/device?user_code=XXXXXX' \
  [--headless false] \
  [--email-providers duckmail,mailtm] \
  [--duckmail-url URL] [--duckmail-key KEY]
```

Go settings (`email_providers`, `duckmail_*`) are passed as these flags by `internal/register`.

## Stdout protocol (for Go)

```
__STEP__ device
__STEP__ email mailtm
__STEP__ otp
__STEP__ profile
__STEP__ turnstile
__STEP__ allow
__STEP__ done
__CREDS__ {"email":"...","name":"...","password":"...","provider":"..."}
__RESULT__ {"status":"success"}
__RESULT__ {"status":"error","reason":"...","step":"..."}
```

## Risks

Automating xAI signup may violate ToS; rate limits and CAPTCHA can break selectors. Use only on systems you control; prefer manual device login when possible.
