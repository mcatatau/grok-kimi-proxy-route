import { state } from "./state.js";
import { escapeHtml } from "./util.js";

export function mountMenu(root, { id, options, value, prefix, onChange, chip }) {
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

export function closeAllMenus() {
  document.querySelectorAll(".dd.open").forEach((el) => el.classList.remove("open"));
}

document.addEventListener("click", () => closeAllMenus());
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeAllMenus();
});
