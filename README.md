# Grok Proxy Plus

<p align="center">
  <strong>Desktop OpenAI-compatible proxy for Grok</strong><br/>
  Multi-account · streaming · thinking · tokens · local <code>/v1</code> API
</p>

<p align="center">
  <a href="#features">Features</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#openai-compatible-proxy">OpenAI proxy</a> ·
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

Use it with **Cursor, Open Code, Continue, Open WebUI**, or any client that speaks OpenAI Chat Completions / Responses.

> **Not affiliated with xAI.** Unofficial community project. Use at your own risk. See [DISCLAIMER.md](./DISCLAIMER.md) and [LICENSE](./LICENSE).

---

## Features

| Feature | Description |
|--------|-------------|
| **No Grok CLI** | Own OAuth login + token refresh |
| **Multi-account** | Add several xAI accounts, switch per request |
| **Streaming + thinking** | Real-time reasoning and answer stream |
| **Token & cost stats** | Usage, latency charts, estimated Grok 4.5 pricing |
| **Chat or Responses API** | Full history chat, or token-saving `last_response_id` chains |
| **Local API proxy** | OpenAI: `POST /v1/chat/completions`, `POST /v1/responses`, `GET /v1/models` · Anthropic: `POST /v1/messages` |
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
| **messages** | `/v1/messages` | Anthropic Messages API (stream + tools) |
| ~~completions~~ | `/v1/completions` | **Not supported** (legacy) |

---

## Multi-account

- **+ Adicionar conta** → device-code login (xAI)
- Each account stored separately under AppData
- Switch from the **sidebar** or the **conta** chip in the composer
- The **active** account is used for UI chat **and** the local proxy

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
└── logs/
```

---

## Build from source

```bash
# Windows / macOS / Linux
wails build

# Target a platform explicitly
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

Checks AppData layout, models list, and a live chat (requires a logged-in account on that machine).

---

## Releases

GitHub Actions builds **Windows** and **Linux** automatically:

| Trigger | Workflow |
|---------|----------|
| Push / PR to `main` | [CI](./.github/workflows/ci.yml) — compile + upload artifacts |
| Push tag `v*.*.*` | [Release](./.github/workflows/release.yml) — create GitHub Release with binaries |

### Cut a release

```bash
git tag v1.0.0
git push origin v1.0.0
```

Assets:

- `GrokProxyPlus-windows-amd64.exe`
- `GrokProxyPlus-linux-amd64`

---

## Project layout

```text
.
├── main.go / app.go           # Wails app + bindings
├── internal/
│   ├── oauth/                 # device login + refresh
│   ├── store/                 # multi-account AppData
│   ├── upstream/              # cli-chat-proxy client (stream)
│   ├── proxyhttp/             # local OpenAI HTTP server
│   └── pricing/               # token cost estimates
├── frontend/                  # UI (vanilla + Vite)
├── cmd/selftest/              # integration smoke test
├── .github/workflows/         # CI + release
├── LICENSE
├── DISCLAIMER.md
└── README.md
```

---

## Security notes

- **Tokens never go into the git repo** — only AppData on your machine  
- OAuth `client_id` in source is the **public** xAI CLI client (PKCE, no client secret)  
- Do not commit `accounts/`, `*.env`, or release binaries from a dirty local machine  
- Treat the local proxy as **localhost-only** unless you know what you are doing  

---

## Disclaimer

**Use at your own risk.** Authors are **not responsible** for bans, billing, data loss, ToS violations, or any damages.  
This is **not** an official xAI product. Full text: [DISCLAIMER.md](./DISCLAIMER.md).

---

## License

**MIT (Non-Commercial)** — free for personal / non-commercial use.  
**No commercial use** without written permission.  
Full terms: [LICENSE](./LICENSE).
