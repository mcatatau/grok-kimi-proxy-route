export function $(sel, root = document) {
  return root.querySelector(sel);
}

export function fmt(n) {
  if (n == null) return "0";
  return Number(n).toLocaleString("en-US");
}

export function fmtUSD(n) {
  const v = Number(n) || 0;
  if (v > 0 && v < 0.0001) return "<$0.0001";
  return "$" + v.toFixed(v >= 1 ? 2 : 4);
}

export function fmtMs(n) {
  if (n == null || n <= 0) return "—";
  if (n < 1000) return Math.round(n) + " ms";
  return (n / 1000).toFixed(2) + " s";
}

export function shortPath(p) {
  if (!p) return "";
  const s = String(p);
  return s.length <= 36 ? s : "…" + s.slice(-34);
}

export function initials(s) {
  if (!s) return "?";
  const p = String(s).split(/[\s@._-]+/).filter(Boolean);
  return ((p[0]?.[0] || "?") + (p[1]?.[0] || "")).toUpperCase();
}

export function escapeHtml(s) {
  return String(s ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

export function domainFromUrl(u) {
  try {
    return new URL(u).hostname.replace(/^www\./, "");
  } catch {
    return "";
  }
}

export function kindLabel(kind) {
  if (kind === "x") return "X";
  return "Web";
}

/** Format seconds as cooldown e.g. 5h 12m, 45m, 30s */
export function fmtDuration(sec) {
  let s = Math.max(0, Math.floor(Number(sec) || 0));
  if (s <= 0) return "0s";
  const d = Math.floor(s / 86400);
  s %= 86400;
  const h = Math.floor(s / 3600);
  s %= 3600;
  const m = Math.floor(s / 60);
  s %= 60;
  if (d > 0) return h > 0 ? d + "d " + h + "h" : d + "d";
  if (h > 0) return m > 0 ? h + "h " + m + "m" : h + "h";
  if (m > 0) return s > 0 && m < 5 ? m + "m " + s + "s" : m + "m";
  return s + "s";
}

/** Account can be used for requests (not exhausted / chat-denied / dead SSO). */
export function isUsableAccount(a) {
  if (!a) return false;
  if (a.exhausted) return false;
  if (a.chat_denied) return false;
  if (a.needs_login) return false;
  if (a.expired && a.has_refresh === false) return false;
  return true;
}

export function countUsableAccounts(list) {
  return (list || []).filter(isUsableAccount).length;
}
