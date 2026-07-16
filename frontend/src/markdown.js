import { marked } from "marked";
import DOMPurify from "dompurify";
import { escapeHtml } from "./util.js";

marked.setOptions({
  gfm: true,
  breaks: true,
  pedantic: false,
});

export function renderMarkdown(text) {
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

export function enhanceMarkdownRoot(root) {
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
