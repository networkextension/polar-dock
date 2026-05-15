// Multi-pane terminal workspace for the Polar host module.
//
// Each pane is a self-contained tile: pick a host → pick a skill →
// the pane opens a fresh skill.start via POST /shell/open and mounts
// an xterm bound to the byte-bridge WS that was already used by the
// single-modal shell flow (host_shell_handlers.go).
//
// MVP scope: pure browser state. Layout = grid auto-sized by the
// toolbar's pane count selector. No persistence (refresh = blank
// panes). No drag/resize. No input broadcast. Each pane closes
// independently when its WS terminates or the user clicks ×.

import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import "@xterm/xterm/css/xterm.css";

import { fetchHosts, fetchHostSkillsForHost, openHostShell } from "./api/hosts.js";
import { fetchCurrentUser, logout } from "./api/session.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import { byId } from "./lib/dom.js";
import type { Host, HostSkill } from "./types/hosts.js";

initStoredTheme();
bindThemeSync();

const grid = byId<HTMLElement>("consoleGrid");
const layoutSelect = byId<HTMLSelectElement>("consoleLayoutSelect");
const closeAllBtn = byId<HTMLButtonElement>("consoleCloseAllBtn");
const logoutBtn = byId<HTMLButtonElement>("logoutBtn");

let hosts: Host[] = [];
const skillsByHost = new Map<string, HostSkill[]>();
const activePanes = new Map<string, ConsolePane>(); // pane_id → pane

// Console pane: one tile of the grid. Owns its DOM node, xterm,
// WS, and the "empty picker" state.
class ConsolePane {
  readonly id: string;
  readonly root: HTMLElement;
  private titleEl: HTMLElement;
  private bodyEl: HTMLElement;
  private term?: Terminal;
  private fit?: FitAddon;
  private ws?: WebSocket;
  private resizeObs?: ResizeObserver;

  constructor(id: string) {
    this.id = id;
    this.root = document.createElement("div");
    this.root.className = "console-pane";
    this.root.dataset.paneId = id;
    this.root.innerHTML = `
      <div class="console-pane-head">
        <span class="console-pane-title" data-pane-title>未选择</span>
        <button type="button" class="btn-inline" data-pane-close style="background:#dc2626;color:#fff;border-color:#dc2626;font-size:10px;padding:2px 8px;">×</button>
      </div>
      <div class="console-pane-body" data-pane-body></div>
    `;
    this.titleEl = this.root.querySelector("[data-pane-title]") as HTMLElement;
    this.bodyEl = this.root.querySelector("[data-pane-body]") as HTMLElement;

    this.root.querySelector<HTMLButtonElement>("[data-pane-close]")?.addEventListener("click", () => {
      this.dispose();
      activePanes.delete(this.id);
      this.root.remove();
      ensurePaneCount();
    });

    this.renderPicker();
  }

  private renderPicker(): void {
    this.bodyEl.innerHTML = `
      <div class="console-pane-empty">
        <div>选择主机 + skill 启动</div>
        <select data-pane-host class="input" style="max-width:240px;">
          <option value="">— host —</option>
          ${hosts.map((h) => `<option value="${h.id}">${escapeHTML(h.name)} (${escapeHTML(h.slug)})</option>`).join("")}
        </select>
        <select data-pane-skill class="input" disabled style="max-width:240px;">
          <option value="">— skill —</option>
        </select>
        <button type="button" class="btn-inline btn-secondary" data-pane-start disabled>启动</button>
      </div>
    `;

    const hostSel = this.bodyEl.querySelector<HTMLSelectElement>("[data-pane-host]")!;
    const skillSel = this.bodyEl.querySelector<HTMLSelectElement>("[data-pane-skill]")!;
    const startBtn = this.bodyEl.querySelector<HTMLButtonElement>("[data-pane-start]")!;

    hostSel.addEventListener("change", async () => {
      const hostId = hostSel.value;
      skillSel.innerHTML = `<option value="">加载中...</option>`;
      skillSel.disabled = true;
      startBtn.disabled = true;
      if (!hostId) {
        skillSel.innerHTML = `<option value="">— skill —</option>`;
        return;
      }
      let list = skillsByHost.get(hostId);
      if (!list) {
        const { response, data } = await fetchHostSkillsForHost(hostId);
        if (!response.ok) {
          skillSel.innerHTML = `<option value="">加载失败</option>`;
          return;
        }
        list = data.skills || [];
        skillsByHost.set(hostId, list);
      }
      // Only byte-streaming skills can host a pane (shell + nanokdp).
      const usable = list.filter((s) => s.kind === "shell" || s.kind === "nanokdp");
      skillSel.innerHTML =
        `<option value="">— skill —</option>` +
        usable.map((s) => `<option value="${s.id}">${escapeHTML(s.kind)} (id=${s.id})</option>`).join("");
      skillSel.disabled = false;
    });

    skillSel.addEventListener("change", () => {
      startBtn.disabled = !skillSel.value;
    });

    startBtn.addEventListener("click", async () => {
      const hostId = hostSel.value;
      const skillId = Number(skillSel.value);
      if (!hostId || !skillId) return;
      startBtn.disabled = true;
      startBtn.textContent = "启动中...";
      try {
        await this.connect(hostId, skillId);
      } catch (err) {
        startBtn.disabled = false;
        startBtn.textContent = "启动";
        alert(`启动失败: ${err instanceof Error ? err.message : String(err)}`);
      }
    });
  }

  private async connect(hostId: string, skillId: number): Promise<void> {
    const host = hosts.find((h) => h.id === hostId);
    const fit = new FitAddon();
    const proposed = { rows: 30, cols: 100 };
    const { response, data } = await openHostShell(hostId, skillId, proposed);
    if (!response.ok || !data.ws_url) {
      throw new Error(data.error || `HTTP ${response.status}`);
    }

    // Tear down the picker and put a fresh terminal in.
    this.bodyEl.innerHTML = `<div class="console-pane-term" data-pane-term></div>`;
    const termEl = this.bodyEl.querySelector<HTMLElement>("[data-pane-term]")!;
    const term = new Terminal({
      fontFamily: "ui-monospace, 'JetBrains Mono', monospace",
      fontSize: 12,
      cursorBlink: true,
      convertEol: false,
      theme: { background: "#0f172a", foreground: "#e2e8f0" },
    });
    const linksAddon = new WebLinksAddon();
    term.loadAddon(fit);
    term.loadAddon(linksAddon);
    term.open(termEl);
    setTimeout(() => fit.fit(), 0);
    term.focus();

    this.term = term;
    this.fit = fit;
    this.titleEl.textContent = `${host?.name || hostId} · run=${data.run_id}`;

    const ws = new WebSocket(data.ws_url);
    ws.binaryType = "arraybuffer";
    this.ws = ws;

    const sendResize = () => {
      try {
        fit.fit();
      } catch {
        return;
      }
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ kind: "resize", rows: term.rows, cols: term.cols }));
      }
    };
    ws.addEventListener("open", () => {
      term.writeln(`\x1b[2m[console] connected · run=${data.run_id}\x1b[0m`);
      sendResize();
    });
    ws.addEventListener("message", (ev) => {
      if (typeof ev.data === "string") return;
      term.write(new Uint8Array(ev.data));
    });
    ws.addEventListener("close", (ev) => {
      term.writeln(`\r\n\x1b[2m[console] closed (code=${ev.code})\x1b[0m`);
      term.options.disableStdin = true;
    });
    ws.addEventListener("error", () => {
      term.writeln(`\r\n\x1b[31m[console] WS error\x1b[0m`);
    });
    term.onData((s) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(s));
      }
    });
    term.onResize(() => sendResize());

    // React to grid layout changes so the xterm cell occupies its
    // tile fully. ResizeObserver on the pane root catches both the
    // user changing layout count and browser window resize.
    this.resizeObs = new ResizeObserver(() => {
      if (this.fit) {
        try {
          this.fit.fit();
        } catch {
          /* term may be disposed */
        }
      }
    });
    this.resizeObs.observe(this.root);
  }

  dispose(): void {
    this.resizeObs?.disconnect();
    if (this.ws && this.ws.readyState <= WebSocket.OPEN) {
      try {
        this.ws.close(1000, "pane_closed");
      } catch {
        /* ignore */
      }
    }
    this.term?.dispose();
  }
}

function ensurePaneCount(): void {
  const target = Number(layoutSelect.value || 4);
  const cols = target <= 1 ? 1 : target <= 4 ? 2 : 3;
  grid.dataset.cols = String(cols);

  while (activePanes.size < target) {
    const id = "p" + Math.random().toString(36).slice(2, 8);
    const pane = new ConsolePane(id);
    activePanes.set(id, pane);
    grid.appendChild(pane.root);
  }
  // Shrink if user picked fewer panes. Prefer to drop empty (un-started)
  // panes first so we don't terminate live shells unexpectedly.
  while (activePanes.size > target) {
    const candidates = Array.from(activePanes.values());
    const pickle =
      candidates.find((p) => !p.root.querySelector("[data-pane-term]")) ||
      candidates[candidates.length - 1];
    pickle.dispose();
    activePanes.delete(pickle.id);
    pickle.root.remove();
  }
}

async function bootstrap(): Promise<void> {
  await hydrateSiteBrand();
  const { response: meRes, data: me } = await fetchCurrentUser();
  if (!meRes.ok) {
    window.location.href = "/login.html?next=/console.html";
    return;
  }
  renderSidebarFoot(me);

  const { response: hostsRes, data: hostsData } = await fetchHosts();
  if (hostsRes.ok) {
    hosts = hostsData.hosts || [];
  }

  layoutSelect.addEventListener("change", () => ensurePaneCount());
  closeAllBtn.addEventListener("click", () => {
    activePanes.forEach((p) => {
      p.dispose();
      p.root.remove();
    });
    activePanes.clear();
    ensurePaneCount();
  });
  logoutBtn.addEventListener("click", async () => {
    await logout();
    window.location.href = "/login.html";
  });

  ensurePaneCount();
}

function escapeHTML(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

void bootstrap();
