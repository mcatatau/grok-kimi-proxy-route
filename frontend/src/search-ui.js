import { escapeHtml, domainFromUrl, kindLabel } from "./util.js";
import { state } from "./state.js";
import { paintMessages } from "./shell.js";

export function ensureSearch(last, id, kind) {
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

export function ensureTool(last, id, name) {
  if (!last.tools) last.tools = [];
  let t = last.tools.find((x) => x.id === id);
  if (!t) {
    t = { id, name: name || "web_search", status: "running", query: "" };
    last.tools.push(t);
  }
  return t;
}

export function faviconUrl(domainOrUrl) {
  const d = domainOrUrl.includes(".") && !domainOrUrl.includes("://")
    ? domainOrUrl
    : domainFromUrl(domainOrUrl) || domainOrUrl;
  if (!d) return "";
  return `https://www.google.com/s2/favicons?domain=${encodeURIComponent(d)}&sz=64`;
}

export function renderFavStack(results, max = 5) {
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

export function renderSourceCards(results) {
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

export function renderSearchBlock(m) {
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

export function onSearchEvent(type, payload) {
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
  requestAnimationFrame(() => paintMessages());
}

export function onChatEventTool(ev) {
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
