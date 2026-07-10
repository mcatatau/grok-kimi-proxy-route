# Grok Proxy Plus

<p align="center">
  <strong>Desktop OpenAI-compatible proxy for Grok</strong><br/>
  Multi-account · streaming · thinking · tokens · local <code>/v1</code> API · SSO · auto-register
</p>

<p align="center">
  <a href="#features">Features</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#openai-compatible-proxy">OpenAI proxy</a> ·
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

**Grok Proxy Plus** is a **desktop app** (Wails + Go) that:

1. Logs you into **xAI / Grok** (device-code OAuth — **no Grok CLI required**)
2. Exposes a **local OpenAI-compatible API** (`http://127.0.0.1:8787/v1`)
3. Gives you a modern chat UI with **streaming**, **thinking**, **token/cost stats**, and **multi-account** support
4. Optionally **imports SSO tokens** and **auto-creates accounts** via a Python browser bot (`grok-signup-bot/`)

Use it with **Cursor, Open Code, Continue, Open WebUI**, or any client that speaks OpenAI Chat Completions / Responses (or Anthropic Messages).

> **Not affiliated with xAI.** Unofficial community project. Use at your own risk. See [DISCLAIMER.md](./DISCLAIMER.md) and [LICENSE](./LICENSE).

---

## Features

| Feature | Description |
|--------|-------------|
| **No Grok CLI** | Own OAuth device login + token refresh |
| **Multi-account** | Several xAI accounts; switch per request; sidebar cards |
| **Rate-limit failover** | Marks free-tier exhaustion (~24h), skips exhausted, same-request retry on proxy |
| **Streaming + thinking** | Real-time reasoning and answer stream |
| **Native search UI** | `web_search` / `x_search` events (research panel) |
| **Token & cost stats** | Usage, latency charts, estimated Grok 4.5 pricing |
| **Chat or Responses API** | Full history chat, or token-saving `last_response_id` chains |
| **Anthropic Messages** | `POST /v1/messages` (stream + tools) |
| **Local API proxy** | OpenAI: chat/completions, responses, models · Anthropic: messages · SSO import endpoint |
| **SSO import** | Paste token, file (`email:password:SSO` or raw), `AppData/sso-watch/*.txt`, `POST /v1/sso` |
| **Auto-register** | Device OAuth + DrissionPage bot + temp email (Mail.tm / DuckMail); batch + keep-alive loop |
| **Skills / MCP config** | Backend store + system-prompt catalog (no live MCP bridge yet; no Skills UI yet) |
| **Cross-platform build** | Windows + Linux via GitHub Actions |

---

## Quick start

### Option A — download a release

1. Open [Releases](../../releases)
2. Download:
   - **Windows:** `GrokProxyPlus-windows-amd64.exe`
   - **Linux:** `GrokProxyPlus-linux-amd64`
3. Run the app → **+ Adicionar conta** → authorize in the browser
4. Point your client at the local proxy (see below)

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
python3 -m venv .venv
.venv/bin/pip install -r grok-signup-bot/requirements.txt
# Optional: DuckMail env DUCKMAIL_URL / DUCKMAIL_KEY
```

The Go app resolves Python as `../../.venv/bin/python3` relative to the executable (works under `wails dev` / monorepo; **not** portable for release installs — see `plan/executed/auto-register-plan-v1.md`).

---

## OpenAI-compatible proxy

After the app starts (with an active account), a local server listens on:

```text
http://127.0.0.1:8787/v1
```

(If `8787` is busy, the app tries **`8788`**.)

| Setting | Value |
|---------|--------|
| **Base URL** | `http://127.0.0.1:8787/v1` |
| **API key** | any string (or the optional key set in the app) |
| **Model** | `grok-4.5` or `grok-4.5-responses` |

### Example — environment

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
export OPENAI_API_KEY=grok-desktop
export OPENAI_MODEL=grok-4.5
```

### Example — cURL

```bash
curl http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer grok-desktop" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.5",
    "stream": true,
    "reasoning_effort": "high",
    "messages": [{"role":"user","content":"Hello"}]
  }'
```

### Example — Open Code / openai-compatible provider

Copy from the in-app **Stats** modal (recommended), or:

```json
{
  "provider": {
    "grok-desktop": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Grok Proxy Plus",
      "options": {
        "baseURL": "http://127.0.0.1:8787/v1",
        "apiKey": "grok-desktop"
      },
      "models": {
        "grok-4.5": { "name": "Grok 4.5" },
        "grok-4.5-responses": { "name": "Grok 4.5 (Responses)" }
      }
    }
  }
}
```

### API modes

| Mode | Endpoint | Notes |
|------|----------|--------|
| **chat** | `/v1/chat/completions` | Classic OpenAI chat + `reasoning_content` stream |
| **responses** | `/v1/responses` | Multi-turn + native `web_search` / `x_search` (tools sanitized for OpenCode) |
| **messages** | `/v1/messages` | Anthropic Messages API (stream + tools); rate-limit retry; usage persistence still partial |
| **sso** | `POST /v1/sso` (also `/sso`) | Body: raw SSO / access token → import account |
| ~~completions~~ | `/v1/completions` | **Not supported** (legacy) |

On rate-limit (429/402 free-usage-exhausted) the proxy may mark the account exhausted, switch account, and **retry the same request** (buffered body). Header `X-Account-Status` reflects classification or `all-exhausted`.

---

## Multi-account & failover

- **+ Adicionar conta** → device-code login (xAI)
- Each account stored separately under AppData
- Switch from the **sidebar** or the **conta** chip in the composer
- The **active** account is used for UI chat **and** the local proxy
- **Exhausted** accounts show a badge; **Resetar** clears the flag; auto-recover after **24h**
- Proxy same-request failover: [plan/executed/account-exhaustion-plan.md](./plan/executed/account-exhaustion-plan.md)

Data directory (never committed to git):

| OS | Path |
|----|------|
| Windows | `%LOCALAPPDATA%\GrokDesktop\` |
| macOS | `~/Library/Application Support/GrokDesktop/` |
| Linux | `~/.local/share/GrokDesktop/` |

```text
GrokDesktop/
├── settings.json
├── usage.json
├── history.json
├── accounts/<id>.json
├── skills/
├── mcp_servers.json
├── sso-watch/*.txt
├── auto_creds.json
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

**Risks:** ToS, bans, automation. Prefer manual device login for personal use. Release binaries need a configured Python path (currently monorepo-relative).

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

Assets: `GrokProxyPlus-windows-amd64.exe`, `GrokProxyPlus-linux-amd64`

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
