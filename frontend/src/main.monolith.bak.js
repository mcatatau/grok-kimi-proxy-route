import "./style.css";
import "./app.css";

import { marked } from "marked";
import DOMPurify from "dompurify";

import {
  GetBootstrap,
  ImportSSO,
  ImportSSOFromFile,
  ListModels,
  SetActiveAccount,
  RemoveAccount,
  RenameAccount,
  ResetAccount,
  RecoverAccounts,
  StartDevice,
  StartDeviceLogin,
  CancelDeviceLogin,
  CreateAccount,
  CreateAccounts,
  OpenExternal,
  UpdateSettings,
  SendChat,
  CancelChat,
  GetStats,
} from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";

// Markdown like ChatGPT: GFM, breaks for soft newlines
marked.setOptions({
  gfm: true,
  breaks: true,
  pedantic: false,
});

const state = {
  batchCreating: false,
  settings: {},
  accounts: [],
  models: [],
  usage: {},
  activeRequest: null,
  proxyBase: "",
  dataDir: "",
  messages: [],
  streaming: false,
  lastResponseId: null,
  device: null,
  shellBuilt: false,
  sessionCost: 0,
  sessionLat: null,
  // custom dropdowns
  picks: {
    effort: "high",
    api: "chat",
    model: "grok-4.5",
    cEffort: "high",
    cApi: "chat",
    cModel: "grok-4.5",
  },
  menus: {},
};

/** Custom dark menu — replaces native <select> (white OS list on Windows). */
function mountMenu(root, { id, options, value, prefix, onChange, chip }) {
  root.className = "dd" + (chip ? " seg dd-chip" : "");
  root.dataset.menuId = id;
  const optList = () =>
    typeof options === "function" ? options() : options;

  const render = () => {
    const opts = optList();
    const cur = opts.find((o) => o.value === root._value) || opts[0];
    if (cur && root._value !== cur.value) root._value = cur.value;
    const label = cur?.label || root._value || "—";
    root.innerHTML = `
      <button type="button" class="dd-trigger" aria-haspopup="listbox">
        <span class="dd-value">${prefix ? `<span class="dd-label">${escapeHtml(prefix)}</span> ` : ""}${escapeHtml(label)}</span>
        <span class="dd-chev"></span>
      </button>
      <div class="dd-menu" role="listbox"></div>
    `;
    const menu = root.querySelector(".dd-menu");
    opts.forEach((o) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = "dd-item" + (o.value === root._value ? " active" : "");
      item.role = "option";
      item.innerHTML = `<span>${escapeHtml(o.label)}</span><span class="check">✓</span>`;
      item.onclick = (e) => {
        e.stopPropagation();
        root._value = o.value;
        root.classList.remove("open");
        render();
        onChange?.(o.value);
      };
      menu.appendChild(item);
    });
    root.querySelector(".dd-trigger").onclick = (e) => {
      e.stopPropagation();
      const was = root.classList.contains("open");
      closeAllMenus();
      if (!was) {
        // open upward if near bottom
        const rect = root.getBoundingClientRect();
        const spaceBelow = window.innerHeight - rect.bottom;
        menu.classList.toggle("drop-up", spaceBelow < 220);
        root.classList.add("open");
      }
    };
  };

  root._value = value;
  root.getValue = () => root._value;
  root.setValue = (v) => {
    root._value = v;
    // For account menu, display email if we can resolve it
    render();
  };
  root.setOptions = (next) => {
    if (typeof options !== "function") options = next;
    render();
  };
  root.refresh = render;
  // Account chip: show email and list all accounts to switch active
  if (id === "c-account") {
    root.refresh = () => {
      const opts = optList();
      const cur = opts.find((o) => o.value === root._value) || opts[0];
      const acc = state.accounts.find((a) => a.id === root._value);
      const display =
        acc?.email || acc?.label || cur?.label || "escolher conta";
      root.innerHTML = `
        <button type="button" class="dd-trigger" title="Clique para alternar a conta da request">
          <span class="dd-value"><span class="dd-label">conta</span> ${escapeHtml(display)}</span>
          <span class="dd-chev"></span>
        </button>
        <div class="dd-menu" role="listbox"></div>
      `;
      const menu = root.querySelector(".dd-menu");
      opts.forEach((o) => {
        const a = state.accounts.find((x) => x.id === o.value);
        const item = document.createElement("button");
        item.type = "button";
        item.className = "dd-item" + (o.value === root._value ? " active" : "");
        item.setAttribute("role", "option");
        const title = a?.email || o.label;
        const sub = a?.label && a.label !== a.email ? a.label : a?.active ? "em uso agora" : "clique para usar";
        item.innerHTML = `<span style="min-width:0"><span style="display:block;overflow:hidden;text-overflow:ellipsis">${escapeHtml(title)}</span><span style="display:block;font-size:10.5px;color:rgba(255,255,255,0.35);margin-top:2px">${escapeHtml(sub)}</span></span><span class="check">✓</span>`;
        item.onclick = (e) => {
          e.stopPropagation();
          root._value = o.value;
          root.classList.remove("open");
          root.refresh();
          onChange?.(o.value);
        };
        menu.appendChild(item);
      });
      root.querySelector(".dd-trigger").onclick = (e) => {
        e.stopPropagation();
        const was = root.classList.contains("open");
        closeAllMenus();
        if (!was) {
          const rect = root.getBoundingClientRect();
          const spaceBelow = window.innerHeight - rect.bottom;
          menu.classList.toggle("drop-up", spaceBelow < 220);
          root.classList.add("open");
        }
      };
    };
    root.setValue = (v) => {
      root._value = v;
      root.refresh();
    };
  }
  root.refresh();
  state.menus[id] = root;
  return root;
}

function closeAllMenus() {
  document.querySelectorAll(".dd.open").forEach((el) => el.classList.remove("open"));
}

document.addEventListener("click", () => closeAllMenus());
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeAllMenus();
});

function $(sel, root = document) {
  return root.querySelector(sel);
}

function fmt(n) {
  if (n == null) return "0";
  return Number(n).toLocaleString("en-US");
}

function fmtUSD(n) {
  const v = Number(n) || 0;
  if (v > 0 && v < 0.0001) return "<$0.0001";
  return "$" + v.toFixed(v >= 1 ? 2 : 4);
}

function fmtMs(n) {
  if (n == null || n <= 0) return "—";
  if (n < 1000) return Math.round(n) + " ms";
  return (n / 1000).toFixed(2) + " s";
}

function shortPath(p) {
  if (!p) return "";
  const s = String(p);
  return s.length <= 36 ? s : "…" + s.slice(-34);
}

function initials(s) {
  if (!s) return "?";
  const p = String(s).split(/[\s@._-]+/).filter(Boolean);
  return ((p[0]?.[0] || "?") + (p[1]?.[0] || "")).toUpperCase();
}

function escapeHtml(s) {
  return String(s ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

/** Render markdown safely for chat bubbles (assistant + optional user). */
function renderMarkdown(text) {
  const raw = String(text ?? "");
  if (!raw.trim()) return "";
  try {
    const html = marked.parse(raw, { async: false });
    return DOMPurify.sanitize(html, {
      USE_PROFILES: { html: true },
      ADD_ATTR: ["target", "rel"],
    });
  } catch {
    return `<p>${escapeHtml(raw)}</p>`;
  }
}

function enhanceMarkdownRoot(root) {
  if (!root) return;
  // External links open safely
  root.querySelectorAll("a[href]").forEach((a) => {
    a.setAttribute("target", "_blank");
    a.setAttribute("rel", "noopener noreferrer");
  });
  // Copy button on code blocks
  root.querySelectorAll("pre").forEach((pre) => {
    if (pre.querySelector(".code-copy")) return;
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "code-copy";
    btn.textContent = "Copiar";
    btn.onclick = async (e) => {
      e.stopPropagation();
      const code = pre.querySelector("code")?.innerText || pre.innerText;
      try {
        await navigator.clipboard.writeText(code);
        btn.textContent = "Copiado";
        setTimeout(() => (btn.textContent = "Copiar"), 1200);
      } catch (_) {}
    };
    pre.appendChild(btn);
  });
}

function globalUsage() {
  return (
    state.usage?._global || {
      prompt_tokens: 0,
      completion_tokens: 0,
      reasoning_tokens: 0,
      total_tokens: 0,
      requests: 0,
    }
  );
}

function activeAccount() {
  return state.accounts.find((a) => a.active) || state.accounts[0] || null;
}

function ensureShell() {
  if (state.shellBuilt) return;
  const app = $("#app");
  app.innerHTML = `
    <div class="shell">
      <aside class="rail">
        <div class="brand">
          <div class="logo"></div>
          <div>
            <h1>Grok</h1>
            <span>Desktop proxy</span>
          </div>
        </div>

        <div>
          <div class="accounts-head">
            <div class="rail-label">Contas</div>
            <span class="accounts-count" id="accounts-count">0</span>
          </div>
          <div class="accounts" id="accounts"></div>
          <div class="rail-actions" style="margin-top:10px">
            <button class="btn btn-solid" id="btn-add">+ Adicionar conta</button>
            <button class="btn btn-quiet" id="btn-batch-create" style="margin-top:4px">+ Gerar contas</button>
            <button class="btn btn-quiet" id="btn-import-sso" style="margin-top:6px">Importar SSO</button>
            <button class="btn btn-quiet" id="btn-import-file" style="margin-top:4px">Importar de arquivo</button>
          </div>
        </div>

        <div class="rail-block">
          <div class="rail-label">Uso</div>
          <div class="stats">
            <div class="stat"><label>Total</label><b id="u-total">0</b></div>
            <div class="stat"><label>Custo</label><b id="u-cost">$0</b></div>
            <div class="stat"><label>Prompt</label><b id="u-prompt">0</b></div>
            <div class="stat"><label>Out</label><b id="u-comp">0</b></div>
            <div class="stat"><label>Think</label><b id="u-reason">0</b></div>
            <div class="stat"><label>Lat. méd</label><b id="u-lat">—</b></div>
          </div>
          <div class="rail-actions" style="margin-top:10px">
            <button class="btn btn-quiet" id="btn-stats">Estatísticas</button>
          </div>
        </div>

        <div class="rail-block">
          <div class="rail-label">Global</div>
          <div class="field">
            <span class="field-label">Raciocínio</span>
            <div id="set-effort"></div>
          </div>
          <div class="field">
            <span class="field-label">API</span>
            <div id="set-api"></div>
          </div>
          <div class="field">
            <span class="field-label">Modelo</span>
            <div id="set-model"></div>
          </div>
        </div>

        <div class="rail-foot">
          <b>Proxy</b>
          <span id="proxy-url">—</span><br /><br />
          <b>AppData</b>
          <span id="data-dir">—</span>
        </div>
      </aside>

      <main class="stage">
        <header class="top">
          <div class="status" id="status">
            <span class="dot-live"></span>
            <span id="status-text">Pronto</span>
          </div>
          <div class="token-live">
            <span>in <b id="sess-in">0</b></span>
            <span>out <b id="sess-out">0</b></span>
            <span>think <b id="sess-think">0</b></span>
            <span class="cost" id="sess-cost">$0</span>
            <span id="sess-lat" style="display:none"></span>
            <button class="icon-btn" id="btn-stats-top" type="button">Stats</button>
          </div>
        </header>

        <div class="stream" id="stream">
          <div class="stream-inner" id="stream-inner"></div>
        </div>

        <div class="dock">
          <div class="composer">
            <textarea id="prompt" rows="1" placeholder="Pergunte qualquer coisa…"></textarea>
            <div class="composer-row">
              <div class="tools">
                <div id="c-account"></div>
                <div id="c-model"></div>
                <div id="c-effort"></div>
                <div id="c-api"></div>
                <span class="tool-hint" title="Pesquisa nativa xAI (web + X) via Responses">search: xAI</span>
              </div>
              <button class="send" id="send" title="Enviar">↑</button>
            </div>
          </div>
        </div>
      </main>
    </div>
  `;

  $("#btn-add").onclick = startLogin;
  $("#btn-batch-create").onclick = showBatchCreateModal;
  $("#btn-import-sso").onclick = importSSO;
  $("#btn-import-file").onclick = importSSOFile;
  $("#btn-stats").onclick = openStatsModal;
  $("#btn-stats-top").onclick = openStatsModal;

  const effortOpts = [
    { value: "low", label: "Low" },
    { value: "medium", label: "Medium" },
    { value: "high", label: "High" },
  ];
  const apiOpts = [
    { value: "chat", label: "Chat" },
    { value: "responses", label: "Responses" },
  ];
  const modelOpts = () =>
    (state.models.length
      ? state.models
      : [
          { id: "grok-4.5", name: "Grok 4.5" },
          { id: "grok-4.5-responses", name: "Grok 4.5 (Responses)" },
        ]
    ).map((m) => ({ value: m.id, label: m.name || m.id }));

  mountMenu($("#set-effort"), {
    id: "set-effort",
    options: effortOpts,
    value: state.picks.effort,
    onChange: (v) => {
      state.picks.effort = v;
      state.picks.cEffort = v;
      state.menus["c-effort"]?.setValue(v);
      saveGlobal({ reasoning_effort: v });
    },
  });
  mountMenu($("#set-api"), {
    id: "set-api",
    options: apiOpts,
    value: state.picks.api,
    onChange: (v) => {
      state.picks.api = v;
      state.picks.cApi = v;
      state.menus["c-api"]?.setValue(v);
      saveGlobal({ api_mode: v });
    },
  });
  mountMenu($("#set-model"), {
    id: "set-model",
    options: modelOpts,
    value: state.picks.model,
    onChange: (v) => {
      state.picks.model = v;
      state.picks.cModel = v;
      state.menus["c-model"]?.setValue(v);
      saveGlobal({ default_model: v });
    },
  });

  // Composer account switcher: click email chip → pick another account
  const accountOpts = () => {
    if (!state.accounts.length) {
      return [{ value: "", label: "sem conta — adicione à esquerda" }];
    }
    return state.accounts.map((a) => ({
      value: a.id,
      // show email first (what people recognize), label as fallback
      label: a.active
        ? `● ${a.email || a.label || a.id}`
        : a.email || a.label || a.id,
    }));
  };

  mountMenu($("#c-account"), {
    id: "c-account",
    options: accountOpts,
    value: activeAccount()?.id || "",
    prefix: "conta",
    chip: true,
    onChange: async (v) => {
      if (!v) return;
      if (v === activeAccount()?.id) return;
      await SetActiveAccount(v);
      await refreshBootstrap(false);
    },
  });
  mountMenu($("#c-model"), {
    id: "c-model",
    options: modelOpts,
    value: state.picks.cModel,
    prefix: "model",
    chip: true,
    onChange: (v) => {
      state.picks.cModel = v;
    },
  });
  mountMenu($("#c-effort"), {
    id: "c-effort",
    options: effortOpts.map((o) => ({ ...o, label: o.value })),
    value: state.picks.cEffort,
    prefix: "think",
    chip: true,
    onChange: (v) => {
      state.picks.cEffort = v;
    },
  });
  mountMenu($("#c-api"), {
    id: "c-api",
    options: apiOpts.map((o) => ({ ...o, label: o.value })),
    value: state.picks.cApi,
    prefix: "api",
    chip: true,
    onChange: (v) => {
      state.picks.cApi = v;
    },
  });

  const prompt = $("#prompt");
  prompt.addEventListener("input", () => autoGrow(prompt));
  prompt.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      if (!state.streaming) submit();
    }
  });
  $("#send").onclick = () => {
    if (state.streaming) CancelChat();
    else submit();
  };

  state.shellBuilt = true;
}

function autoGrow(ta) {
  ta.style.height = "auto";
  ta.style.height = Math.min(160, ta.scrollHeight) + "px";
}

function fillModels() {
  // custom menus re-render options via refresh
  state.menus["set-model"]?.refresh?.();
  state.menus["c-model"]?.refresh?.();
  const prefer = state.settings.default_model || state.picks.model || "grok-4.5";
  if (state.menus["set-model"]) state.menus["set-model"].setValue(prefer);
  if (state.menus["c-model"] && !state.picks.cModelTouched) {
    state.menus["c-model"].setValue(prefer);
    state.picks.cModel = prefer;
  }
}

function paintChrome() {
  ensureShell();
  const u = globalUsage();
  const acc = activeAccount();
  const busy = !!state.activeRequest;

  $("#u-total").textContent = fmt(u.total_tokens);
  $("#u-cost").textContent = fmtUSD(u.cost_usd);
  $("#u-prompt").textContent = fmt(u.prompt_tokens);
  $("#u-comp").textContent = fmt(u.completion_tokens);
  $("#u-reason").textContent = fmt(u.reasoning_tokens);
  if (u.latency_samples > 0) {
    $("#u-lat").textContent = fmtMs(u.latency_sum_ms / u.latency_samples);
  }
  $("#proxy-url").textContent = state.proxyBase || "—";
  $("#data-dir").textContent = shortPath(state.dataDir) || "—";
  $("#data-dir").title = state.dataDir || "";

  const list = $("#accounts");
  const countEl = $("#accounts-count");
  if (countEl) {
    const n = state.accounts.length;
    countEl.textContent = n === 1 ? "1 conta" : `${n} contas`;
  }
  list.innerHTML = "";
  if (!state.accounts.length) {
    list.innerHTML = `<div class="account empty-hint">Nenhuma conta ainda.<br/>Clique em <b>+ Adicionar conta</b> para logar na xAI.<br/>Você pode adicionar várias e trocar qual envia a request.</div>`;
  } else {
    state.accounts.forEach((a) => {
      const u = a.usage || {};
      const card = document.createElement("div");
      card.className = "account" + (a.active ? " active" : "");
      card.innerHTML = `
        <div class="account-top" data-act="select">
          <div class="avatar">${escapeHtml(initials(a.email || a.label))}</div>
          <div style="min-width:0">
            <strong title="${escapeHtml(a.email || a.id)}">${escapeHtml(a.label || a.email || a.id)}</strong>
            <div class="meta-line">
              ${a.active ? `<span class="badge badge-live">ativa</span>` : `<span class="badge badge-ok">salva</span>`}
              ${a.expired ? `<span class="badge badge-warn" title="Token OAuth expirado — faça login de novo">login expirado</span>` : ""}
              ${a.exhausted ? `<span class="badge badge-warn" title="Cota/rate limit do free tier (~24h). Use Resetar se já liberou, ou outra conta.">cota esgotada</span>` : ""}
              <span>${escapeHtml((a.email || "").split("@")[0] || a.id.slice(0, 8))}</span>
            </div>
          </div>
        </div>
        <div class="account-usage">
          <span><b>${fmt(u.total_tokens || 0)}</b> tok</span>
          <span><b>${fmtUSD(u.cost_usd || 0)}</b></span>
          <span><b>${fmt(u.requests || 0)}</b> req</span>
        </div>
        <div class="account-actions">
          ${
            a.active
              ? `<button type="button" class="primary" data-act="noop" disabled style="opacity:.55">Em uso</button>`
              : `<button type="button" class="primary" data-act="select">Usar</button>`
          }
          ${a.exhausted ? `<button type="button" data-act="reset">Resetar</button>` : ""}
          <button type="button" data-act="rename">Renomear</button>
          <button type="button" class="danger" data-act="remove">Remover</button>
        </div>
      `;
      card.querySelectorAll("[data-act]").forEach((btn) => {
        btn.onclick = async (e) => {
          e.stopPropagation();
          const act = btn.getAttribute("data-act");
          if (act === "select") {
            await SetActiveAccount(a.id);
            await refreshBootstrap(false);
          } else if (act === "reset") {
            await ResetAccount(a.id);
            await RecoverAccounts();
            await refreshBootstrap(false);
          } else if (act === "rename") {
            const next = prompt("Nome da conta", a.label || a.email || "");
            if (next == null || !String(next).trim()) return;
            try {
              await RenameAccount(a.id, String(next).trim());
              await refreshBootstrap(false);
            } catch (err) {
              alert("Rename: " + err);
            }
          } else if (act === "remove") {
            if (!confirm(`Remover conta ${a.label || a.email}?`)) return;
            await RemoveAccount(a.id);
            await refreshBootstrap(false);
          }
        };
      });
      list.appendChild(card);
    });
  }

  // refresh composer account menu
  state.menus["c-account"]?.refresh?.();
  const activeId = activeAccount()?.id || "";
  if (activeId) state.menus["c-account"]?.setValue(activeId);

  // sync pick values from settings
  state.picks.effort = state.settings.reasoning_effort || state.picks.effort || "high";
  state.picks.api = state.settings.api_mode || state.picks.api || "chat";
  state.picks.model = state.settings.default_model || state.picks.model || "grok-4.5";
  if (!state.picks.cEffort) state.picks.cEffort = state.picks.effort;
  if (!state.picks.cApi) state.picks.cApi = state.picks.api;
  if (!state.picks.cModel) state.picks.cModel = state.picks.model;

  state.menus["set-effort"]?.setValue(state.picks.effort);
  state.menus["set-api"]?.setValue(state.picks.api);
  fillModels();
  state.menus["c-effort"]?.setValue(state.picks.cEffort);
  state.menus["c-api"]?.setValue(state.picks.cApi);
  state.menus["c-model"]?.setValue(state.picks.cModel);
  if (acc?.id) state.menus["c-account"]?.setValue(acc.id);

  paintStatus();
  paintSend();
  paintMessages();
}

function paintStatus() {
  const el = $("#status");
  const text = $("#status-text");
  if (!el || !text) return;
  const acc = activeAccount();
  const req = state.activeRequest;
  if (req) {
    el.classList.add("live");
    const phase =
      req.phase === "searching"
        ? "pesquisando na web"
        : req.phase === "thinking"
          ? "thinking"
          : req.phase || "…";
    text.innerHTML = `Request → <strong>${escapeHtml(req.label || req.email || "conta")}</strong> · ${escapeHtml(phase)}`;
  } else {
    el.classList.remove("live");
    const n = state.accounts.length;
    text.innerHTML = acc
      ? `Ativa: <strong>${escapeHtml(acc.label || acc.email || acc.id)}</strong>${n > 1 ? ` · ${n} contas` : ""}`
      : "Nenhuma conta — adicione à esquerda (multi-conta ok)";
  }
}

function paintSend() {
  const btn = $("#send");
  if (!btn) return;
  btn.classList.toggle("stop", state.streaming);
  btn.textContent = state.streaming ? "■" : "↑";
  btn.title = state.streaming ? "Parar" : "Enviar";
}

function paintMessages() {
  const inner = $("#stream-inner");
  if (!inner) return;

  if (!state.messages.length) {
    inner.innerHTML = `
      <div class="hero">
        <div class="orb"></div>
        <h2>O que você quer saber?</h2>
        <p>Conversa contínua, thinking em cinza translúcido, multi-conta e proxy OpenAI local.</p>
      </div>
    `;
    return;
  }

  // Rebuild stream with markdown for assistant (and light md for user)
  const html = state.messages
    .map((m, i) => {
      if (m.role === "user") {
        // User: preserve plain text layout but allow simple md if they paste it
        const body = m.content?.includes("`") || m.content?.includes("**") || m.content?.includes("\n")
          ? renderMarkdown(m.content)
          : `<p>${escapeHtml(m.content).replaceAll("\n", "<br>")}</p>`;
        return `
          <section class="turn turn-user" data-i="${i}">
            <div class="turn-label">Você</div>
            <div class="turn-body md">${body}</div>
          </section>
        `;
      }
      const searchUI = renderSearchBlock(m);
      const think = m.thinking
        ? `<div class="think">${escapeHtml(m.thinking)}</div>`
        : "";
      const cursor = m.streaming ? `<span class="cursor" aria-hidden="true"></span>` : "";
      const meta = m.meta ? `<div class="turn-meta">${escapeHtml(m.meta)}</div>` : "";
      const answer = renderMarkdown(m.content || "") + cursor;
      const hasAnswer = !!(m.content && m.content.trim());
      const searching =
        m.search?.status === "searching" ||
        (m.searches || []).some((s) => s.status === "searching") ||
        (m.tools || []).some((t) => t.status === "running");
      const showAnswer = hasAnswer || (m.streaming && !searching);
      return `
        <section class="turn turn-assistant" data-i="${i}">
          <div class="turn-label">Grok</div>
          ${searchUI}
          ${think}
          ${hasAnswer || showAnswer ? `<div class="answer md">${answer || (m.streaming ? cursor : "")}</div>` : m.streaming && searchUI ? "" : `<div class="answer md">${answer}</div>`}
          ${meta}
        </section>
      `;
    })
    .join("");

  const stream = $("#stream");
  const nearBottom = stream.scrollHeight - stream.scrollTop - stream.clientHeight < 120;
  inner.innerHTML = html;
  enhanceMarkdownRoot(inner);
  if (nearBottom || state.streaming) {
    stream.scrollTop = stream.scrollHeight;
  }
}

async function saveGlobal(patch) {
  state.settings = await UpdateSettings(patch);
  if (patch.reasoning_effort) {
    state.picks.effort = patch.reasoning_effort;
    state.picks.cEffort = patch.reasoning_effort;
    state.menus["c-effort"]?.setValue(patch.reasoning_effort);
  }
  if (patch.api_mode) {
    state.picks.api = patch.api_mode;
    state.picks.cApi = patch.api_mode;
    state.menus["c-api"]?.setValue(patch.api_mode);
  }
  if (patch.default_model) {
    state.picks.model = patch.default_model;
    state.picks.cModel = patch.default_model;
    state.menus["c-model"]?.setValue(patch.default_model);
  }
}

async function importSSO() {
  const token = prompt("Cole o SSO token do grok-register:", "");
  if (!token || !String(token).trim()) return;
  try {
    const result = await ImportSSO(String(token).trim());
    await refreshBootstrap(true);
  } catch (e) {
    alert("Falha ao importar SSO: " + e);
  }
}

async function importSSOFile() {
  const path = prompt("Caminho do arquivo de SSO (ex: /caminho/sso.txt):", "");
  if (!path || !String(path).trim()) return;
  try {
    const result = await ImportSSOFromFile(String(path).trim());
    await refreshBootstrap(true);
    alert(`Importado: ${result.imported} tokens de ${result.file}`);
  } catch (e) {
    alert("Falha ao importar arquivo: " + e);
  }
}

async function startLogin() {
  try {
    const st = await StartDeviceLogin();
    state.device = st;
    showDeviceModal(st);
    if (st.verification_url) {
      try {
        await OpenExternal(st.verification_url);
      } catch (_) {}
    }
  } catch (e) {
    alert("Falha ao iniciar login: " + e);
  }
}

function showDeviceModal(st) {
  document.querySelector(".overlay")?.remove();
  const overlay = document.createElement("div");
  overlay.className = "overlay";
  overlay.innerHTML = `
    <div class="sheet">
      <h3>Adicionar conta</h3>
      <p>Confirme o código na página da xAI. O app completa sozinho.</p>
      <div class="code">${escapeHtml(st.user_code)}</div>
      <div class="sheet-actions">
        <button class="btn btn-solid" id="m-open">Abrir login</button>
        <button class="btn btn-quiet" id="m-copy">Copiar código</button>
        <button class="btn btn-quiet" id="m-cancel">Cancelar</button>
      </div>
      <div class="hint">${escapeHtml(st.verification_url || "")}</div>
    </div>
  `;
  document.body.appendChild(overlay);
  $("#m-open", overlay).onclick = () => OpenExternal(st.verification_url);
  $("#m-copy", overlay).onclick = async () => {
    await navigator.clipboard.writeText(st.user_code);
  };
  $("#m-cancel", overlay).onclick = () => {
    CancelDeviceLogin();
    state.device = null;
    overlay.remove();
  };
}

function showBatchCreateModal() {
  document.querySelector(".overlay")?.remove();
  const activeN = state.accounts.filter((a) => a && !a.exhausted && !a.expired).length;
  const overlay = document.createElement("div");
  overlay.className = "overlay";
  overlay.innerHTML = `
    <div class="sheet">
      <h3>Gerar contas</h3>
      <p>Quantas contas <b>novas</b> criar neste lote? (teto = criação simultânea/lote, <b>sem</b> limite no pool)<br/>
      <span style="font-size:12px;color:var(--text-dim)">No pool agora: ${state.accounts.length} · ~ativas: ${activeN} · cada criação leva alguns minutos</span></p>
      <input type="number" id="bc-count" value="2" min="1" max="5" style="width:80px;box-sizing:border-box" />
      <div class="sheet-actions" style="margin-top:12px">
        <button class="btn btn-solid" id="bc-start">Gerar</button>
        <button class="btn btn-quiet" id="bc-cancel">Cancelar</button>
      </div>
      <div id="bc-progress" style="margin-top:10px;font-size:13px;color:var(--text-dim);max-height:300px;overflow-y:auto"></div>
    </div>
  `;
  document.body.appendChild(overlay);

  $("#bc-start", overlay).onclick = async () => {
    let n = parseInt($("#bc-count", overlay)?.value || "1", 10);
    if (!Number.isFinite(n) || n < 1) n = 1;
    if (n > 5) n = 5;
    const btn = $("#bc-start", overlay);
    const prog = $("#bc-progress", overlay);
    btn.disabled = true;
    btn.textContent = "Gerando...";
    state.batchCreating = true;
    prog.innerHTML = `<div style="color:#888">Aguarde, criando ${n} conta(s)… (pode levar vários minutos)</div>`;
    try {
      const results = await CreateAccounts(n);
      const list = Array.isArray(results) ? results : [];
      const okN = list.filter((r) => r.status === "success").length;
      prog.innerHTML = list.map((r, i) => {
        const status = r.status === "success" ? "✅" : "❌";
        const email = r.creds?.email || r.creds?.account_id || "";
        const creds = email ? ` <span style="font-size:11px;color:var(--text-dim)">${escapeHtml(email)}</span>` : "";
        const reason = r.reason ? ` — ${escapeHtml(String(r.reason))}` : "";
        return `<div>${status} Tentativa ${r.attempt || i + 1}: ${escapeHtml(r.status || "?")}${reason}${creds}</div>`;
      }).join("") || `<div style="color:red">Nenhum resultado (pediu ${n})</div>`;
      prog.innerHTML += `<div style="margin-top:8px;color:var(--text-dim)">Pedido: ${n} · Criadas: ${okN}/${list.length} · Ativas na UI: ${state.accounts.length}</div>`;
      if (okN < n) {
        prog.innerHTML += `<div style="margin-top:6px;color:#fbbf24">Nem todas foram criadas — veja o motivo em cada ❌ (bot/OTP/poll). O app tenta todas as ${n} vezes.</div>`;
      }
      await paintChrome();
      if (list.some(r => r.status === "success")) {
        setTimeout(async () => {
          if (document.body.contains(overlay)) overlay.remove();
          state.batchCreating = false;
          await paintChrome();
        }, 4000);
      } else {
        state.batchCreating = false;
        btn.disabled = false;
        btn.textContent = "Gerar";
      }
    } catch (e) {
      state.batchCreating = false;
      prog.innerHTML = `<div style="color:red">Erro: ${escapeHtml(String(e))}</div>`;
      btn.disabled = false;
      btn.textContent = "Gerar";
    }
  };
  $("#bc-cancel", overlay).onclick = () => { state.batchCreating = false; overlay.remove(); };
}

async function submit() {
  const promptEl = $("#prompt");
  const text = (promptEl?.value || "").trim();
  if (!text || state.streaming) return;
  if (!activeAccount()) {
    alert("Adicione e selecione uma conta primeiro.");
    return;
  }

  const model =
    state.menus["c-model"]?.getValue?.() || state.picks.cModel || state.settings.default_model;
  const effort =
    state.menus["c-effort"]?.getValue?.() || state.picks.cEffort || state.settings.reasoning_effort;
  const apiMode =
    state.menus["c-api"]?.getValue?.() || state.picks.cApi || state.settings.api_mode;

  state.messages.push({ role: "user", content: text });
  state.messages.push({
    role: "assistant",
    content: "",
    thinking: "",
    streaming: true,
    tools: [],
    searches: [],
    search: null,
    citations: [],
  });
  promptEl.value = "";
  autoGrow(promptEl);
  state.streaming = true;
  thinkChars = 0;
  state.sessionCost = 0;
  state.sessionLat = null;
  $("#sess-in").textContent = "0";
  $("#sess-out").textContent = "0";
  $("#sess-think").textContent = "0";
  $("#sess-cost").textContent = "$0";
  const latEl = $("#sess-lat");
  if (latEl) {
    latEl.style.display = "none";
    latEl.textContent = "";
  }
  {
    const stEl = $("#sess-think");
    if (stEl) delete stEl.dataset.final;
  }
  paintSend();
  paintStatus();
  paintMessages();

  const payload = {
    model,
    messages: [{ role: "user", content: text }],
    stream: true,
    reasoning_effort: effort,
    api_mode: apiMode,
  };

  // full history for chat mode continuity in UI only; for API send context
  if (apiMode === "chat") {
    payload.messages = state.messages
      .filter((m) => m.role === "user" || (m.role === "assistant" && m.content && !m.streaming))
      .map((m) => ({ role: m.role, content: m.content }));
    // last user already included; drop incomplete assistant
    if (payload.messages.at(-1)?.role === "assistant") payload.messages.pop();
  } else if (state.lastResponseId) {
    payload.last_response_id = state.lastResponseId;
    payload.messages = [{ role: "user", content: text }];
  } else {
    // first responses turn — can send conversation so far as messages
    payload.messages = state.messages
      .filter((m) => m.role === "user" || (m.role === "assistant" && m.content && !m.streaming))
      .map((m) => ({ role: m.role, content: m.content }));
    if (payload.messages.at(-1)?.role === "assistant") payload.messages.pop();
  }

  try {
    await SendChat(payload);
  } catch (e) {
    state.streaming = false;
    const last = state.messages.at(-1);
    if (last?.role === "assistant") {
      last.content = "Erro: " + e;
      last.streaming = false;
    }
    paintSend();
    paintMessages();
  }
}

let thinkChars = 0;
let paintScheduled = false;

function schedulePaintMessages() {
  if (paintScheduled) return;
  paintScheduled = true;
  requestAnimationFrame(() => {
    paintScheduled = false;
    paintMessages();
  });
}

function domainFromUrl(u) {
  try {
    return new URL(u).hostname.replace(/^www\./, "");
  } catch {
    return "";
  }
}

function kindLabel(kind) {
  if (kind === "x") return "X";
  return "Web";
}

function ensureSearch(last, id, kind) {
  if (!last.searches) last.searches = [];
  let s = last.searches.find((x) => x.id === id);
  if (!s) {
    s = {
      id,
      kind: kind || "web",
      query: "",
      results: [],
      status: "searching",
      provider: "xAI",
    };
    last.searches.push(s);
  }
  // keep legacy single search pointer for paint/compat
  last.search = s;
  return s;
}

function ensureTool(last, id, name) {
  if (!last.tools) last.tools = [];
  let t = last.tools.find((x) => x.id === id);
  if (!t) {
    t = { id, name: name || "web_search", status: "running", query: "" };
    last.tools.push(t);
  }
  return t;
}

function faviconUrl(domainOrUrl) {
  const d = domainOrUrl.includes(".") && !domainOrUrl.includes("://")
    ? domainOrUrl
    : domainFromUrl(domainOrUrl) || domainOrUrl;
  if (!d) return "";
  return `https://www.google.com/s2/favicons?domain=${encodeURIComponent(d)}&sz=64`;
}

function renderFavStack(results, max = 5) {
  const list = (results || []).slice(0, max);
  if (!list.length) {
    return `
      <div class="ms-favs ghost">
        <span class="ms-fav shimmer"></span>
        <span class="ms-fav shimmer"></span>
        <span class="ms-fav shimmer"></span>
      </div>
    `;
  }
  const rest = Math.max(0, (results || []).length - max);
  return `
    <div class="ms-favs">
      ${list
        .map((r, i) => {
          const domain = r.domain || domainFromUrl(r.url) || "?";
          const src = faviconUrl(domain);
          return `
            <span class="ms-fav" style="z-index:${20 - i};animation-delay:${i * 40}ms" title="${escapeHtml(domain)}">
              ${
                src
                  ? `<img src="${escapeHtml(src)}" alt="" loading="lazy" onerror="this.parentElement.classList.add('fallback');this.remove()"/>`
                  : ""
              }
              <span class="ms-fav-letter">${escapeHtml((domain[0] || "?").toUpperCase())}</span>
            </span>
          `;
        })
        .join("")}
      ${rest > 0 ? `<span class="ms-fav more">+${rest}</span>` : ""}
    </div>
  `;
}

function renderSourceCards(results) {
  const list = (results || []).slice(0, 10);
  if (!list.length) return "";
  return `
    <div class="ms-sources">
      ${list
        .map((r, idx) => {
          const domain = r.domain || domainFromUrl(r.url) || "source";
          const title = r.title || domain;
          const fav = faviconUrl(domain);
          return `
            <a class="ms-card" href="${escapeHtml(r.url || "#")}" target="_blank" rel="noopener noreferrer" style="animation-delay:${idx * 35}ms">
              <div class="ms-card-icon">
                ${
                  fav
                    ? `<img src="${escapeHtml(fav)}" alt="" loading="lazy" onerror="this.style.display='none';this.nextElementSibling.style.display='grid'"/>`
                    : ""
                }
                <span class="ms-card-letter" style="${fav ? "display:none" : ""}">${escapeHtml((domain[0] || "?").toUpperCase())}</span>
              </div>
              <div class="ms-card-body">
                <div class="ms-card-title">${escapeHtml(title)}</div>
                <div class="ms-card-domain">${escapeHtml(domain)}</div>
              </div>
              <span class="ms-card-arrow">↗</span>
            </a>
          `;
        })
        .join("")}
    </div>
  `;
}

function renderSearchBlock(m) {
  const items = m.searches?.length
    ? m.searches
    : m.search
      ? [m.search]
      : [];
  const toolsRunning = (m.tools || []).some((t) => t.status === "running");
  if (!items.length && !toolsRunning) return "";

  // Aggregate for Manus-style single research panel
  const anyLive =
    toolsRunning ||
    items.some((s) => s.status === "searching" || s.status === "running");
  const anyErr = items.some((s) => s.status === "error");
  const allResults = [];
  const queries = [];
  const kinds = new Set();
  for (const s of items) {
    if (s.query) queries.push(s.query);
    if (s.kind) kinds.add(s.kind);
    for (const r of s.results || []) {
      if (!allResults.some((x) => x.url === r.url)) allResults.push(r);
    }
  }
  const primaryQ = queries[0] || "";
  const kindTxt =
    kinds.has("web") && kinds.has("x")
      ? "Web · X"
      : kinds.has("x")
        ? "X"
        : "Web";

  let statusLine = "Researching the web";
  let statusClass = "live";
  if (anyErr && !anyLive) {
    statusLine = "Search failed";
    statusClass = "err";
  } else if (!anyLive && allResults.length) {
    statusLine = `Found ${allResults.length} source${allResults.length === 1 ? "" : "s"}`;
    statusClass = "done";
  } else if (!anyLive && items.length) {
    statusLine = "Research complete";
    statusClass = "done";
  }

  const steps = items
    .map((s) => {
      const st = s.status || "searching";
      const live = st === "searching" || st === "running";
      const icon = live ? "◌" : st === "error" ? "!" : "✓";
      return `
        <div class="ms-step ${st}">
          <span class="ms-step-ico">${icon}</span>
          <div class="ms-step-main">
            <span class="ms-step-kind">${escapeHtml(kindLabel(s.kind || "web"))}</span>
            <span class="ms-step-q">${escapeHtml(s.query || (live ? "Looking up…" : "—"))}</span>
          </div>
          ${live ? `<span class="ms-step-spin"></span>` : ""}
        </div>
      `;
    })
    .join("");

  return `
    <div class="ms-panel ${statusClass}">
      <div class="ms-head">
        <div class="ms-head-left">
          <div class="ms-orb ${anyLive ? "spin" : ""}" aria-hidden="true">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none">
              <circle cx="12" cy="12" r="9" stroke="currentColor" stroke-width="1.5" opacity="0.35"/>
              <path d="M3 12h18M12 3c2.5 2.8 3.8 5.8 3.8 9s-1.3 6.2-3.8 9c-2.5-2.8-3.8-5.8-3.8-9S9.5 5.8 12 3z" stroke="currentColor" stroke-width="1.5"/>
            </svg>
          </div>
          <div class="ms-head-text">
            <div class="ms-status">${escapeHtml(statusLine)}</div>
            <div class="ms-sub">
              <span class="ms-badge">${escapeHtml(kindTxt)}</span>
              ${primaryQ ? `<span class="ms-query">“${escapeHtml(primaryQ)}”</span>` : ""}
              ${items.length > 1 ? `<span class="ms-meta">${items.length} queries</span>` : ""}
            </div>
          </div>
        </div>
        ${renderFavStack(allResults)}
      </div>
      ${
        anyLive && !allResults.length
          ? `
        <div class="ms-loading">
          <div class="ms-bar"><i></i></div>
          <div class="ms-skeleton">
            <div class="ms-sk"></div>
            <div class="ms-sk"></div>
            <div class="ms-sk short"></div>
          </div>
        </div>
      `
          : ""
      }
      ${items.length > 1 || anyLive ? `<div class="ms-steps">${steps || ""}</div>` : ""}
      ${renderSourceCards(allResults)}
      ${
        anyErr
          ? `<div class="ms-error">${escapeHtml(items.find((s) => s.error)?.error || "Search error")}</div>`
          : ""
      }
    </div>
  `;
}

function onSearchEvent(type, payload) {
  const last = state.messages.at(-1);
  if (!last || last.role !== "assistant") return;

  const kind =
    payload?.kind === "x" || String(payload?.name || "").startsWith("x_")
      ? "x"
      : "web";
  const id = payload?.tool_call_id || payload?.id || "search";

  if (type === "search:start") {
    const s = ensureSearch(last, id, kind);
    s.status = "searching";
    s.query = payload?.query || s.query || "";
    s.kind = payload?.kind || kind;
    s.provider = "xAI";
    const t = ensureTool(last, id, kind === "x" ? "x_search" : "web_search");
    t.status = "running";
    t.query = s.query;
  } else if (type === "search:results") {
    const s = ensureSearch(last, id, payload?.kind || kind);
    s.query = payload?.query || s.query || "";
    s.results = (payload?.results || []).map((r) => ({
      ...r,
      domain: r.domain || domainFromUrl(r.url),
    }));
    s.duration_ms = payload?.duration_ms;
    s.status = "done";
    s.kind = payload?.kind || s.kind || kind;
    s.provider = "xAI";
    const t = ensureTool(last, id, s.kind === "x" ? "x_search" : "web_search");
    t.status = "done";
    t.query = s.query;
  } else if (type === "search:error") {
    const s = ensureSearch(last, id, kind);
    s.status = "error";
    s.error = payload?.error || "erro";
    const t = ensureTool(last, id, "web_search");
    t.status = "error";
  } else if (type === "search:done") {
    const s = last.searches?.find((x) => x.id === id) || last.search;
    if (s && s.status === "searching") s.status = "done";
  } else if (type === "tool:call") {
    const name = payload?.name || "web_search";
    const k = name.includes("x_") || name === "x_search" ? "x" : "web";
    ensureTool(last, id, name);
    ensureSearch(last, id, k).status = "searching";
  } else if (type === "tool:done") {
    const t = ensureTool(last, id, payload?.name || "web_search");
    t.status = "done";
    const s = last.searches?.find((x) => x.id === id);
    if (s && s.status === "searching") s.status = "done";
  }
  schedulePaintMessages();
}

function onChatEventTool(ev) {
  const last = state.messages.at(-1);
  if (!last || last.role !== "assistant") return false;

  if (ev.type === "tool_call") {
    onSearchEvent("tool:call", {
      id: ev.id,
      name: ev.text,
      kind: ev.payload?.kind,
    });
    return true;
  }
  if (ev.type === "search_query") {
    onSearchEvent("search:start", {
      query: ev.text,
      tool_call_id: ev.id,
      provider: "xAI",
      kind: ev.payload?.kind,
    });
    return true;
  }
  if (ev.type === "search_results") {
    onSearchEvent("search:results", {
      ...(ev.payload || {}),
      query: ev.text || ev.payload?.query,
      tool_call_id: ev.id,
      provider: "xAI",
    });
    return true;
  }
  if (ev.type === "tool_done") {
    onSearchEvent("tool:done", { id: ev.id, name: ev.text });
    return true;
  }
  if (ev.type === "tool_error") {
    onSearchEvent("search:error", { error: ev.error, tool_call_id: ev.id });
    return true;
  }
  if (ev.type === "citation") {
    if (!last.citations) last.citations = [];
    const url = ev.payload?.url || ev.text;
    if (url && !last.citations.some((c) => c.url === url)) {
      last.citations.push({ url, title: ev.payload?.title || "" });
    }
    return true;
  }
  return false;
}

function onChatEvent(ev) {
  const last = state.messages.at(-1);
  if (!last || last.role !== "assistant") return;

  if (onChatEventTool(ev)) return;

  if (ev.type === "thinking") {
    last.thinking = (last.thinking || "") + (ev.text || "");
    thinkChars += (ev.text || "").length;
    const approx = Math.max(0, Math.round(thinkChars / 4));
    const el = $("#sess-think");
    if (el && !el.dataset.final) el.textContent = fmt(approx) + "~";
  } else if (ev.type === "content") {
    last.content = (last.content || "") + (ev.text || "");
  } else if (ev.type === "usage" && ev.usage) {
    const u = ev.usage;
    const est = ev.estimated ? " · est." : "";
    last.meta = `${u.prompt_tokens || 0} in · ${u.completion_tokens || 0} out · ${u.reasoning_tokens || 0} think · ${fmtMs(ev.latency_ms)}${est}`;
    $("#sess-in").textContent = fmt(u.prompt_tokens);
    $("#sess-out").textContent = fmt(u.completion_tokens);
    $("#sess-think").textContent = fmt(u.reasoning_tokens);
    $("#sess-think").dataset.final = "1";
    // cost approx client-side until stats event (Grok 4.5 rates)
    const cost =
      ((u.prompt_tokens || 0) * 2 +
        (u.completion_tokens || 0) * 6 +
        (u.reasoning_tokens || 0) * 6) /
      1e6;
    state.sessionCost = cost;
    $("#sess-cost").textContent = fmtUSD(cost);
    if (ev.latency_ms) {
      const latEl = $("#sess-lat");
      if (latEl) {
        latEl.style.display = "";
        latEl.textContent = fmtMs(ev.latency_ms);
      }
    }
  } else if (ev.type === "done") {
    last.streaming = false;
    state.streaming = false;
    if (ev.id) state.lastResponseId = ev.id;
    if (ev.model) last.meta = (last.meta ? last.meta + " · " : "") + ev.model;
    if (ev.latency_ms && last.meta && !last.meta.includes("ms") && !last.meta.includes(" s")) {
      last.meta += " · " + fmtMs(ev.latency_ms);
    }
    paintSend();
    paintStatus();
  } else if (ev.type === "error") {
    last.content = (last.content || "") + (last.content ? "\n" : "") + ev.error;
    last.streaming = false;
    state.streaming = false;
    paintSend();
    paintStatus();
  }

  schedulePaintMessages();
}

async function refreshBootstrap(full = true) {
  const b = await GetBootstrap();
  state.settings = b.settings || {};
  state.accounts = b.accounts || [];
  state.usage = b.usage || {};
  state.proxyBase = b.proxy_base || "";
  state.dataDir = b.data_dir || "";
  state.activeRequest = b.active_request || null;
  if (full || !state.models.length) {
    try {
      state.models = await ListModels();
    } catch (_) {
      state.models = [
        { id: "grok-4.5", name: "Grok 4.5" },
        { id: "grok-4.5-responses", name: "Grok 4.5 (Responses)" },
      ];
    }
  }
  paintChrome();
}

function wireEvents() {
  EventsOn("chat:event", onChatEvent);
  EventsOn("search:start", (p) => onSearchEvent("search:start", p));
  EventsOn("search:results", (p) => onSearchEvent("search:results", p));
  EventsOn("search:error", (p) => onSearchEvent("search:error", p));
  EventsOn("search:done", (p) => onSearchEvent("search:done", p));
  EventsOn("tool:call", (p) => onSearchEvent("tool:call", p));
  EventsOn("tool:args", (p) => onSearchEvent("tool:args", p));
  EventsOn("tool:done", (p) => onSearchEvent("tool:done", p));
  EventsOn("request:active", (req) => {
    state.activeRequest = req;
    paintStatus();
  });
  EventsOn("usage:update", (u) => {
    state.usage = u || {};
    const g = globalUsage();
    const set = (id, val) => {
      const n = document.getElementById(id);
      if (n) n.textContent = val;
    };
    set("u-total", fmt(g.total_tokens));
    set("u-cost", fmtUSD(g.cost_usd));
    set("u-prompt", fmt(g.prompt_tokens));
    set("u-comp", fmt(g.completion_tokens));
    set("u-reason", fmt(g.reasoning_tokens));
    if (g.latency_samples > 0) {
      set("u-lat", fmtMs(g.latency_sum_ms / g.latency_samples));
    }
    // Refresh account cards (proxy path usage / cost)
    paintChrome();
  });
  EventsOn("register:progress", (p) => {
    const st = $("#status-text");
    if (st && p) {
      st.textContent = `Registro: ${p.step || ""}${p.message ? " — " + p.message : ""}`;
    }
  });
  EventsOn("stats:sample", (sample) => {
    if (sample?.cost_usd != null) {
      state.sessionCost = sample.cost_usd;
      const el = $("#sess-cost");
      if (el) el.textContent = fmtUSD(sample.cost_usd);
    }
  });
  EventsOn("auth:success", async (payload) => {
    state.device = null;
    // Do not tear down batch-create modal mid-run (or after batch; modal owns its lifecycle)
    if (!state.batchCreating && !payload?.batch) {
      document.querySelector(".overlay")?.remove();
    }
    await refreshBootstrap(true);
    const n = payload?.count || state.accounts.length;
    const st = $("#status-text");
    if (st) {
      const label = payload?.batch
        ? escapeHtml(payload?.label || `${n} contas`)
        : `<strong>${escapeHtml(payload?.email || payload?.label || "")}</strong>`;
      st.innerHTML = `Conta(s) · ${label} · ${n} no total`;
    }
  });
  EventsOn("auth:error", (msg) => {
    alert("Auth error: " + msg);
    state.device = null;
    document.querySelector(".overlay")?.remove();
  });
  EventsOn("accounts:update", (accounts) => {
    state.accounts = accounts || [];
    paintChrome();
  });

  // Periodic recovery check — every 60s, auto-recover + refresh UI
  setInterval(async () => {
    try {
      await RecoverAccounts();
      const b = await GetBootstrap();
      state.settings = b.settings || {};
      state.accounts = b.accounts || [];
      state.usage = b.usage || {};
      paintChrome();
    } catch (_) {}
  }, 60000);
}

function sparklineSVG(values, color = "rgba(125,211,252,0.9)") {
  const nums = (values || []).map((v) => Number(v) || 0);
  if (!nums.length) {
    return `<div class="chart-empty">Sem amostras ainda — envie um chat</div>`;
  }
  const w = 320;
  const h = 88;
  const pad = 6;
  const max = Math.max(...nums, 1);
  const min = Math.min(...nums, 0);
  const span = Math.max(max - min, 1);
  const pts = nums.map((v, i) => {
    const x = pad + (i * (w - pad * 2)) / Math.max(nums.length - 1, 1);
    const y = h - pad - ((v - min) / span) * (h - pad * 2);
    return [x, y];
  });
  const line = pts.map((p, i) => (i === 0 ? `M ${p[0]} ${p[1]}` : `L ${p[0]} ${p[1]}`)).join(" ");
  const area =
    line +
    ` L ${pts[pts.length - 1][0]} ${h - pad} L ${pts[0][0]} ${h - pad} Z`;
  const last = nums[nums.length - 1];
  return `
    <svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
      <defs>
        <linearGradient id="g-${color.replace(/[^a-z0-9]/gi, "")}" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="${color}" stop-opacity="0.35"/>
          <stop offset="100%" stop-color="${color}" stop-opacity="0"/>
        </linearGradient>
      </defs>
      <path d="${area}" fill="url(#g-${color.replace(/[^a-z0-9]/gi, "")})"/>
      <path d="${line}" fill="none" stroke="${color}" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>
      <text x="${w - pad}" y="${pad + 10}" text-anchor="end" fill="rgba(255,255,255,0.35)" font-size="10">${fmt(last)}</text>
    </svg>
  `;
}

async function openStatsModal() {
  document.querySelector(".stats-overlay")?.remove();
  let stats;
  try {
    stats = await GetStats();
  } catch (e) {
    alert("Stats: " + e);
    return;
  }
  const g = stats.global || {};
  const proxy = stats.proxy || {};
  const rate = stats.active_rate || {};
  const snippets = {
    opencode: proxy.opencode || "",
    env: proxy.openai_env || "",
    curl: proxy.curl || "",
  };
  let tab = "opencode";

  const overlay = document.createElement("div");
  overlay.className = "stats-overlay";
  overlay.innerHTML = `
    <div class="stats-panel" role="dialog" aria-label="Estatísticas">
      <div class="stats-head">
        <div>
          <h2>Estatísticas & integração</h2>
          <p>Tokens, latência, custo estimado (Grok 4.5 API) e config OpenAI-compatible</p>
        </div>
        <button class="icon-btn" id="stats-close">Fechar</button>
      </div>

      <div class="stats-grid">
        <div class="kpi"><label>Tokens total</label><strong>${fmt(g.total_tokens)}</strong><span>${fmt(g.requests)} requests</span></div>
        <div class="kpi"><label>Custo est.</label><strong>${fmtUSD(g.cost_usd)}</strong><span>in $${rate.input_per_m ?? 2}/M · out $${rate.output_per_m ?? 6}/M</span></div>
        <div class="kpi"><label>Latência méd.</label><strong>${fmtMs(stats.avg_latency_ms)}</strong><span>TTFT méd. ${fmtMs(stats.avg_ttft_ms)}</span></div>
        <div class="kpi"><label>Reasoning</label><strong>${fmt(g.reasoning_tokens)}</strong><span>prompt ${fmt(g.prompt_tokens)} · out ${fmt(g.completion_tokens)}</span></div>
      </div>

      <div class="charts">
        <div class="chart-card">
          <h3>Latência total (ms) — últimas requests</h3>
          <div id="chart-lat">${sparklineSVG(stats.latency_series, "rgba(125,211,252,0.95)")}</div>
        </div>
        <div class="chart-card">
          <h3>Time to first token (ms)</h3>
          <div id="chart-ttft">${sparklineSVG(stats.ttft_series, "rgba(167,139,250,0.95)")}</div>
        </div>
      </div>

      <div class="snippet-card">
        <h3>OpenAI-compatible · Open Code / Cursor / Continue</h3>
        <p class="sub">Base URL do proxy local embutido. Cole no Open Code (provider openai-compatible) ou use as envs.</p>
        <div class="snippet-tabs">
          <button type="button" data-tab="opencode" class="on">Open Code JSON</button>
          <button type="button" data-tab="env">ENV</button>
          <button type="button" data-tab="curl">cURL</button>
        </div>
        <div class="snippet-body">
          <pre id="snippet-pre">${escapeHtml(snippets.opencode)}</pre>
          <button type="button" class="copy" id="snippet-copy">Copiar</button>
        </div>
      </div>

      <p class="pricing-note">
        Preço de referência Grok 4.5 (docs.x.ai): <b>$2.00 / 1M input</b>, <b>$0.50 / 1M cached</b>, <b>$6.00 / 1M output</b>.
        Reasoning conta como output. Valores são estimativas da sessão local — a fatura real depende do plano/conta xAI.
        ${proxy.base_url ? `Proxy: <code>${escapeHtml(proxy.base_url)}</code>` : ""}
      </p>
    </div>
  `;
  document.body.appendChild(overlay);

  const close = () => overlay.remove();
  $("#stats-close", overlay).onclick = close;
  overlay.addEventListener("click", (e) => {
    if (e.target === overlay) close();
  });
  document.addEventListener(
    "keydown",
    function esc(e) {
      if (e.key === "Escape") {
        close();
        document.removeEventListener("keydown", esc);
      }
    },
    { once: true }
  );

  const pre = $("#snippet-pre", overlay);
  const tabs = overlay.querySelectorAll(".snippet-tabs button");
  tabs.forEach((btn) => {
    btn.onclick = () => {
      tabs.forEach((b) => b.classList.remove("on"));
      btn.classList.add("on");
      tab = btn.dataset.tab;
      pre.textContent = snippets[tab] || "";
    };
  });
  $("#snippet-copy", overlay).onclick = async () => {
    await navigator.clipboard.writeText(snippets[tab] || "");
    const b = $("#snippet-copy", overlay);
    b.textContent = "Copiado";
    setTimeout(() => (b.textContent = "Copiar"), 1200);
  };
}

async function main() {
  wireEvents();
  await refreshBootstrap(true);
}

main().catch((e) => {
  document.body.innerHTML = `<pre style="color:#f88;padding:24px;font-family:monospace">Falha UI: ${e}</pre>`;
});
