// /hosts.html — Host module Phase 0 page logic.
//
// Behavior:
//   - List of workspace-scoped hosts on the left, click to open detail
//     on the right.
//   - "+ 注册主机" opens a modal that mints a one-time enrollment
//     token; UI shows the install snippet and a copy button.
//   - Detail panel shows os/arch, last-seen status, and the
//     advertised_skills array (read-only in P0 — enabling/configuring
//     skills lands with P1+).
//
// No skill enable / start / stop yet. That's by design; per
// doc/host-module-dev.md the foundation lands first.

import { fetchCurrentUser, logout } from "./api/session.js";
import { deleteHost, enrollHost, fetchHost, fetchHosts } from "./api/hosts.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type { Host } from "./types/hosts.js";

initStoredTheme();
bindThemeSync();

// ── DOM ────────────────────────────────────────────────────────────────
const hostsList = byId<HTMLElement>("hostsList");
const hostsEmpty = byId<HTMLElement>("hostsEmpty");
const hostsPanel = byId<HTMLElement>("hostsPanel");
const hostsName = byId<HTMLElement>("hostsName");
const hostsStatusBadge = byId<HTMLElement>("hostsStatusBadge");
const hostsOSArch = byId<HTMLElement>("hostsOSArch");
const hostsSlug = byId<HTMLElement>("hostsSlug");
const hostsSkillsList = byId<HTMLElement>("hostsSkillsList");
const hostsLastSeen = byId<HTMLElement>("hostsLastSeen");
const hostsLastSeenIP = byId<HTMLElement>("hostsLastSeenIP");
const hostsDeleteBtn = byId<HTMLButtonElement>("hostsDeleteBtn");

const hostsAddBtn = byId<HTMLButtonElement>("hostsAddBtn");
const hostsAddModal = byId<HTMLElement>("hostsAddModal");
const hostsAddModalCloseBtn = byId<HTMLButtonElement>("hostsAddModalCloseBtn");
const hostsAddForm = byId<HTMLFormElement>("hostsAddForm");
const hostsAddNameInput = byId<HTMLInputElement>("hostsAddName");
const hostsAddStatus = byId<HTMLElement>("hostsAddStatus");
const hostsAddSubmitBtn = byId<HTMLButtonElement>("hostsAddSubmitBtn");
const hostsAddTokenBox = byId<HTMLElement>("hostsAddTokenBox");
const hostsAddTokenSnippet = byId<HTMLElement>("hostsAddTokenSnippet");
const hostsAddCopyBtn = byId<HTMLButtonElement>("hostsAddCopyBtn");

const logoutBtn = byId<HTMLButtonElement>("logoutBtn");

// ── state ───────────────────────────────────────────────────────────────
let hosts: Host[] = [];
let activeHostId: string | null = null;
let activeHost: Host | null = null;

// ── helpers ─────────────────────────────────────────────────────────────
function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function formatRelative(iso?: string): string {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  const diff = Date.now() - then;
  if (diff < 0) return new Date(iso).toLocaleString();
  if (diff < 5_000) return "刚刚";
  if (diff < 60_000) return `${Math.floor(diff / 1000)} 秒前`;
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;
  return `${Math.floor(diff / 86_400_000)} 天前`;
}

// Online threshold: agent advertises every WS handshake + a future
// heartbeat will keep last_seen_at fresh. 60s gives generous slack
// for cellular / wifi roaming without false-yellow flicker.
const ONLINE_MS = 60_000;
function isOnline(host: Host): boolean {
  if (!host.last_seen_at) return false;
  return Date.now() - new Date(host.last_seen_at).getTime() < ONLINE_MS;
}

function setModalOpen(modal: HTMLElement, open: boolean): void {
  modal.classList.toggle("open", open);
  modal.setAttribute("aria-hidden", open ? "false" : "true");
}

function clearStatus(el: HTMLElement | null): void {
  if (!el) return;
  el.textContent = "";
  el.classList.remove("status-success", "status-error");
}

function setStatus(el: HTMLElement | null, msg: string, error = false): void {
  if (!el) return;
  el.textContent = msg;
  el.classList.toggle("status-error", error);
  el.classList.toggle("status-success", !error);
}

// ── list rendering ──────────────────────────────────────────────────────
function renderHostsList(): void {
  if (!hosts.length) {
    hostsList.innerHTML = `<div class="chat-empty">还没有主机。点右上角「＋ 注册主机」开始。</div>`;
    return;
  }
  hostsList.innerHTML = hosts
    .map((h) => {
      const online = isOnline(h);
      const dot = online ? "🟢" : "⚪";
      const status = online ? `online · ${formatRelative(h.last_seen_at)}` : `offline · ${formatRelative(h.last_seen_at)}`;
      const skillCount = (h.advertised_skills || []).length;
      const skillBadges = (h.advertised_skills || [])
        .slice(0, 4)
        .map((s) => `<span class="settings-value-pill" style="font-size:10px;">${escapeHTML(s.kind)}</span>`)
        .join(" ");
      const active = h.id === activeHostId ? "active" : "";
      return `
        <div class="video-studio-project-item ${active}" data-host-id="${escapeHTML(h.id)}">
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">
              ${dot} ${escapeHTML(h.name)}
            </div>
            <div style="font-size:11px; color:var(--text-muted,#888); margin-top:2px;">
              ${escapeHTML(h.os || "?")}/${escapeHTML(h.arch || "?")} · ${escapeHTML(status)}
            </div>
            <div style="margin-top:4px; display:flex; gap:4px; flex-wrap:wrap;">
              ${skillBadges}
              ${skillCount > 4 ? `<span style="font-size:10px; color:var(--text-muted,#888);">+${skillCount - 4}</span>` : ""}
            </div>
          </div>
        </div>`;
    })
    .join("");

  hostsList.querySelectorAll<HTMLElement>("[data-host-id]").forEach((el) => {
    el.addEventListener("click", () => {
      const id = el.getAttribute("data-host-id");
      if (id) void openHost(id);
    });
  });
}

// ── detail rendering ────────────────────────────────────────────────────
function renderHostDetail(host: Host): void {
  hostsEmpty.hidden = true;
  hostsPanel.hidden = false;
  activeHost = host;

  hostsName.textContent = host.name;
  const online = isOnline(host);
  hostsStatusBadge.textContent = online ? "🟢 online" : "⚪ offline";
  hostsStatusBadge.style.background = online ? "rgba(34,197,94,.12)" : "rgba(148,163,184,.12)";
  hostsOSArch.textContent = `${host.os || "?"}/${host.arch || "?"}`;
  hostsSlug.textContent = host.slug;

  const skills = host.advertised_skills || [];
  if (!skills.length) {
    hostsSkillsList.innerHTML = `<div class="chat-empty">agent 还没上报任何 skill（可能是旧版 polar-agent，升级后会自动上报）。</div>`;
  } else {
    hostsSkillsList.innerHTML = skills
      .map((s) => {
        const caps = s.capabilities ? Object.keys(s.capabilities).join(", ") : "";
        return `
          <div class="video-studio-project-item" style="cursor:default;">
            <div style="flex:1; min-width:0;">
              <div style="font-weight:500;">${escapeHTML(s.kind)}${s.version ? ` <span style="font-size:11px; color:var(--text-muted,#888);">v${escapeHTML(s.version)}</span>` : ""}</div>
              ${caps ? `<div style="font-size:11px; color:var(--text-muted,#888); margin-top:2px;">${escapeHTML(caps)}</div>` : ""}
            </div>
            <button type="button" class="btn-inline" disabled title="启用功能在 Phase 1+ 落地" style="opacity:.5;">启用</button>
          </div>`;
      })
      .join("");
  }

  hostsLastSeen.textContent = host.last_seen_at ? formatRelative(host.last_seen_at) + " （" + new Date(host.last_seen_at).toLocaleString() + "）" : "—";
  hostsLastSeenIP.textContent = host.last_seen_ip ? `IP: ${host.last_seen_ip}` : "IP: —";

  hostsDeleteBtn.hidden = false;
}

async function loadHosts(): Promise<void> {
  const { response, data } = await fetchHosts();
  if (!response.ok) {
    hostsList.innerHTML = `<div class="chat-empty">加载失败：${escapeHTML(data.error || response.statusText)}</div>`;
    return;
  }
  hosts = data.hosts || [];
  renderHostsList();
  if (activeHostId) {
    const stillExists = hosts.find((h) => h.id === activeHostId);
    if (!stillExists) {
      activeHostId = null;
      activeHost = null;
      hostsEmpty.hidden = false;
      hostsPanel.hidden = true;
    }
  }
}

async function openHost(id: string): Promise<void> {
  activeHostId = id;
  renderHostsList(); // re-render to update active highlight
  const { response, data } = await fetchHost(id);
  if (!response.ok || !data.host) {
    hostsEmpty.hidden = false;
    hostsPanel.hidden = true;
    return;
  }
  renderHostDetail(data.host);
}

// ── modal: add host ─────────────────────────────────────────────────────
hostsAddBtn.addEventListener("click", () => {
  hostsAddNameInput.value = "";
  clearStatus(hostsAddStatus);
  hostsAddTokenBox.hidden = true;
  hostsAddSubmitBtn.disabled = false;
  hostsAddSubmitBtn.textContent = "生成注册凭证";
  setModalOpen(hostsAddModal, true);
  setTimeout(() => hostsAddNameInput.focus(), 0);
});

hostsAddModalCloseBtn.addEventListener("click", () => {
  setModalOpen(hostsAddModal, false);
  void loadHosts();
});

hostsAddForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = hostsAddNameInput.value.trim();
  if (!name) {
    setStatus(hostsAddStatus, "请填写主机名称", true);
    return;
  }
  hostsAddSubmitBtn.disabled = true;
  hostsAddSubmitBtn.textContent = "生成中...";
  setStatus(hostsAddStatus, "");

  const { response, data } = await enrollHost({ name });
  hostsAddSubmitBtn.disabled = false;
  hostsAddSubmitBtn.textContent = "生成注册凭证";

  if (!response.ok || !data.token) {
    setStatus(hostsAddStatus, data.error || "生成失败", true);
    return;
  }
  setStatus(hostsAddStatus, `凭证已生成，1 小时内有效。`, false);
  hostsAddTokenBox.hidden = false;
  hostsAddTokenSnippet.textContent = data.install_hint || `polar-agent register --token=${data.token} --name="${name}"`;
});

hostsAddCopyBtn.addEventListener("click", async () => {
  const text = hostsAddTokenSnippet.textContent || "";
  try {
    await navigator.clipboard.writeText(text);
    hostsAddCopyBtn.textContent = "已复制";
    setTimeout(() => {
      hostsAddCopyBtn.textContent = "复制";
    }, 1500);
  } catch {
    window.prompt("复制下面这段命令：", text);
  }
});

// ── delete host ─────────────────────────────────────────────────────────
hostsDeleteBtn.addEventListener("click", async () => {
  if (!activeHost) return;
  if (!window.confirm(`确认删除主机「${activeHost.name}」？关联的 skill 配置会一并清理（P0 阶段还没有配置，所以本质上只是从列表移除）。`)) {
    return;
  }
  const { response, data } = await deleteHost(activeHost.id);
  if (!response.ok) {
    window.alert(data.error || "删除失败");
    return;
  }
  activeHostId = null;
  activeHost = null;
  hostsEmpty.hidden = false;
  hostsPanel.hidden = true;
  hostsDeleteBtn.hidden = true;
  await loadHosts();
});

// ── boot ────────────────────────────────────────────────────────────────
async function boot(): Promise<void> {
  const { response, data } = await fetchCurrentUser();
  if (!response.ok) {
    window.location.href = "/login.html";
    return;
  }
  renderSidebarFoot(data);
  hydrateSiteBrand();
  await loadHosts();
}

logoutBtn?.addEventListener("click", () => {
  void logout();
});

void boot();
