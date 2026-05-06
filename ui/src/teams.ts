// Teams (collaboration workspaces) page entrypoint. T2 scope:
//   - List teams I'm a member of
//   - Open a team to view metadata + members (read-only)
//   - Edit team name/description/avatar (admin+)
//   - Delete a non-personal team (owner only)
//
// Invitations + member management land in T3. Workspace switcher
// (X-Workspace-Id header) lands in T4.

import {
  createTeam,
  deleteTeam,
  fetchTeam,
  fetchTeamMembers,
  fetchTeams,
  updateTeam,
} from "./api/teams.js";
import { fetchCurrentUser, logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type { TeamMember, TeamRole, TeamWithRole } from "./types/teams.js";

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
  await loadTeams();
  if (teams.length > 0) {
    void openTeam(teams[0].id);
  }
}

void init();
