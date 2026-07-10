import "./style.css";
import "./app.css";

import {
  GetBootstrap,
  ListModels,
  RecoverAccounts,
  CancelChat,
} from "../wailsjs/go/main/App";
import { EventsOn } from "../wailsjs/runtime/runtime";

import { state } from "./state.js";
import { $, escapeHtml, fmt, fmtUSD, fmtMs } from "./util.js";
import { onSearchEvent } from "./search-ui.js";
import { openStatsModal } from "./stats.js";
import {
  ensureShell,
  paintChrome,
  paintStatus,
  paintSend,
  globalUsage,
  refreshBootstrap,
} from "./shell.js";
import { submit, onChatEvent } from "./chat.js";

// Wire send after shell exists
function wireShellActions() {
  const send = $("#send");
  if (send && !send._wired) {
    send._wired = true;
    send.onclick = () => {
      if (state.streaming) CancelChat();
      else submit();
    };
  }
  const prompt = $("#prompt");
  if (prompt && !prompt._wired) {
    prompt._wired = true;
    prompt.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        if (!state.streaming) submit();
      }
    });
  }
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
    if (!state.batchCreating && !payload?.batch) {
      document.querySelector(".overlay")?.remove();
    }
    await refreshBootstrap(true);
    wireShellActions();
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

async function main() {
  wireEvents();
  await refreshBootstrap(true);
  wireShellActions();
  // stats buttons are wired in ensureShell via openStatsModal import
}

main().catch((e) => {
  document.body.innerHTML = `<pre style="color:#f88;padding:24px;font-family:monospace">Falha UI: ${e}</pre>`;
});
