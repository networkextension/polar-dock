// Multi-pane terminal workspace for the Polar host module.
//
// Each pane is a self-contained tile: pick a host → pick a skill →
// the pane opens a fresh skill.start via POST /shell/open and mounts
// an xterm bound to the byte-bridge WS that was already used by the
// single-modal shell flow (host_shell_handlers.go).
//
// Layouts are saved per-user-per-workspace and recallable from the
// "已保存" dropdown. Loading a saved layout auto-starts every pane's
// shell — the operator doesn't have to click "Start" N times.

import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import "@xterm/xterm/css/xterm.css";

import { fetchHosts, fetchHostSkillsForHost, listHostShellSessions, openHostShell } from "./api/hosts.js";
import { fetchCurrentUser, logout } from "./api/session.js";
import {
  createConsoleLayout,
  deleteConsoleLayout,
  fetchConsoleLayouts,
  updateConsoleLayout,
} from "./api/console-layouts.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import { byId } from "./lib/dom.js";
import type { Host, HostSkill } from "./types/hosts.js";
import type { ConsoleLayout, ConsolePaneConfig } from "./types/console.js";

initStoredTheme();
bindThemeSync();

const grid = byId<HTMLElement>("consoleGrid");
const layoutSelect = byId<HTMLSelectElement>("consoleLayoutSelect");
const savedSelect = byId<HTMLSelectElement>("consoleSavedSelect");
const saveBtn = byId<HTMLButtonElement>("consoleSaveBtn");
const deleteBtn = byId<HTMLButtonElement>("consoleDeleteBtn");
const closeAllBtn = byId<HTMLButtonElement>("consoleCloseAllBtn");
const logoutBtn = byId<HTMLButtonElement>("logoutBtn");

let hosts: Host[] = [];
const skillsByHost = new Map<string, HostSkill[]>();
const activePanes = new Map<string, ConsolePane>(); // pane_id → pane
const savedLayouts = new Map<number, ConsoleLayout>();

// Console pane: one tile of the grid. Owns its DOM node, xterm,
// WS, and the "empty picker" state.
class ConsolePane {
  readonly id: string;
  readonly root: HTMLElement;
  // Tracked so saveCurrentLayout() can serialize the grid.
  currentHostId?: string;
  currentSkillId?: number;
  private titleEl: HTMLElement;
  private bodyEl: HTMLElement;
  private term?: Terminal;
  private fit?: FitAddon;
  private ws?: WebSocket;
  private resizeObs?: ResizeObserver;

  constructor(id: string, autofill?: { hostId: string; hostSkillId: number }) {
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

    if (autofill) {
      const hostLabel = hosts.find((h) => h.id === autofill.hostId)?.name || autofill.hostId;
      this.titleEl.textContent = `${hostLabel} · 启动中...`;
      this.bodyEl.innerHTML = `<div class="console-pane-empty"><div>启动中...</div></div>`;
      void this.connect(autofill.hostId, autofill.hostSkillId).catch((err) => {
        this.renderLoadError(autofill.hostId, autofill.hostSkillId, err);
      });
    } else {
      this.renderPicker();
    }
  }

  private renderPicker(): void {
    // The start button is styled inline (not via .btn-* classes) so it
    // pops on the dark pane background — the global secondary button
    // uses surface-light tones that vanish on #0f172a.
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
        <select data-pane-shell class="input" hidden style="max-width:240px;">
          <option value="">默认 shell (auto)</option>
          <option value="bash">bash</option>
          <option value="zsh">zsh</option>
          <option value="sh">sh</option>
        </select>
        <button type="button" data-pane-start disabled
          style="padding:8px 24px;border:none;border-radius:8px;background:#16a34a;color:#fff;font-weight:600;font-size:13px;cursor:pointer;opacity:.5;">
          启动
        </button>
      </div>
    `;

    const hostSel = this.bodyEl.querySelector<HTMLSelectElement>("[data-pane-host]")!;
    const skillSel = this.bodyEl.querySelector<HTMLSelectElement>("[data-pane-skill]")!;
    const shellSel = this.bodyEl.querySelector<HTMLSelectElement>("[data-pane-shell]")!;
    const startBtn = this.bodyEl.querySelector<HTMLButtonElement>("[data-pane-start]")!;

    const setStartEnabled = (enabled: boolean) => {
      startBtn.disabled = !enabled;
      startBtn.style.opacity = enabled ? "1" : ".5";
      startBtn.style.cursor = enabled ? "pointer" : "not-allowed";
    };

    hostSel.addEventListener("change", async () => {
      const hostId = hostSel.value;
      skillSel.innerHTML = `<option value="">加载中...</option>`;
      skillSel.disabled = true;
      shellSel.hidden = true;
      setStartEnabled(false);
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
      const selected = skillSel.selectedOptions[0]?.textContent || "";
      shellSel.hidden = !selected.startsWith("shell");
      setStartEnabled(!!skillSel.value);
    });

    startBtn.addEventListener("click", async () => {
      const hostId = hostSel.value;
      const skillId = Number(skillSel.value);
      if (!hostId || !skillId) return;
      setStartEnabled(false);
      startBtn.textContent = "启动中...";
      try {
        const shellPref = shellSel.hidden ? undefined : shellSel.value || undefined;
        await this.connect(hostId, skillId, shellPref);
      } catch (err) {
        setStartEnabled(true);
        startBtn.textContent = "启动";
        alert(`启动失败: ${err instanceof Error ? err.message : String(err)}`);
      }
    });
  }

  private renderLoadError(hostId: string, skillId: number, err: unknown): void {
    const msg = err instanceof Error ? err.message : String(err);
    this.titleEl.textContent = `${hostId} · 失败`;
    this.bodyEl.innerHTML = `
      <div class="console-pane-empty">
        <div style="color:#fca5a5;">无法启动 host=${escapeHTML(hostId)} skill=${skillId}</div>
        <div style="font-size:11px; color:#94a3b8;">${escapeHTML(msg)}</div>
        <button type="button" class="btn-inline btn-secondary" data-pane-repick>重新选择</button>
      </div>
    `;
    this.bodyEl.querySelector<HTMLButtonElement>("[data-pane-repick]")?.addEventListener("click", () => {
      this.currentHostId = undefined;
      this.currentSkillId = undefined;
      this.renderPicker();
    });
  }

  private async connect(hostId: string, skillId: number, shell?: string): Promise<void> {
    const host = hosts.find((h) => h.id === hostId);
    const fit = new FitAddon();
    const proposed: { rows: number; cols: number; shell?: string } = { rows: 30, cols: 100 };
    if (shell) proposed.shell = shell;
    const { response, data } = await openHostShell(hostId, skillId, proposed);
    if (!response.ok || !data.ws_url) {
      throw new Error(data.error || `HTTP ${response.status}`);
    }
    this.currentHostId = hostId;
    this.currentSkillId = skillId;

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

function gridColsFor(paneCount: number): number {
  if (paneCount <= 1) return 1;
  if (paneCount <= 4) return 2;
  return 3;
}

function ensurePaneCount(): void {
  const target = Number(layoutSelect.value || 4);
  grid.dataset.cols = String(gridColsFor(target));

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

// Pick a layout-count select value matching this pane count, so the
// existing `data-cols` mapping stays the single source of truth.
function nearestLayoutCount(n: number): number {
  const known = [1, 2, 3, 4, 6, 9];
  for (const v of known) {
    if (v >= n) return v;
  }
  return known[known.length - 1];
}

function disposeAllPanes(): void {
  activePanes.forEach((p) => {
    p.dispose();
    p.root.remove();
  });
  activePanes.clear();
}

function loadLayout(layout: ConsoleLayout): void {
  const panes = layout.panes_config || [];
  if (panes.length === 0) {
    alert("空布局，未生效");
    return;
  }
  disposeAllPanes();
  const target = nearestLayoutCount(panes.length);
  layoutSelect.value = String(target);
  grid.dataset.cols = String(gridColsFor(target));

  for (const cfg of panes) {
    const id = "p" + Math.random().toString(36).slice(2, 8);
    const pane = new ConsolePane(id, { hostId: cfg.host_id, hostSkillId: cfg.host_skill_id });
    activePanes.set(id, pane);
    grid.appendChild(pane.root);
  }
  // Fill remaining slots (e.g. layout has 3 panes but layoutSelect maps to 4)
  // with empty pickers so the grid looks consistent.
  while (activePanes.size < target) {
    const id = "p" + Math.random().toString(36).slice(2, 8);
    const pane = new ConsolePane(id);
    activePanes.set(id, pane);
    grid.appendChild(pane.root);
  }
}

function refreshSavedSelect(selectedId?: number): void {
  const opts: string[] = [`<option value="">— 选择已保存的布局 —</option>`];
  const items = Array.from(savedLayouts.values()).sort((a, b) =>
    a.updated_at < b.updated_at ? 1 : -1,
  );
  for (const l of items) {
    const sel = selectedId && l.id === selectedId ? " selected" : "";
    opts.push(`<option value="${l.id}"${sel}>${escapeHTML(l.name)}</option>`);
  }
  savedSelect.innerHTML = opts.join("");
  deleteBtn.disabled = !savedSelect.value;
}

async function refreshSavedLayouts(selectedId?: number): Promise<void> {
  const { response, data } = await fetchConsoleLayouts();
  savedLayouts.clear();
  if (response.ok && data.layouts) {
    for (const l of data.layouts) {
      savedLayouts.set(l.id, l);
    }
  }
  refreshSavedSelect(selectedId);
}

function currentPanesConfig(): ConsolePaneConfig[] {
  const panes: ConsolePaneConfig[] = [];
  // Iterate in DOM order so the saved layout matches what the
  // operator sees on screen.
  for (const child of Array.from(grid.children) as HTMLElement[]) {
    const paneId = child.dataset.paneId;
    if (!paneId) continue;
    const pane = activePanes.get(paneId);
    if (!pane || !pane.currentHostId || !pane.currentSkillId) continue;
    panes.push({ host_id: pane.currentHostId, host_skill_id: pane.currentSkillId });
  }
  return panes;
}

async function saveCurrentLayout(): Promise<void> {
  const panes = currentPanesConfig();
  if (panes.length === 0) {
    alert("没有可保存的活动 pane（请先启动至少一个 shell/nanokdp）");
    return;
  }
  // If a layout is currently selected, default to updating it; allow
  // rename. Otherwise prompt for a new name.
  const selectedId = Number(savedSelect.value || 0);
  const existing = selectedId ? savedLayouts.get(selectedId) : undefined;
  const defaultName = existing?.name || `布局 ${new Date().toLocaleString()}`;
  const name = window.prompt("布局名称", defaultName);
  if (!name) return;
  const trimmed = name.trim();
  if (!trimmed) return;

  if (existing && trimmed === existing.name) {
    const { response, data } = await updateConsoleLayout(existing.id, { panes_config: panes });
    if (!response.ok) {
      alert(`保存失败: ${data.error || response.status}`);
      return;
    }
    await refreshSavedLayouts(existing.id);
  } else {
    const { response, data } = await createConsoleLayout({ name: trimmed, panes_config: panes });
    if (!response.ok) {
      const hint = response.status === 409 ? "已存在同名布局，请换一个名字" : data.error;
      alert(`保存失败: ${hint || response.status}`);
      return;
    }
    await refreshSavedLayouts(data.layout?.id);
  }
}

async function deleteSelectedLayout(): Promise<void> {
  const id = Number(savedSelect.value || 0);
  if (!id) return;
  const layout = savedLayouts.get(id);
  if (!layout) return;
  if (!window.confirm(`确定删除布局 "${layout.name}"？`)) return;
  const { response, data } = await deleteConsoleLayout(id);
  if (!response.ok) {
    alert(`删除失败: ${data.error || response.status}`);
    return;
  }
  await refreshSavedLayouts();
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
    disposeAllPanes();
    ensurePaneCount();
  });
  savedSelect.addEventListener("change", () => {
    deleteBtn.disabled = !savedSelect.value;
    const id = Number(savedSelect.value || 0);
    if (!id) return;
    const layout = savedLayouts.get(id);
    if (layout) loadLayout(layout);
  });
  saveBtn.addEventListener("click", () => {
    void saveCurrentLayout();
  });
  deleteBtn.addEventListener("click", () => {
    void deleteSelectedLayout();
  });
  logoutBtn.addEventListener("click", async () => {
    await logout();
    window.location.href = "/login.html";
  });

  await refreshSavedLayouts();

  // Reattach: if the operator has live shell sessions across hosts
  // (e.g. they refreshed the page), spawn panes pre-bound to those
  // (host, host_skill) tuples — openHostShell on the dock side will
  // dedupe and return the existing run_id, so the panes land back on
  // the live PTYs without the operator having to re-pick anything.
  const restored = await restoreActiveSessions();
  if (restored === 0) {
    ensurePaneCount();
  }
}

async function restoreActiveSessions(): Promise<number> {
  if (hosts.length === 0) return 0;
  const sessions: { hostId: string; hostSkillId: number }[] = [];
  await Promise.all(
    hosts.map(async (h) => {
      const { response, data } = await listHostShellSessions(h.id);
      if (!response.ok || !data.sessions) return;
      for (const s of data.sessions) {
        sessions.push({ hostId: s.host_id, hostSkillId: s.host_skill_id });
      }
    }),
  );
  if (sessions.length === 0) return 0;
  const target = nearestLayoutCount(sessions.length);
  layoutSelect.value = String(target);
  grid.dataset.cols = String(gridColsFor(target));
  for (const cfg of sessions) {
    const id = "p" + Math.random().toString(36).slice(2, 8);
    const pane = new ConsolePane(id, { hostId: cfg.hostId, hostSkillId: cfg.hostSkillId });
    activePanes.set(id, pane);
    grid.appendChild(pane.root);
  }
  while (activePanes.size < target) {
    const id = "p" + Math.random().toString(36).slice(2, 8);
    const pane = new ConsolePane(id);
    activePanes.set(id, pane);
    grid.appendChild(pane.root);
  }
  return sessions.length;
}

function escapeHTML(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

void bootstrap();
