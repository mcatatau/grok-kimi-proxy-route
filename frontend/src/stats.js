import { GetStats } from "../wailsjs/go/main/App";
import { $, escapeHtml, fmt, fmtUSD, fmtMs } from "./util.js";

export function sparklineSVG(values, color = "rgba(125,211,252,0.9)") {
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

export async function openStatsModal() {
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
