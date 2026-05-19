// /rooms.html — qunliao P0c-2 multi-participant chat surface.
//
// Three columns: room list (left), messages (center), members (right).
// Reuses the existing /ws/chat WebSocket; filters on event.type ===
// "room_message". Sits alongside the 1:1 /chat.html surface; the two
// don't touch each other's state.

import {
  addRoomMember,
  archiveRoom,
  createRoom,
  fetchRoomMembers,
  fetchRoomMessages,
  fetchRooms,
  removeRoomMember,
  sendRoomMessage,
} from "./api/rooms.js";
import { logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSidebarFoot, hydrateSiteBrand } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type { Room, RoomMember, RoomMessage } from "./types/room.js";

initStoredTheme();
bindThemeSync();

// ---- DOM refs ----
const roomListEl = byId<HTMLUListElement>("roomList");
const roomCountEl = byId<HTMLElement>("roomCount");
const roomEmptyEl = byId<HTMLElement>("roomEmpty");
const roomLoadingEl = byId<HTMLElement>("roomLoading");
const roomRefreshBtn = byId<HTMLButtonElement>("roomRefreshBtn");
const newRoomBtn = byId<HTMLButtonElement>("newRoomBtn");

const activeRoomTitleEl = byId<HTMLElement>("activeRoomTitle");
const wsStatusDotEl = byId<HTMLElement>("wsStatusDot");
const messageScrollEl = byId<HTMLElement>("messageScroll");
const messageListEl = byId<HTMLElement>("messageList");
const sendForm = byId<HTMLFormElement>("sendForm");
const sendInput = byId<HTMLInputElement>("sendInput");
const sendBtn = byId<HTMLButtonElement>("sendBtn");

const memberListEl = byId<HTMLUListElement>("memberList");
const memberCountEl = byId<HTMLElement>("memberCount");
const memberEmptyEl = byId<HTMLElement>("memberEmpty");
const addMemberBtn = byId<HTMLButtonElement>("addMemberBtn");

const newRoomModal = byId<HTMLElement>("newRoomModal");
const newRoomCloseBtn = byId<HTMLButtonElement>("newRoomCloseBtn");
const newRoomCancelBtn = byId<HTMLButtonElement>("newRoomCancelBtn");
const newRoomSubmitBtn = byId<HTMLButtonElement>("newRoomSubmitBtn");
const newRoomNameInput = byId<HTMLInputElement>("newRoomName");
const newRoomTopicInput = byId<HTMLInputElement>("newRoomTopic");
const newRoomErrorEl = byId<HTMLElement>("newRoomError");
const logoutBtn = byId<HTMLButtonElement>("logoutBtn");

// ---- state ----
let rooms: Room[] = [];
let activeRoomId: number | null = null;
let activeMessages: RoomMessage[] = [];
let activeMembers: RoomMember[] = [];

function escapeHTML(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
}

// ---- room list ----
async function loadRooms(): Promise<void> {
  roomLoadingEl.hidden = false;
  roomEmptyEl.hidden = true;
  try {
    const { data } = await fetchRooms(false);
    rooms = data.rooms ?? [];
    renderRoomList();
  } catch (err) {
    roomListEl.innerHTML = "";
    roomEmptyEl.hidden = false;
    roomEmptyEl.textContent = `加载失败：${(err as Error).message}`;
  } finally {
    roomLoadingEl.hidden = true;
  }
}

function renderRoomList(): void {
  roomCountEl.textContent = `${rooms.length}`;
  roomListEl.innerHTML = "";
  if (rooms.length === 0) {
    roomEmptyEl.hidden = false;
    return;
  }
  roomEmptyEl.hidden = true;
  rooms.forEach((r) => {
    const li = document.createElement("li");
    li.style.cssText = "padding:8px 10px; cursor:pointer; border-radius:6px; margin-bottom:2px;";
    if (r.id === activeRoomId) {
      li.style.background = "var(--accent-soft, #eef)";
    }
    const name = r.name || `房间 #${r.id}`;
    li.innerHTML = `<div style="font-weight:500;">${escapeHTML(name)}</div>` +
      (r.topic ? `<div class="meta-subtitle" style="font-size:11px;">${escapeHTML(r.topic)}</div>` : "");
    li.addEventListener("click", () => void switchRoom(r.id));
    roomListEl.appendChild(li);
  });
}

async function switchRoom(roomId: number): Promise<void> {
  activeRoomId = roomId;
  const r = rooms.find((x) => x.id === roomId);
  activeRoomTitleEl.textContent = r ? (r.name || `房间 #${r.id}`) : `房间 #${roomId}`;
  sendInput.disabled = false;
  sendBtn.disabled = false;
  addMemberBtn.disabled = false;
  renderRoomList();
  await Promise.all([loadMessages(roomId), loadMembers(roomId)]);
}

// ---- messages ----
async function loadMessages(roomId: number): Promise<void> {
  messageListEl.innerHTML = '<div class="meta-subtitle">加载中...</div>';
  try {
    const { data } = await fetchRoomMessages(roomId, { limit: 100 });
    // Backend returns newest-first; flip to oldest-first for display.
    activeMessages = (data.messages ?? []).slice().reverse();
    renderMessages();
  } catch (err) {
    messageListEl.innerHTML = `<div class="meta-subtitle" style="color:var(--danger,#c00);">加载失败：${escapeHTML((err as Error).message)}</div>`;
  }
}

function renderMessages(): void {
  messageListEl.innerHTML = activeMessages
    .map((m) => {
      const who = escapeHTML(m.sender_username || m.sender_id || "?");
      const when = new Date(m.created_at).toLocaleString();
      const body = escapeHTML(m.content || "");
      return `<div style="padding:6px 8px; border-bottom:1px solid var(--border,#eee);">` +
        `<div style="font-size:11px; color:var(--meta,#888);">${who} · ${when}</div>` +
        `<div style="margin-top:2px; white-space:pre-wrap;">${body}</div></div>`;
    })
    .join("");
  // Scroll to bottom.
  messageScrollEl.scrollTop = messageScrollEl.scrollHeight;
}

function upsertMessage(m: RoomMessage): boolean {
  if (m.room_id !== activeRoomId) return false;
  if (activeMessages.some((x) => x.id === m.id)) return false;
  activeMessages.push(m);
  return true;
}

// ---- send ----
sendForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!activeRoomId) return;
  const text = sendInput.value.trim();
  if (!text) return;
  sendInput.disabled = true;
  sendBtn.disabled = true;
  try {
    await sendRoomMessage(activeRoomId, text);
    sendInput.value = "";
    // The WS event will push the row into activeMessages; if WS is
    // down, reload the list manually.
    if (!wsConnected) {
      await loadMessages(activeRoomId);
    }
  } catch (err) {
    alert(`发送失败：${(err as Error).message}`);
  } finally {
    sendInput.disabled = false;
    sendBtn.disabled = false;
    sendInput.focus();
  }
});

// ---- members ----
async function loadMembers(roomId: number): Promise<void> {
  memberListEl.innerHTML = "";
  memberEmptyEl.hidden = true;
  try {
    const { data } = await fetchRoomMembers(roomId);
    activeMembers = data.members ?? [];
    renderMembers();
  } catch (err) {
    memberListEl.innerHTML = `<div class="meta-subtitle" style="color:var(--danger,#c00);">${escapeHTML((err as Error).message)}</div>`;
  }
}

function renderMembers(): void {
  memberCountEl.textContent = `${activeMembers.length}`;
  if (activeMembers.length === 0) {
    memberEmptyEl.hidden = false;
    memberEmptyEl.textContent = "暂无成员";
    return;
  }
  memberEmptyEl.hidden = true;
  memberListEl.innerHTML = "";
  activeMembers.forEach((m) => {
    const li = document.createElement("li");
    li.style.cssText = "padding:6px 10px; display:flex; align-items:center; gap:6px; border-bottom:1px solid var(--border,#eee);";
    const roleBadge = m.role === "owner" ? "👑" : m.role === "observer" ? "👁" : "";
    li.innerHTML = `<span style="flex:1;">${roleBadge} ${escapeHTML(m.user_id)}</span>` +
      `<button class="btn-inline btn-secondary" data-remove="${escapeHTML(m.user_id)}" title="移除" style="padding:2px 6px;">✕</button>`;
    li.querySelector("[data-remove]")?.addEventListener("click", async () => {
      if (!activeRoomId) return;
      if (!confirm(`移除 ${m.user_id}？`)) return;
      try {
        await removeRoomMember(activeRoomId, m.user_id);
        await loadMembers(activeRoomId);
      } catch (err) {
        alert(`移除失败：${(err as Error).message}`);
      }
    });
    memberListEl.appendChild(li);
  });
}

addMemberBtn.addEventListener("click", async () => {
  if (!activeRoomId) return;
  const userId = prompt("user_id（成员或 bot 的 users.id）：");
  if (!userId) return;
  const role = (prompt("role: owner / member / observer", "member") || "member").trim();
  try {
    await addRoomMember(activeRoomId, userId.trim(), role as "owner" | "member" | "observer");
    await loadMembers(activeRoomId);
  } catch (err) {
    alert(`添加失败：${(err as Error).message}`);
  }
});

// ---- new room modal ----
newRoomBtn.addEventListener("click", () => {
  newRoomNameInput.value = "";
  newRoomTopicInput.value = "";
  newRoomErrorEl.textContent = "";
  newRoomModal.hidden = false;
  newRoomNameInput.focus();
});
newRoomCloseBtn.addEventListener("click", () => (newRoomModal.hidden = true));
newRoomCancelBtn.addEventListener("click", () => (newRoomModal.hidden = true));
newRoomSubmitBtn.addEventListener("click", async () => {
  const name = newRoomNameInput.value.trim();
  if (!name) {
    newRoomErrorEl.textContent = "请填名称";
    return;
  }
  newRoomSubmitBtn.disabled = true;
  try {
    const { data } = await createRoom({
      kind: "room",
      name,
      topic: newRoomTopicInput.value.trim(),
    });
    newRoomModal.hidden = true;
    await loadRooms();
    if (data.room?.id) await switchRoom(data.room.id);
  } catch (err) {
    newRoomErrorEl.textContent = `创建失败：${(err as Error).message}`;
  } finally {
    newRoomSubmitBtn.disabled = false;
  }
});

// ---- WebSocket ----
let ws: WebSocket | null = null;
let wsConnected = false;
let pollTimer: number | null = null;

function getWebSocketUrl(): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}/ws/chat`;
}

function setWSStatus(ok: boolean): void {
  wsConnected = ok;
  if (wsStatusDotEl) {
    wsStatusDotEl.style.color = ok ? "#10b981" : "#ef4444";
    wsStatusDotEl.title = ok ? "WS connected" : "WS disconnected (polling fallback)";
  }
}

function startPolling(): void {
  if (pollTimer != null) return;
  pollTimer = window.setInterval(() => {
    if (activeRoomId) void loadMessages(activeRoomId);
  }, 5000);
}
function stopPolling(): void {
  if (pollTimer != null) {
    window.clearInterval(pollTimer);
    pollTimer = null;
  }
}

function connectWS(): void {
  try {
    ws = new WebSocket(getWebSocketUrl());
  } catch {
    setWSStatus(false);
    startPolling();
    return;
  }
  ws.addEventListener("open", () => {
    setWSStatus(true);
    stopPolling();
  });
  ws.addEventListener("close", () => {
    setWSStatus(false);
    startPolling();
    // Auto-reconnect after a beat. Avoids tight loops if upstream is
    // genuinely down — 5s matches what chat.ts uses.
    setTimeout(connectWS, 5000);
  });
  ws.addEventListener("error", () => {
    setWSStatus(false);
  });
  ws.addEventListener("message", (event) => {
    let payload: { type?: string; room_id?: number; message?: RoomMessage };
    try {
      payload = JSON.parse(event.data);
    } catch {
      return;
    }
    if (payload?.type !== "room_message") return;
    if (!payload.message) return;
    if (upsertMessage(payload.message)) {
      renderMessages();
    }
  });
}

// ---- bootstrap ----
roomRefreshBtn.addEventListener("click", () => void loadRooms());
logoutBtn?.addEventListener("click", () => {
  void logout();
});

(async () => {
  hydrateSiteBrand();
  hydrateSidebarFoot();
  await loadRooms();
  connectWS();
})();
