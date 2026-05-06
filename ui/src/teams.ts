// Teams (collaboration workspaces) page entrypoint. T2 scope:
//   - List teams I'm a member of
//   - Open a team to view metadata + members (read-only)
//   - Edit team name/description/avatar (admin+)
//   - Delete a non-personal team (owner only)
//
// Invitations + member management land in T3. Workspace switcher
// (X-Workspace-Id header) lands in T4.

import {
  acceptInvitation,
  createTeam,
  createTeamInvitation,
  declineInvitation,
  deleteTeam,
  fetchMyInvitations,
  fetchTeam,
  fetchTeamInvitations,
  fetchTeamMembers,
  fetchTeams,
  revokeTeamInvitation,
  updateTeam,
} from "./api/teams.js";
import { fetchCurrentUser, logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type { AdminInvitation, InboxInvitation, TeamMember, TeamRole, TeamWithRole } from "./types/teams.js";

initStoredTheme();
bindThemeSync();

const listEl = byId<HTMLElement>("teamsList");
const emptyEl = byId<HTMLElement>("teamsEmpty");
const panelEl = byId<HTMLElement>("teamsPanel");
const avatarImg = byId<HTMLImageElement>("teamsAvatar");
const avatarPlaceholder = byId<HTMLElement>("teamsAvatarPlaceholder");
const nameEl = byId<HTMLElement>("teamsName");
const kindBadge = byId<HTMLElement>("teamsKindBadge");
const roleBadge = byId<HTMLElement>("teamsRoleBadge");
const slugEl = byId<HTMLElement>("teamsSlug");
const tabButtons = Array.from(document.querySelectorAll<HTMLButtonElement>("[data-teams-tab]"));
const tabPanes = Array.from(document.querySelectorAll<HTMLElement>("[data-teams-pane]"));

const infoNameEl = byId<HTMLInputElement>("teamsInfoName");
const infoDescEl = byId<HTMLTextAreaElement>("teamsInfoDescription");
const infoAvatarEl = byId<HTMLInputElement>("teamsInfoAvatarURL");
const infoSaveBtn = byId<HTMLButtonElement>("teamsInfoSaveBtn");
const infoStatusEl = byId<HTMLElement>("teamsInfoStatus");
const infoReadonlyHintEl = byId<HTMLElement>("teamsInfoReadonlyHint");

const membersListEl = byId<HTMLElement>("teamsMembersList");

const invitationsTabBtn = document.querySelector<HTMLButtonElement>('[data-teams-tab="invitations"]');
const inviteForm = byId<HTMLFormElement>("teamsInviteForm");
const inviteEmailEl = byId<HTMLInputElement>("teamsInviteEmail");
const inviteRoleEl = byId<HTMLSelectElement>("teamsInviteRole");
const inviteSubmitBtn = byId<HTMLButtonElement>("teamsInviteSubmit");
const inviteStatusEl = byId<HTMLElement>("teamsInviteStatus");
const invitationsListEl = byId<HTMLElement>("teamsInvitationsList");

const inboxEl = byId<HTMLElement>("teamsInbox");
const inboxCountEl = byId<HTMLElement>("teamsInboxCount");
const inboxListEl = byId<HTMLElement>("teamsInboxList");

const dangerPersonalEl = byId<HTMLElement>("teamsDangerPersonal");
const dangerTeamEl = byId<HTMLElement>("teamsDangerTeam");
const deleteBtn = byId<HTMLButtonElement>("teamsDeleteBtn");

const createBtn = byId<HTMLButtonElement>("teamsCreateBtn");
const createModal = byId<HTMLElement>("teamsCreateModal");
const createModalCloseBtn = byId<HTMLButtonElement>("teamsCreateModalCloseBtn");
const createForm = byId<HTMLFormElement>("teamsCreateForm");
const createName = byId<HTMLInputElement>("teamsCreateName");
const createDesc = byId<HTMLTextAreaElement>("teamsCreateDescription");
const createAvatar = byId<HTMLInputElement>("teamsCreateAvatarURL");
const createStatusEl = byId<HTMLElement>("teamsCreateStatus");

let teams: TeamWithRole[] = [];
let activeTeamId: string | null = null;
let activeTeam: TeamWithRole | null = null;

const ROLE_LABEL: Record<TeamRole, string> = {
  owner: "Owner",
  admin: "管理员",
  member: "成员",
  viewer: "只读",
};

const KIND_LABEL: Record<string, string> = {
  personal: "个人工作区",
  team: "团队工作区",
};

function setModalOpen(modal: HTMLElement, open: boolean): void {
  modal.classList.toggle("open", open);
  modal.setAttribute("aria-hidden", open ? "false" : "true");
}

function setStatus(el: HTMLElement, msg: string, isError: boolean): void {
  el.textContent = msg;
  el.style.color = isError ? "#c33" : "#3a7";
}

function clearStatus(el: HTMLElement): void {
  el.textContent = "";
  el.style.color = "";
}

function canEdit(role: TeamRole): boolean {
  return role === "owner" || role === "admin";
}

function canDelete(team: TeamWithRole): boolean {
  return team.kind === "team" && team.my_role === "owner";
}

function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function renderTeamList(): void {
  if (teams.length === 0) {
    listEl.innerHTML = `<div class="chat-empty">还没有工作区，点右上角「＋ 新建工作区」创建一个</div>`;
    return;
  }
  listEl.innerHTML = teams
    .map((team) => {
      const active = team.id === activeTeamId ? "active" : "";
      const isPersonal = team.kind === "personal";
      const icon = team.avatar_url
        ? `<img src="${escapeHTML(team.avatar_url)}" alt="" style="width:32px; height:32px; border-radius:8px; object-fit:cover; flex-shrink:0;" />`
        : `<div style="width:32px; height:32px; border-radius:8px; background:linear-gradient(135deg,#e9e9ef,#cfd0d6); display:flex; align-items:center; justify-content:center; font-size:14px; color:#666; flex-shrink:0;">${isPersonal ? "👤" : "⌂"}</div>`;
      return `
        <div class="video-studio-project-item ${active}" data-team-id="${escapeHTML(team.id)}">
          ${icon}
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">${escapeHTML(team.name)}</div>
            <div style="font-size:11px; color:var(--text-muted,#888); display:flex; gap:6px;">
              <span>${KIND_LABEL[team.kind] ?? team.kind}</span>
              <span>·</span>
              <span>${ROLE_LABEL[team.my_role] ?? team.my_role}</span>
            </div>
          </div>
        </div>`;
    })
    .join("");

  listEl.querySelectorAll<HTMLElement>("[data-team-id]").forEach((el) => {
    el.addEventListener("click", () => {
      const id = el.getAttribute("data-team-id");
      if (id) void openTeam(id);
    });
  });
}

function renderTeamPanel(team: TeamWithRole): void {
  emptyEl.hidden = true;
  panelEl.hidden = false;
  nameEl.textContent = team.name;
  slugEl.textContent = team.slug;
  kindBadge.textContent = KIND_LABEL[team.kind] ?? team.kind;
  roleBadge.textContent = ROLE_LABEL[team.my_role] ?? team.my_role;

  if (team.avatar_url) {
    avatarImg.src = team.avatar_url;
    avatarImg.style.display = "block";
    avatarPlaceholder.style.display = "none";
  } else {
    avatarImg.style.display = "none";
    avatarPlaceholder.style.display = "flex";
  }

  // Info tab
  infoNameEl.value = team.name;
  infoDescEl.value = team.description;
  infoAvatarEl.value = team.avatar_url;
  const editable = canEdit(team.my_role);
  infoNameEl.disabled = !editable;
  infoDescEl.disabled = !editable;
  infoAvatarEl.disabled = !editable;
  infoSaveBtn.hidden = !editable;
  infoReadonlyHintEl.hidden = editable;
  clearStatus(infoStatusEl);

  // Invitations tab visibility (admin+, non-personal team only).
  const showInviteTab = canEdit(team.my_role) && team.kind !== "personal";
  if (invitationsTabBtn) invitationsTabBtn.hidden = !showInviteTab;

  // Danger tab
  dangerPersonalEl.hidden = team.kind !== "personal";
  dangerTeamEl.hidden = team.kind === "personal";
  deleteBtn.disabled = !canDelete(team);
  deleteBtn.title = canDelete(team) ? "" : "仅 owner 能删除";
}

function renderMembers(members: TeamMember[]): void {
  if (members.length === 0) {
    membersListEl.innerHTML = `<div class="chat-empty">没有成员</div>`;
    return;
  }
  membersListEl.innerHTML = members
    .map((m) => {
      const initial = (m.username || m.email || "?").charAt(0).toUpperCase();
      const avatar = m.icon_url
        ? `<img src="${escapeHTML(m.icon_url)}" alt="" style="width:36px; height:36px; border-radius:50%; object-fit:cover; flex-shrink:0;" />`
        : `<div style="width:36px; height:36px; border-radius:50%; background:linear-gradient(135deg,#e9e9ef,#cfd0d6); display:flex; align-items:center; justify-content:center; font-size:14px; color:#666; flex-shrink:0;">${escapeHTML(initial)}</div>`;
      return `
        <div class="video-studio-project-item" style="cursor:default;">
          ${avatar}
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500;">${escapeHTML(m.username || "(no name)")}</div>
            <div style="font-size:11px; color:var(--text-muted,#888); overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">${escapeHTML(m.email)}</div>
          </div>
          <span class="settings-value-pill" style="font-size:11px;">${ROLE_LABEL[m.role] ?? m.role}</span>
        </div>`;
    })
    .join("");
}

function setActiveTab(name: string): void {
  tabButtons.forEach((btn) => {
    btn.classList.toggle("active", btn.getAttribute("data-teams-tab") === name);
  });
  tabPanes.forEach((pane) => {
    pane.hidden = pane.getAttribute("data-teams-pane") !== name;
  });
  if (name === "invitations" && activeTeam) {
    void loadInvitations(activeTeam.id);
  }
}

async function loadTeams(): Promise<void> {
  const { response, data } = await fetchTeams();
  if (!response.ok) {
    listEl.innerHTML = `<div class="chat-empty">加载失败</div>`;
    return;
  }
  teams = data.teams;
  renderTeamList();
}

async function openTeam(id: string): Promise<void> {
  activeTeamId = id;
  renderTeamList();
  setActiveTab("info");
  membersListEl.innerHTML = `<div class="chat-empty">加载中...</div>`;

  const { response, data } = await fetchTeam(id);
  if (!response.ok) {
    activeTeam = null;
    emptyEl.hidden = false;
    panelEl.hidden = true;
    return;
  }
  activeTeam = { ...data.team, my_role: data.my_role };
  renderTeamPanel(activeTeam);
  void loadMembers(id);
}

async function loadMembers(id: string): Promise<void> {
  const { response, data } = await fetchTeamMembers(id);
  if (!response.ok) {
    membersListEl.innerHTML = `<div class="chat-empty">加载失败</div>`;
    return;
  }
  renderMembers(data.members);
}

// ---- invitations (admin tab) ------------------------------------

function renderInvitations(invs: AdminInvitation[]): void {
  if (invs.length === 0) {
    invitationsListEl.innerHTML = `<div class="chat-empty">没有待处理的邀请</div>`;
    return;
  }
  invitationsListEl.innerHTML = invs
    .map((inv) => {
      const expires = new Date(inv.expires_at).toLocaleString();
      return `
        <div class="video-studio-project-item" style="cursor:default; flex-direction:column; align-items:stretch; gap:6px;">
          <div style="display:flex; align-items:center; gap:8px;">
            <div style="flex:1; min-width:0;">
              <div style="font-weight:500; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">${escapeHTML(inv.email)}</div>
              <div style="font-size:11px; color:var(--text-muted,#888);">
                ${ROLE_LABEL[inv.role] ?? inv.role} · 由 ${escapeHTML(inv.inviter_name)} 发出 · 到期 ${escapeHTML(expires)}
              </div>
            </div>
            <button type="button" class="btn-inline btn-secondary" data-invite-copy="${escapeHTML(inv.invite_url)}">复制链接</button>
            <button type="button" class="btn-inline" data-invite-revoke="${escapeHTML(inv.id)}" style="color:#c33;">撤销</button>
          </div>
        </div>`;
    })
    .join("");
  invitationsListEl.querySelectorAll<HTMLButtonElement>("[data-invite-copy]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const url = btn.getAttribute("data-invite-copy") || "";
      try {
        await navigator.clipboard.writeText(url);
        const orig = btn.textContent;
        btn.textContent = "已复制";
        setTimeout(() => {
          btn.textContent = orig;
        }, 1200);
      } catch {
        window.prompt("复制下面这个链接：", url);
      }
    });
  });
  invitationsListEl.querySelectorAll<HTMLButtonElement>("[data-invite-revoke]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const id = btn.getAttribute("data-invite-revoke");
      if (id) void doRevoke(id);
    });
  });
}

async function loadInvitations(teamID: string): Promise<void> {
  invitationsListEl.innerHTML = `<div class="chat-empty">加载中...</div>`;
  const { response, data } = await fetchTeamInvitations(teamID);
  if (!response.ok) {
    invitationsListEl.innerHTML = `<div class="chat-empty">加载失败</div>`;
    return;
  }
  renderInvitations(data.invitations);
}

async function submitInvite(e: Event): Promise<void> {
  e.preventDefault();
  if (!activeTeam) return;
  const email = inviteEmailEl.value.trim();
  const role = inviteRoleEl.value as TeamRole;
  if (!email) {
    setStatus(inviteStatusEl, "请填写邮箱", true);
    return;
  }
  inviteSubmitBtn.disabled = true;
  setStatus(inviteStatusEl, "发送中...", false);
  const { response, data } = await createTeamInvitation(activeTeam.id, { email, role });
  inviteSubmitBtn.disabled = false;
  if (!response.ok) {
    const err = (data as unknown as { error?: string })?.error || "发送失败";
    setStatus(inviteStatusEl, err, true);
    return;
  }
  inviteEmailEl.value = "";
  if (data.email_send_disabled) {
    setStatus(inviteStatusEl, "邀请已创建，但服务器未配置邮件通道。请手动复制链接发给对方。", false);
  } else if (!data.email_sent) {
    setStatus(inviteStatusEl, "邀请已创建，但邮件发送失败。请用「复制链接」手动转发。", true);
  } else {
    setStatus(inviteStatusEl, "邀请邮件已发送", false);
  }
  await loadInvitations(activeTeam.id);
}

async function doRevoke(invID: string): Promise<void> {
  if (!activeTeam) return;
  if (!window.confirm("确认撤销该邀请？对方点击邀请链接时将看到「邀请已失效」。")) return;
  const { response } = await revokeTeamInvitation(activeTeam.id, invID);
  if (!response.ok) {
    window.alert("撤销失败");
    return;
  }
  await loadInvitations(activeTeam.id);
}

// ---- inbox ---------------------------------------------------------

function renderInbox(invs: InboxInvitation[]): void {
  if (invs.length === 0) {
    inboxEl.hidden = true;
    return;
  }
  inboxEl.hidden = false;
  inboxCountEl.textContent = String(invs.length);
  inboxListEl.innerHTML = invs
    .map((inv) => {
      const expires = new Date(inv.expires_at).toLocaleString();
      return `
        <div style="display:flex; align-items:center; gap:8px; padding:8px 10px; background:#fff; border:1px solid #f0d597; border-radius:6px;">
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500; font-size:13px;">${escapeHTML(inv.team_name)} <span style="font-size:11px; color:var(--text-muted,#888);">· ${ROLE_LABEL[inv.role] ?? inv.role}</span></div>
            <div style="font-size:11px; color:var(--text-muted,#888);">${escapeHTML(inv.inviter_name)} 邀请你 · 到期 ${escapeHTML(expires)}</div>
          </div>
          <button type="button" class="btn-inline btn-secondary" data-inbox-accept="${escapeHTML(inv.token)}">接受</button>
          <button type="button" class="btn-inline" data-inbox-decline="${escapeHTML(inv.token)}" style="color:#c33;">拒绝</button>
        </div>`;
    })
    .join("");
  inboxListEl.querySelectorAll<HTMLButtonElement>("[data-inbox-accept]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const token = btn.getAttribute("data-inbox-accept");
      if (token) void doInboxAccept(token);
    });
  });
  inboxListEl.querySelectorAll<HTMLButtonElement>("[data-inbox-decline]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const token = btn.getAttribute("data-inbox-decline");
      if (token) void doInboxDecline(token);
    });
  });
}

async function loadInbox(): Promise<void> {
  const { response, data } = await fetchMyInvitations();
  if (!response.ok) return;
  renderInbox(data.invitations);
}

async function doInboxAccept(token: string): Promise<void> {
  const { response, data } = await acceptInvitation(token);
  if (!response.ok) {
    window.alert(data?.error || "接受失败");
    return;
  }
  await Promise.all([loadInbox(), loadTeams()]);
  if (data?.team_id) void openTeam(data.team_id);
}

async function doInboxDecline(token: string): Promise<void> {
  if (!window.confirm("确认拒绝该邀请？")) return;
  const { response, data } = await declineInvitation(token);
  if (!response.ok) {
    window.alert(data?.error || "拒绝失败");
    return;
  }
  await loadInbox();
}

async function saveInfo(): Promise<void> {
  if (!activeTeam || !canEdit(activeTeam.my_role)) return;
  const name = infoNameEl.value.trim();
  if (!name) {
    setStatus(infoStatusEl, "名称不能为空", true);
    return;
  }
  infoSaveBtn.disabled = true;
  setStatus(infoStatusEl, "保存中...", false);
  try {
    const { response, data } = await updateTeam(activeTeam.id, {
      name,
      description: infoDescEl.value.trim(),
      avatar_url: infoAvatarEl.value.trim(),
    });
    if (!response.ok) {
      const err = (data as { error?: string })?.error || "保存失败";
      setStatus(infoStatusEl, err, true);
      return;
    }
    activeTeam = { ...data.team, my_role: activeTeam.my_role };
    // Reflect into list view too.
    teams = teams.map((t) => (t.id === activeTeam!.id ? { ...activeTeam! } : t));
    renderTeamList();
    renderTeamPanel(activeTeam);
    setStatus(infoStatusEl, "已保存", false);
  } finally {
    infoSaveBtn.disabled = false;
  }
}

async function doDelete(): Promise<void> {
  if (!activeTeam || !canDelete(activeTeam)) return;
  const name = activeTeam.name;
  const confirmed = window.confirm(`确认删除工作区「${name}」？该操作不可恢复。`);
  if (!confirmed) return;
  const { response, data } = await deleteTeam(activeTeam.id);
  if (!response.ok) {
    const err = (data as unknown as { error?: string })?.error || "删除失败";
    window.alert(err);
    return;
  }
  // Remove from list, jump back to empty / first remaining team.
  teams = teams.filter((t) => t.id !== activeTeam!.id);
  activeTeam = null;
  activeTeamId = null;
  emptyEl.hidden = false;
  panelEl.hidden = true;
  renderTeamList();
  if (teams.length > 0) {
    void openTeam(teams[0].id);
  }
}

function openCreateModal(): void {
  createName.value = "";
  createDesc.value = "";
  createAvatar.value = "";
  clearStatus(createStatusEl);
  setModalOpen(createModal, true);
  setTimeout(() => createName.focus(), 0);
}

function closeCreateModal(): void {
  setModalOpen(createModal, false);
}

async function submitCreate(e: Event): Promise<void> {
  e.preventDefault();
  const name = createName.value.trim();
  if (!name) {
    setStatus(createStatusEl, "名称不能为空", true);
    return;
  }
  setStatus(createStatusEl, "创建中...", false);
  const { response, data } = await createTeam({
    name,
    description: createDesc.value.trim(),
    avatar_url: createAvatar.value.trim(),
  });
  if (!response.ok) {
    const err = (data as unknown as { error?: string })?.error || "创建失败";
    setStatus(createStatusEl, err, true);
    return;
  }
  closeCreateModal();
  await loadTeams();
  void openTeam(data.team.id);
}

// ---- wiring -------------------------------------------------------

tabButtons.forEach((btn) => {
  btn.addEventListener("click", () => {
    const name = btn.getAttribute("data-teams-tab");
    if (name) setActiveTab(name);
  });
});
infoSaveBtn.addEventListener("click", () => void saveInfo());
deleteBtn.addEventListener("click", () => void doDelete());
createBtn.addEventListener("click", openCreateModal);
createModalCloseBtn.addEventListener("click", closeCreateModal);
createModal.addEventListener("click", (e) => {
  const t = e.target as HTMLElement;
  if (t.classList.contains("modal-backdrop")) closeCreateModal();
});
createForm.addEventListener("submit", (e) => void submitCreate(e));
inviteForm.addEventListener("submit", (e) => void submitInvite(e));

document.getElementById("logoutBtn")?.addEventListener("click", async () => {
  try {
    await logout();
  } finally {
    window.location.replace("/login.html");
  }
});

// ---- bootstrap ----------------------------------------------------

async function init(): Promise<void> {
  await hydrateSiteBrand();
  const { response, data } = await fetchCurrentUser();
  if (!response.ok) {
    window.location.href = "/login.html";
    return;
  }
  renderSidebarFoot(data);
  await Promise.all([loadTeams(), loadInbox()]);
  if (teams.length > 0) {
    void openTeam(teams[0].id);
  }
}

void init();
