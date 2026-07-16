# Grok Proxy Plus

<p align="center">
  <strong>Multi-route desktop proxy</strong><br/>
  <b>Grok (xAI)</b> + <b>Kimi Work</b> · multi-account · streaming · local <code>/v1</code> · SQLite
</p>

<p align="center">
  <a href="#features">Features</a> ·
  <a href="#providers-multi-route">Providers</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#openai-compatible-proxy">OpenAI proxy</a> ·
  <a href="#kimi-work">Kimi Work</a> ·
  <a href="#multi-account--failover">Multi-account</a> ·
  <a href="#auto-register--sso">Auto-register / SSO</a> ·
  <a href="#docs">Docs</a> ·
  <a href="#build-from-source">Build</a> ·
  <a href="#releases">Releases</a> ·
  <a href="#disclaimer">Disclaimer</a> ·
  <a href="#license">License</a>
</p>

---

## What is this?

**Grok Proxy Plus** is a **desktop app** (Wails + Go) that became a **multi-route provider hub**:

1. **Grok (xAI)** — device-code OAuth, multi-account pool, **`/v1/responses`**
2. **Kimi Work** — Google browser login (same flow as Kimi Desktop), mints `sk-kimi`, multi-account, **`/v1/chat/completions`**
3. One local OpenAI-compatible server (`http://127.0.0.1:8787/v1`) for Cursor / OpenCode / etc.
4. In-app chat UI (streaming, thinking, token/cost stats)
5. Optional Grok SSO import + auto-register bot

> **Not affiliated with xAI or Moonshot/Kimi.** Unofficial community project. Use at your own risk. See [DISCLAIMER.md](./DISCLAIMER.md) and [LICENSE](./LICENSE).

---

## Providers (multi-route)

| Provider | Auth mode | How you add accounts | HTTP API used by proxy | Models (examples) |
|----------|-----------|----------------------|------------------------|-------------------|
| **Grok (xAI)** | **Auth** (session pool) | Device OAuth / SSO / auto-register | **`POST /v1/responses` only** | `grok-4.5` |
| **Kimi Work** | **Auth** (session pool) | **Login with Google** (system browser) → mint `sk-kimi` | **`POST /v1/chat/completions` only** | `kimi-for-coding`, `k3-agent`, `k3-agent-swarm`, `k2d6-agent` |

**Important routing rules (v1.3+):**

- The **model selected in the desktop UI** only affects the **in-app chat**. It does **not** rewrite models for HTTP clients (OpenCode/Cursor/SDK).
- HTTP clients send whatever `model` they want; the proxy honors it (aliases like `default` map to the provider default).
- **Grok** rejects `/v1/chat/completions` with a clear error → use **Responses**.
- **Kimi** rejects `/v1/responses` → use **chat/completions** (agent-gw has no native Responses).
- Active **provider** in Global settings decides which account pool + upstream is used.

---

## Features

| Feature | Description |
|--------|-------------|
| **Multi-route providers** | Grok + Kimi Work in one app / one local port |
| **No Grok CLI** | Own OAuth device login + token refresh |
| **Kimi Work coding API** | Browser Google login → `CreateAPIKey(WORK)` → `sk-kimi` → `agent-gw.kimi.com/coding/v1` |
| **Multi-account (Auth)** | Separate pools per provider; sidebar + “Ver contas” modal |
| **SQLite persistence** | Accounts, settings, usage, history in `grokdesktop.db` (JSON migrated once) |
| **Rate-limit failover** | Marks free-tier exhaustion, skips exhausted, same-request retry (Grok) |
| **Kimi capacity errors** | “Too many people chatting…” → fail fast, **no** account rotate |
| **Streaming + thinking** | Real-time reasoning and answer stream |
| **Native search UI** | Grok `web_search` / `x_search` (research panel) |
| **Token & cost stats** | Usage charts; Grok 4.5 + Kimi K3/K2.6 list prices |
| **Local API proxy** | OpenAI chat/completions + responses (by provider) · models · Anthropic messages · SSO |
| **SSO import** | Paste token, file, `AppData/sso-watch/*.txt`, `POST /v1/sso` |
| **Auto-register** | Device OAuth + DrissionPage bot + temp email (Grok) |
| **Cross-platform build** | Windows + Linux via GitHub Actions |

---

## Quick start

### Option A — download a release

1. Open [Releases](../../releases)
2. Download:
   - **Windows:** `GrokProxyPlus-windows-amd64.exe` (bot scripts **embedded** — extract to AppData) **or** `…exe.zip` (portable: exe + `grok-signup-bot/`)
   - **Linux:** `GrokProxyPlus-linux-amd64` (embedded bot) **or** `…tar.gz` (binary + bot)
3. Run the app → **+ Adicionar conta** → authorize in the browser
4. Point your client at the local proxy (see below)

**WebView2 (Windows):** the desktop UI needs [Microsoft Edge WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/) (preinstalled on most Win10/11).

**Auto-register on a release build:**

| | Windows | Linux |
|--|---------|--------|
| Unpack | bare `.exe` OK (bot embedded) or zip with sibling `grok-signup-bot\` | bare binary OK or tar.gz |
| Python | Python 3 from python.org, **Add to PATH** (`python -c "import sys; print(sys.executable)"`) | `python3` + venv |
| Deps | **auto** `python -m venv` + pip under AppData on first register | same |
| Browser | Chrome or Edge | Chrome/Chromium |
| Paths | settings → **embedded extract** under AppData → sibling `grok-signup-bot` | same |

Without Python, **device login + SSO import** still work. SmartScreen may warn on unsigned builds — “More info → Run anyway”.

### Option B — run from source

**Requirements:** Go 1.23+, Node 20+, [Wails v2](https://wails.io/), (Linux: GTK/WebKit dev packages)

```bash
git clone https://github.com/Maicon501a/grok-proxy-plus.git
cd grok-proxy-plus

# install Wails once
go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0

# dev
wails dev

# production build
wails build
```

Binary output: `build/bin/` (e.g. `GrokDesktop.exe` on Windows).

**Optional auto-register (dev tree):**

```bash
# Linux / macOS
python3 -m venv .venv
.venv/bin/pip install -r grok-signup-bot/requirements.txt

# Windows (PowerShell)
python -m venv .venv
.\.venv\Scripts\pip install -r grok-signup-bot\requirements.txt
```

Path resolution (bot): settings `bot_dir` → **embedded** extract under `%LOCALAPPDATA%\GrokDesktop\signup-bot\<ver>\` → monorepo / next to exe. Python: settings `python_path` → monorepo `.venv` → `python`/`python3` on PATH.

---

## OpenAI-compatible proxy

After the app starts (with an active account for the **selected provider**), a local server listens on:

```text
http://127.0.0.1:8787/v1
```

(If `8787` is busy, the app tries **`8788`**.)

| Setting | Value |
|---------|--------|
| **Base URL** | `http://127.0.0.1:8787/v1` |
| **API key** | any string (or the optional key set in the app) |
| **Active provider** | Global → Provider (`xai` or `kimi_work`) |

### Grok (provider = xAI)

| Setting | Value |
|---------|--------|
| Endpoint | **`POST /v1/responses`** (chat/completions returns 400) |
| Model | `grok-4.5` |

```bash
curl http://127.0.0.1:8787/v1/responses \
  -H "Authorization: Bearer grok-desktop" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "stream": true,
    "input": "Hello"
  }'
```

### Kimi Work (provider = kimi_work)

| Setting | Value |
|---------|--------|
| Endpoint | **`POST /v1/chat/completions`** (responses returns 400) |
| Models | `kimi-for-coding` (wire id), aliases `k3-agent`, `k3-agent-swarm`, `k2d6-agent` |
| Tools | Native OpenAI `tools` / `tool_calls` |

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer kimi-work" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "kimi-for-coding",
    "stream": false,
    "messages": [{"role":"user","content":"Hello"}]
  }'
```

### Example — Open Code (both providers, same baseURL)

Switch **Global → Provider** in the app, then use matching models:

```json
{
  "provider": {
    "grok-proxy-plus": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Grok Proxy Plus",
      "options": {
        "baseURL": "http://127.0.0.1:8787/v1",
        "apiKey": "local"
      },
      "models": {
        "grok-4.5": { "name": "Grok 4.5 (Responses)" },
        "kimi-for-coding": { "name": "Kimi For Coding" },
        "k3-agent": { "name": "K3 Max (Work)" },
        "k3-agent-swarm": { "name": "K3 Swarm Max (Work)" }
      }
    }
  }
}
```

### API surface

| Endpoint | Grok | Kimi Work | Notes |
|----------|------|-----------|--------|
| `/v1/models` | ✓ | ✓ | Catalog for **active** provider |
| `/v1/responses` | ✓ | ✗ | Grok only |
| `/v1/chat/completions` | ✗ | ✓ | Kimi only (OpenAI tools native) |
| `/v1/messages` | ✓* | — | Anthropic-shaped (Grok path) |
| `/v1/search` | ✓ | — | Native xAI search helper |
| `POST /v1/sso` | ✓ | — | Import Grok SSO |

\*Best-effort; prefer Responses for Grok clients when possible.

On Grok rate-limit (429/402 free-usage-exhausted) the proxy may mark the account exhausted, switch account, and **retry the same request**.  
On Kimi “Too many people are chatting…” the proxy returns **503 `kimi_server_busy`** and does **not** rotate accounts.

---

## Kimi Work

Kimi Work is Moonshot’s **coding/agent** product (Desktop “Work” mode), not the consumer web chat JWT path.

### Auth flow (same idea as official Desktop)

```text
System browser → Google OAuth (loopback 127.0.0.1:61120+)
  → POST https://www.kimi.com/api/auth/login/google  { code: <google id_token> }
  → access_token + refresh_token
  → Connect CreateAPIKey(scope=WORK) → sk-kimi-…
  → Upstream: https://agent-gw.kimi.com/coding/v1
```

In the app: **Provider → Kimi Work · Auth** → **+ Conta Kimi** → **Login com Google**.

- Multi-account = multiple Kimi users (each Google login → one `sk-kimi` pool entry).
- Does **not** require the Kimi Desktop app installed.
- Wire model id from agent-gw is usually **`kimi-for-coding`** (K3-class coding SKU). Desktop labels `k3-agent` / swarm are aliases.

### Pricing (estimate, platform list)

Used by in-app stats (USD / 1M tokens):

| Model family | Cache hit | Input (miss) | Output |
|--------------|----------:|-------------:|-------:|
| Kimi K3 / `kimi-for-coding` | $0.30 | $3.00 | $15.00 |
| Kimi K2.6 / `k2d6-agent` | $0.16 | $0.95 | $4.00 |

Source: [platform.kimi.ai pricing](https://platform.kimi.ai/docs/pricing/chat-k3). Membership/subscription billing on consumer accounts may differ.

---

## Multi-account & failover

### Grok (xAI)

- **+ Conta Grok** → device-code login / SSO / auto-register
- Exhausted accounts: badge + skip + optional auto-register
- Failover plan: [plan/executed/account-exhaustion-plan.md](./plan/executed/account-exhaustion-plan.md)

### Kimi Work

- **+ Conta Kimi** → Google browser login only (primary path)
- Pool is per-user `sk-kimi`; capacity errors are server-side (no rotate)

### Shared UX

- Switch **provider** in Global settings
- **Ver contas** modal lists only the **active provider** pool
- Active account is used for UI chat **and** the local proxy for that provider

Data directory (never committed to git):

| OS | Path |
|----|------|
| Windows | `%LOCALAPPDATA%\GrokDesktop\` |
| macOS | `~/Library/Application Support/GrokDesktop/` |
| Linux | `~/.local/share/GrokDesktop/` |

```text
GrokDesktop/
├── grokdesktop.db        # SQLite: accounts, settings, usage, history
├── settings.json         # legacy dual-write
├── usage.json / history.json
├── accounts/<id>.json    # legacy dual-write backup
├── signup-bot/<ver>/
├── python-venv/
├── skills/
├── mcp_servers.json
├── sso-watch/*.txt
└── logs/
```

---

## Auto-register & SSO

### SSO

| Method | How |
|--------|-----|
| UI paste | Importar SSO |
| File | `email:password:SSO` or raw token lines |
| Watch dir | `AppData/sso-watch/*.txt` every 30s |
| HTTP | `POST /v1/sso` (gated by proxy API key if set) |

### Auto-register (optional, experimental)

Flow: **Device OAuth** → Python bot (`grok-signup-bot/`, **DrissionPage**) → **PollDevice** → account saved.

| UI / backend | Behavior |
|--------------|----------|
| **+ Gerar contas** | Batch 1–5 (cap 5 active) |
| `autoRegisterLoop` | Every 5 min if active &lt; min (**opt-in**: `auto_register_enabled` in settings, default **false**) |
| Email | DuckMail → Mail.tm |
| Plan | [plan/executed/auto-register-plan-v1.md](./plan/executed/auto-register-plan-v1.md) |

**Risks:** ToS, bans, automation. Prefer manual device login for personal use.  
Bare release binaries **embed** the Python bot and extract it under AppData; on first auto-register the app creates **`python-venv`** and `pip install`s deps. You still need **host Python 3** (with venv/pip) and **Chrome/Edge**.

---


## Docs

| Doc | Conteúdo |
|-----|----------|
| [plan/executed/](./plan/executed/) | Planos **concluídos** (exhaustion, auto-register, FINDINGS) |
| [plan/executed/hardening-plan-v1.md](./plan/executed/hardening-plan-v1.md) | Hardening A–D (feito) |
| [plan/executed/account-exhaustion-plan.md](./plan/executed/account-exhaustion-plan.md) | Failover + usage (feito) |
| [plan/executed/auto-register-plan-v1.md](./plan/executed/auto-register-plan-v1.md) | Auto-register (feito) |
| [docs/grok-register-analysis.md](./docs/grok-register-analysis.md) | Guia SSO + análise grok-register |
| [grok-signup-bot/README.md](./grok-signup-bot/README.md) | Setup do bot Python |

## Build from source

```bash
wails build
wails build -platform windows/amd64
wails build -platform linux/amd64
```

### Linux dependencies (Debian/Ubuntu)

```bash
sudo apt-get install -y \
  libgtk-3-dev libwebkit2gtk-4.1-dev \
  libayatana-appindicator3-dev librsvg2-dev \
  gcc pkg-config
```

### Self-test (no GUI)

```bash
go run ./cmd/selftest
```

---

## Releases

| Trigger | Workflow |
|---------|----------|
| Push / PR to `main` | [CI](./.github/workflows/ci.yml) |
| Push tag `v*.*.*` | [Release](./.github/workflows/release.yml) |

```bash
git tag v1.0.0
git push origin v1.0.0
```

Assets: `GrokProxyPlus-windows-amd64.exe`, `GrokProxyPlus-windows-amd64.exe.zip`, `GrokProxyPlus-linux-amd64`, `GrokProxyPlus-linux-amd64.tar.gz`

---

## Project layout

```text
.
├── main.go / app.go
├── internal/
│   ├── oauth/
│   ├── store/
│   ├── upstream/
│   ├── proxyhttp/
│   ├── pricing/
│   ├── register/
│   ├── skills/
│   └── mcpconfig/
├── grok-signup-bot/
├── frontend/
├── cmd/selftest/
├── docs/
├── plan/executed/          # planos concluídos
├── .github/workflows/
├── LICENSE
├── DISCLAIMER.md
└── README.md
```

---

## UI language

The desktop UI strings are **Portuguese (pt-BR)** by design. README and API docs stay English. Full i18n is not planned unless requested.

## Security notes

- **Tokens never go into the git repo** — only AppData on your machine  
- OAuth `client_id` in source is the **public** xAI CLI client (PKCE, no client secret)  
- Do not commit `accounts/`, `*.env`, or dirty release binaries  
- Treat the local proxy as **localhost-only**  
- Empty proxy API key leaves `/v1/sso` open on localhost  
- Auto-register stores plaintext passwords in `auto_creds.json`  

---

## Disclaimer

**Use at your own risk.** Authors are **not responsible** for bans, billing, data loss, ToS violations, or any damages.  
This is **not** an official xAI product. Full text: [DISCLAIMER.md](./DISCLAIMER.md).

---

## License

**MIT (Non-Commercial)** — free for personal / non-commercial use.  
**No commercial use** without written permission.  
Full terms: [LICENSE](./LICENSE).
