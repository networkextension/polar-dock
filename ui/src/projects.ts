// Projects page — phase 1.2 of the AI-driven workflow.
//
// One page covers the full pre-coding pipeline:
//   create project → write description → AI decompose into features →
//   user picks which features → generate plan → tasks list shows up.
//
// Per-task chat thread (1:1) wiring + bot pickup come in phase 1.3.

import { fetchAvailableLLMConfigs, fetchBotUsers } from "./api/dashboard.js";
import {
  bulkUpdateProjectTaskStatus,
  createAgentToken,
  createProject,
  decomposeProject,
  deleteProject,
  fetchAgentBotStatus,
  fetchAgentTokens,
  fetchProject,
  fetchProjects,
  generateProjectPlan,
  pickupProjectTask,
  revokeAgentToken,
  updateProject,
  updateProjectFeature,
  updateProjectTask,
} from "./api/projects.js";
import { fetchCurrentUser, logout } from "./api/session.js";
import type { BotUser } from "./types/dashboard.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type {
  AgentToken,
  FeatureComplexity,
  Project,
  ProjectFeature,
  ProjectStatus,
  ProjectTask,
  TaskStatus,
} from "./types/projects.js";

initStoredTheme();
bindThemeSync();

const listEl = byId<HTMLElement>("projectsList");
const emptyEl = byId<HTMLElement>("projectsEmpty");
const panelEl = byId<HTMLElement>("projectsPanel");
const filterSearchEl = byId<HTMLInputElement>("projectsFilterSearch");
const filterStatusEl = byId<HTMLSelectElement>("projectsFilterStatus");
const filterCodeProviderEl = byId<HTMLSelectElement>("projectsFilterCodeProvider");
const filterCountEl = byId<HTMLElement>("projectsFilterCount");

const nameEl = byId<HTMLElement>("projectsName");
const statusPillEl = byId<HTMLElement>("projectsStatusPill");
const createdAtEl = byId<HTMLElement>("projectsCreatedAt");
const deleteBtn = byId<HTMLButtonElement>("projectsDeleteBtn");

const descEl = byId<HTMLTextAreaElement>("projectsDescription");
const saveDescBtn = byId<HTMLButtonElement>("projectsSaveDescBtn");
const gitRemoteUrlEl = byId<HTMLInputElement>("projectsGitRemoteUrl");
const saveGitRemoteBtn = byId<HTMLButtonElement>("projectsSaveGitRemoteBtn");
const decomposeBtn = byId<HTMLButtonElement>("projectsDecomposeBtn");
const decomposeStatusEl = byId<HTMLElement>("projectsDecomposeStatus");
const decomposeLLMSelect = byId<HTMLSelectElement>("projectsDecomposeLLMSelect");
const decomposeBotSelect = byId<HTMLSelectElement>("projectsDecomposeBotSelect");

const featuresSection = byId<HTMLElement>("projectsFeaturesSection");
const featuresListEl = byId<HTMLElement>("projectsFeaturesList");
const featuresCountEl = byId<HTMLElement>("projectsFeaturesCount");
const generatePlanBtn = byId<HTMLButtonElement>("projectsGeneratePlanBtn");
const planStatusEl = byId<HTMLElement>("projectsPlanStatus");

const tasksSection = byId<HTMLElement>("projectsTasksSection");
const tasksListEl = byId<HTMLElement>("projectsTasksList");
const tasksCountEl = byId<HTMLElement>("projectsTasksCount");
const featuresCollapseBtn = byId<HTMLButtonElement>("projectsFeaturesCollapseBtn");
const featuresBodyEl = byId<HTMLElement>("projectsFeaturesBody");
const tasksCollapseBtn = byId<HTMLButtonElement>("projectsTasksCollapseBtn");
const dispatchAllBotSelect = byId<HTMLSelectElement>("projectsDispatchAllBot");
const dispatchAllBtn = byId<HTMLButtonElement>("projectsDispatchAllBtn");
const finishAllBtn = byId<HTMLButtonElement>("projectsFinishAllBtn");
const cancelAllBtn = byId<HTMLButtonElement>("projectsCancelAllBtn");
const dispatchAllStatusEl = byId<HTMLElement>("projectsDispatchAllStatus");

const newBtn = byId<HTMLButtonElement>("projectsNewBtn");
const agentTokensBtn = byId<HTMLButtonElement>("projectsAgentTokensBtn");
const agentTokensModal = byId<HTMLElement>("agentTokensModal");
const agentTokensModalCloseBtn = byId<HTMLButtonElement>("agentTokensModalCloseBtn");
const agentTokenNameInput = byId<HTMLInputElement>("agentTokenName");
const agentTokenCreateBtn = byId<HTMLButtonElement>("agentTokenCreateBtn");
const agentTokenJustCreatedEl = byId<HTMLElement>("agentTokenJustCreated");
const agentTokenJustCreatedRawEl = byId<HTMLElement>("agentTokenJustCreatedRaw");
const agentTokenCopyBtn = byId<HTMLButtonElement>("agentTokenCopyBtn");
const agentTokensListEl = byId<HTMLElement>("agentTokensList");
const createModal = byId<HTMLElement>("projectsCreateModal");
const createModalCloseBtn = byId<HTMLButtonElement>("projectsCreateModalCloseBtn");
const createForm = byId<HTMLFormElement>("projectsCreateForm");
const createName = byId<HTMLInputElement>("projectsCreateName");
const createDesc = byId<HTMLTextAreaElement>("projectsCreateDescription");
const createRuntimeEnv = byId<HTMLSelectElement>("projectsCreateRuntimeEnv");
const createBusinessScenario = byId<HTMLSelectElement>("projectsCreateBusinessScenario");
const createCodeProvider = byId<HTMLSelectElement>("projectsCreateCodeProvider");
const createGitRemoteUrl = byId<HTMLInputElement>("projectsCreateGitRemoteUrl");
const createStatusEl = byId<HTMLElement>("projectsCreateStatus");

const RUNTIME_ENV_LABELS: Record<string, string> = {
  ios_swiftui: "iOS · SwiftUI",
  ios_uikit: "iOS · UIKit",
  macos_native: "macOS",
  web_static: "Web 静态",
  web_react: "Web React",
  web_nextjs: "Web Next.js",
  cli_tool: "CLI 工具",
  chrome_ext: "Chrome 扩展",
};

const BUSINESS_SCENARIO_LABELS: Record<string, string> = {
  creative_game: "创意小游戏",
  content_app: "文学/历史/教育内容",
  productivity: "生产力工具",
  social: "社交互动",
  data_viz: "数据可视化",
  commerce: "商业/交易",
  media: "媒体内容",
};

let projects: Project[] = [];
let activeProjectID: string | null = null;
let activeProject: Project | null = null;
let activeFeatures: ProjectFeature[] = [];
let activeTasks: ProjectTask[] = [];
let availableBots: BotUser[] = [];
let pickerOpenForTaskID: string | null = null;
// botToolMap caches each bot's currently-attached agent tool name
// (kimi/claude/codex/...) so the pickup picker can highlight bots
// matching the active project's code_provider. Empty string means
// "no agent attached" or "tool-loop mode".
let botToolMap: Record<string, string> = {};

const STATUS_LABEL: Record<ProjectStatus, string> = {
  draft: "草稿",
  planning: "规划中",
  building: "开发中",
  beta: "外测中",
  shipped: "已发布",
  archived: "已归档",
};

const COMPLEXITY_LABEL: Record<FeatureComplexity, string> = {
  simple: "简单",
  medium: "中等",
  complex: "复杂",
};

const COMPLEXITY_COLOR: Record<FeatureComplexity, string> = {
  simple: "#3a7",
  medium: "#c80",
  complex: "#c33",
};

const TASK_STATUS_LABEL: Record<TaskStatus, string> = {
  todo: "待办",
  in_progress: "进行中",
  review: "待审",
  done: "完成",
  blocked: "阻塞",
};

const TASK_STATUS_OPTIONS: TaskStatus[] = ["todo", "in_progress", "review", "done", "blocked"];

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

function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

// ---- list rendering -----------------------------------------------

// applyProjectFilter applies the search + status + code_provider
// filters to the loaded `projects` array. Pure client-side; the
// list endpoint returns everything in the workspace and the
// filter row narrows the visible subset.
function applyProjectFilter(): Project[] {
  const search = filterSearchEl.value.trim().toLowerCase();
  const status = filterStatusEl.value;
  const provider = filterCodeProviderEl.value;
  return projects.filter((p) => {
    if (search && !p.name.toLowerCase().includes(search)) {
      return false;
    }
    if (status && p.status !== status) {
      return false;
    }
    if (provider) {
      // Special sentinel: "__none__" matches projects with no
      // code_provider set (empty string).
      if (provider === "__none__" && p.code_provider !== "") {
        return false;
      }
      if (provider !== "__none__" && p.code_provider !== provider) {
        return false;
      }
    }
    return true;
  });
}

function renderProjectList(): void {
  if (projects.length === 0) {
    filterCountEl.textContent = "";
    listEl.innerHTML = `<div class="chat-empty">还没有项目，点右上角「＋ 新建项目」开始</div>`;
    return;
  }
  const filtered = applyProjectFilter();
  filterCountEl.textContent = filtered.length === projects.length
    ? `${projects.length}`
    : `${filtered.length}/${projects.length}`;
  if (filtered.length === 0) {
    listEl.innerHTML = `<div class="chat-empty">没有符合筛选条件的项目</div>`;
    return;
  }
  listEl.innerHTML = filtered
    .map((p) => {
      const active = p.id === activeProjectID ? "active" : "";
      const providerTag = p.code_provider
        ? ` · <span style="color:#3a7;">${escapeHTML(p.code_provider)}</span>`
        : "";
      return `
        <div class="video-studio-project-item ${active}" data-project-id="${escapeHTML(p.id)}">
          <div style="width:32px; height:32px; border-radius:8px; background:linear-gradient(135deg,#e9e9ef,#cfd0d6); display:flex; align-items:center; justify-content:center; font-size:14px; color:#666; flex-shrink:0;">✦</div>
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">${escapeHTML(p.name)}</div>
            <div style="font-size:11px; color:var(--text-muted,#888);">${STATUS_LABEL[p.status] ?? p.status}${providerTag}</div>
          </div>
        </div>`;
    })
    .join("");
  listEl.querySelectorAll<HTMLElement>("[data-project-id]").forEach((el) => {
    el.addEventListener("click", () => {
      const id = el.getAttribute("data-project-id");
      if (id) void openProject(id);
    });
  });
}

function renderProjectPanel(): void {
  if (!activeProject) {
    emptyEl.hidden = false;
    panelEl.hidden = true;
    return;
  }
  emptyEl.hidden = true;
  panelEl.hidden = false;
  nameEl.textContent = activeProject.name;
  // Project status pill plus the (optional) constraint badges so
  // the user always sees what stack + scenario is locking the AI
  // 拆解 — these are the things that make the difference between
  // "useful feature list" and "generic SaaS plan".
  const constraintChips: string[] = [];
  const env = activeProject.runtime_env;
  const scenario = activeProject.business_scenario;
  const codeProvider = activeProject.code_provider;
  if (env && RUNTIME_ENV_LABELS[env]) {
    constraintChips.push(`<span class="tag-chip" title="运行环境">${RUNTIME_ENV_LABELS[env]}</span>`);
  }
  if (scenario && BUSINESS_SCENARIO_LABELS[scenario]) {
    constraintChips.push(`<span class="tag-chip" title="业务场景">${BUSINESS_SCENARIO_LABELS[scenario]}</span>`);
  }
  if (codeProvider) {
    constraintChips.push(`<span class="tag-chip tag-chip-accent" title="首选编码工具">🤖 ${escapeHTML(codeProvider)}</span>`);
  }
  statusPillEl.innerHTML = `${STATUS_LABEL[activeProject.status] ?? activeProject.status} ${constraintChips.join(" ")}`;
  createdAtEl.textContent = `创建于 ${new Date(activeProject.created_at).toLocaleString()}`;
  descEl.value = activeProject.description;
  gitRemoteUrlEl.value = activeProject.git_remote_url || "";
  clearStatus(decomposeStatusEl);
  clearStatus(planStatusEl);
  // Decompose button text reflects whether this is the first
  // decompose (sprint=0) or an iteration (current_sprint > 0).
  const sprint = activeProject.current_sprint || 0;
  decomposeBtn.textContent = sprint === 0
    ? "AI 拆解功能 →"
    : `AI 拆解 → Sprint ${sprint + 1} →`;

  renderFeatures();
  renderTasks();
}

// groupBySprint splits a sorted-by-sprint slice into [sprint, items[]].
// Pre-condition: items already sorted by sprint_number ASC then ord
// (which is what listProjectFeatures / listProjectTasks return).
function groupBySprint<T extends { sprint_number: number }>(items: T[]): Array<[number, T[]]> {
  const groups: Array<[number, T[]]> = [];
  for (const item of items) {
    const sprint = item.sprint_number || 1;
    const last = groups[groups.length - 1];
    if (last && last[0] === sprint) {
      last[1].push(item);
    } else {
      groups.push([sprint, [item]]);
    }
  }
  return groups;
}

function renderFeatures(): void {
  if (activeFeatures.length === 0) {
    featuresSection.hidden = true;
    return;
  }
  featuresSection.hidden = false;
  const selectedCount = activeFeatures.filter((f) => f.selected).length;
  featuresCountEl.textContent = `${selectedCount} / ${activeFeatures.length} 已选`;
  generatePlanBtn.disabled = selectedCount === 0;

  // Group by sprint so each iteration is a labeled section. Single
  // sprint = no header (avoid noise on first decompose). Multi-sprint
  // = "Sprint N" header with item count.
  const groups = groupBySprint(activeFeatures);
  const showSprintHeaders = groups.length > 1;

  featuresListEl.innerHTML = groups
    .map(([sprint, items]) => {
      const sprintHeader = showSprintHeaders
        ? `<div style="font-size:12px; font-weight:600; color:var(--text-muted,#888); padding:6px 0 2px 0; border-top:1px dashed var(--border,#ececf0); margin-top:6px;">Sprint ${sprint} · ${items.length} 个功能</div>`
        : "";
      const rows = items
        .map((f) => {
          const c = COMPLEXITY_COLOR[f.complexity];
          const cl = COMPLEXITY_LABEL[f.complexity];
          return `
            <label style="display:flex; gap:10px; align-items:flex-start; padding:10px; background:${f.selected ? "#fff" : "#f7f7f9"}; border:1px solid var(--border,#ececf0); border-radius:6px; cursor:pointer;">
              <input type="checkbox" ${f.selected ? "checked" : ""} data-feature-toggle="${escapeHTML(f.id)}" style="margin:3px 0 0 0; padding:0; width:auto; flex-shrink:0;" />
              <div style="flex:1; min-width:0;">
                <div style="display:flex; gap:8px; align-items:center; flex-wrap:wrap;">
                  <div style="font-weight:500; font-size:14px;">${escapeHTML(f.title)}</div>
                  <span style="padding:1px 6px; border-radius:3px; background:${c}; color:#fff; font-size:10px;">${escapeHTML(cl)}</span>
                </div>
                <div style="font-size:12px; color:var(--text-muted,#888); margin-top:3px;">${escapeHTML(f.description)}</div>
              </div>
            </label>`;
        })
        .join("");
      return sprintHeader + rows;
    })
    .join("");

  featuresListEl.querySelectorAll<HTMLInputElement>("[data-feature-toggle]").forEach((cb) => {
    cb.addEventListener("change", () => {
      const id = cb.getAttribute("data-feature-toggle");
      if (id) void toggleFeature(id, cb.checked);
    });
  });
}

function renderTasks(): void {
  if (activeTasks.length === 0) {
    tasksSection.hidden = true;
    return;
  }
  tasksSection.hidden = false;
  const todoCount = activeTasks.filter((t) => t.status === "todo").length;
  tasksCountEl.textContent = `共 ${activeTasks.length} 个 · ${todoCount} 待办`;
  // Disable the dispatch-all button when there's nothing to dispatch
  // OR when no bot is selected. Re-evaluated on each render.
  dispatchAllBtn.disabled = todoCount === 0 || !dispatchAllBotSelect.value;
  // Sync the dropdown to the project's preferred provider whenever
  // the panel opens — saves a click in the common case (project
  // has code_provider=kimi, agent attached as kimi-bot).
  syncDispatchAllBotOptions();
  // Group by sprint so the user can see iteration history in
  // order. Same logic as features: single sprint = no header,
  // multi-sprint = labeled section per sprint.
  const taskGroups = groupBySprint(activeTasks);
  const showTaskSprintHeaders = taskGroups.length > 1;
  tasksListEl.innerHTML = taskGroups
    .map(([sprint, items]) => {
      const sprintHeader = showTaskSprintHeaders
        ? `<div style="font-size:12px; font-weight:600; color:var(--text-muted,#888); padding:6px 0 2px 0; border-top:1px dashed var(--border,#ececf0); margin-top:6px;">Sprint ${sprint} · ${items.length} 个任务</div>`
        : "";
      const rows = items
        .map((t) => {
          const opts = TASK_STATUS_OPTIONS.map(
            (s) => `<option value="${s}" ${s === t.status ? "selected" : ""}>${TASK_STATUS_LABEL[s]}</option>`,
          ).join("");
          const isPickerOpen = pickerOpenForTaskID === t.id;
          const finishBtn = t.status !== "done"
            ? `<button type="button" class="btn-inline btn-secondary" data-task-finish="${escapeHTML(t.id)}" title="标记为完成" style="font-size:11px; padding:3px 6px;">✓</button>`
            : "";
          const cancelBtn = t.status !== "blocked" && t.status !== "done"
            ? `<button type="button" class="btn-inline btn-secondary" data-task-cancel="${escapeHTML(t.id)}" title="标记为阻塞 / 取消" style="font-size:11px; padding:3px 6px;">✗</button>`
            : "";
          return `
            <div style="padding:10px; background:#fff; border:1px solid var(--border,#ececf0); border-radius:6px; display:flex; gap:10px; align-items:flex-start;">
              <div style="flex:1; min-width:0;">
                <div style="font-weight:500; font-size:14px;">${escapeHTML(t.title)}</div>
                <div style="font-size:12px; color:var(--text-muted,#888); margin-top:3px;">${escapeHTML(t.description)}</div>
                <div style="margin-top:6px; font-size:11px; display:flex; align-items:center; gap:8px; flex-wrap:wrap;">
                  ${renderTaskBotSlot(t, isPickerOpen)}
                </div>
              </div>
              <div style="display:flex; gap:4px; align-items:center;">
                ${finishBtn}${cancelBtn}
                <select class="input" style="font-size:12px; padding:4px 6px; width:auto;" data-task-status="${escapeHTML(t.id)}">${opts}</select>
              </div>
            </div>`;
        })
        .join("");
      return sprintHeader + rows;
    })
    .join("");
  tasksListEl.querySelectorAll<HTMLSelectElement>("[data-task-status]").forEach((sel) => {
    sel.addEventListener("change", () => {
      const id = sel.getAttribute("data-task-status");
      if (id) void changeTaskStatus(id, sel.value as TaskStatus);
    });
  });
  tasksListEl.querySelectorAll<HTMLElement>("[data-task-finish]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const id = btn.getAttribute("data-task-finish");
      if (id) void changeTaskStatus(id, "done");
    });
  });
  tasksListEl.querySelectorAll<HTMLElement>("[data-task-cancel]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const id = btn.getAttribute("data-task-cancel");
      if (id) void changeTaskStatus(id, "blocked");
    });
  });
  tasksListEl.querySelectorAll<HTMLElement>("[data-task-pickup]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const id = btn.getAttribute("data-task-pickup");
      if (!id) return;
      pickerOpenForTaskID = pickerOpenForTaskID === id ? null : id;
      renderTasks();
    });
  });
  tasksListEl.querySelectorAll<HTMLElement>("[data-pickup-bot]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const taskID = btn.getAttribute("data-task-id");
      const botID = btn.getAttribute("data-pickup-bot");
      if (taskID && botID) void doPickup(taskID, botID);
    });
  });
}

function renderTaskBotSlot(t: ProjectTask, pickerOpen: boolean): string {
  if (t.bot_user_id) {
    const bot = availableBots.find((b) => b.bot_user_id === t.bot_user_id);
    const botName = bot?.name || t.bot_user_id;
    // chat.html accepts ?user_id=<bot> to navigate to the existing
    // user↔bot thread; the most-recent llm_thread (the one we just
    // created) is auto-selected by chat's loader.
    const link = `/chat.html?user_id=${encodeURIComponent(t.bot_user_id)}`;
    return `<span style="color:var(--text-muted,#888);">🤖 ${escapeHTML(botName)}</span>
            <a href="${link}" style="color:var(--accent,#3a7);">→ 进入会话</a>`;
  }
  if (!pickerOpen) {
    return `<button type="button" class="btn-inline btn-secondary" data-task-pickup="${escapeHTML(t.id)}" style="font-size:11px; padding:3px 8px;">🤖 派给 Bot</button>`;
  }
  if (availableBots.length === 0) {
    return `<span style="color:var(--text-muted,#888);">没有 bot 可用，请先在 Bots 页面创建一个</span>
            <button type="button" class="btn-inline btn-secondary" data-task-pickup="${escapeHTML(t.id)}" style="font-size:11px; padding:3px 8px;">取消</button>`;
  }
  // Sort bots so the project's preferred code_provider comes first.
  // botToolMap[bot_user_id] returns the attached agent's tool
  // ("kimi"/"claude"/"codex"/""). Matching = bot's tool ===
  // project.code_provider.
  const preferred = activeProject?.code_provider || "";
  const sorted = [...availableBots].sort((a, b) => {
    const at = botToolMap[a.bot_user_id] || "";
    const bt = botToolMap[b.bot_user_id] || "";
    const aMatch = preferred && at === preferred ? 1 : 0;
    const bMatch = preferred && bt === preferred ? 1 : 0;
    if (aMatch !== bMatch) return bMatch - aMatch;
    // Sub-sort: any attached tool > unattached
    return (bt ? 1 : 0) - (at ? 1 : 0);
  });
  const choices = sorted
    .map((b) => {
      const tool = botToolMap[b.bot_user_id] || "";
      const matchPreferred = preferred && tool === preferred;
      const cls = matchPreferred ? "btn-inline" : "btn-inline btn-secondary";
      const tag = tool
        ? ` <span style="font-size:10px; color:${matchPreferred ? "#fff" : "var(--text-muted,#888)"};">[${escapeHTML(tool)}]</span>`
        : "";
      const style = matchPreferred
        ? "font-size:11px; padding:3px 8px; background:#3a7; color:#fff;"
        : "font-size:11px; padding:3px 8px;";
      return `<button type="button" class="${cls}" data-pickup-bot="${escapeHTML(b.bot_user_id)}" data-task-id="${escapeHTML(t.id)}" style="${style}">${escapeHTML(b.name)}${tag}</button>`;
    })
    .join("");
  const hint = preferred
    ? `<span style="color:var(--text-muted,#888);">选一个 bot（首选 <strong>${escapeHTML(preferred)}</strong>）：</span>`
    : `<span style="color:var(--text-muted,#888);">选一个 bot：</span>`;
  return `${hint}${choices}
          <button type="button" class="btn-inline btn-secondary" data-task-pickup="${escapeHTML(t.id)}" style="font-size:11px; padding:3px 8px;">取消</button>`;
}

// ---- data ops -----------------------------------------------------

async function loadProjects(): Promise<void> {
  const { response, data } = await fetchProjects();
  if (!response.ok) {
    listEl.innerHTML = `<div class="chat-empty">加载失败</div>`;
    return;
  }
  projects = data.projects;
  renderProjectList();
}

async function openProject(id: string): Promise<void> {
  activeProjectID = id;
  renderProjectList();
  const { response, data } = await fetchProject(id);
  if (!response.ok) {
    activeProject = null;
    renderProjectPanel();
    return;
  }
  activeProject = data.project;
  activeFeatures = data.features;
  activeTasks = data.tasks;
  renderProjectPanel();
}

async function saveGitRemoteUrl(): Promise<void> {
  if (!activeProject) return;
  const url = gitRemoteUrlEl.value.trim();
  if (url === (activeProject.git_remote_url || "")) {
    setStatus(decomposeStatusEl, "未修改", false);
    return;
  }
  saveGitRemoteBtn.disabled = true;
  setStatus(decomposeStatusEl, "保存中...", false);
  const { response, data } = await updateProject(activeProject.id, { git_remote_url: url });
  saveGitRemoteBtn.disabled = false;
  if (!response.ok) {
    setStatus(decomposeStatusEl, "保存失败", true);
    return;
  }
  activeProject = data.project;
  setStatus(decomposeStatusEl, url ? "已保存（任务结束会 push）" : "已清空（不再 push）", false);
}

async function saveDescription(): Promise<void> {
  if (!activeProject) return;
  const desc = descEl.value.trim();
  if (desc === activeProject.description) {
    setStatus(decomposeStatusEl, "未修改", false);
    return;
  }
  saveDescBtn.disabled = true;
  setStatus(decomposeStatusEl, "保存中...", false);
  const { response, data } = await updateProject(activeProject.id, { description: desc });
  saveDescBtn.disabled = false;
  if (!response.ok) {
    setStatus(decomposeStatusEl, "保存失败", true);
    return;
  }
  activeProject = data.project;
  setStatus(decomposeStatusEl, "已保存", false);
  // Refresh the list so updated_at sorting reflects.
  void loadProjects();
}

async function decompose(): Promise<void> {
  if (!activeProject) return;
  if (!activeProject.description.trim() && !descEl.value.trim()) {
    setStatus(decomposeStatusEl, "请先填写产品描述", true);
    return;
  }
  // Auto-save the description before decomposing so the AI sees the
  // latest text.
  if (descEl.value.trim() !== activeProject.description) {
    await saveDescription();
  }
  decomposeBtn.disabled = true;
  setStatus(decomposeStatusEl, "AI 正在拆解功能（最多 90 秒）...", false);
  const llmIDRaw = decomposeLLMSelect.value;
  const llmID = llmIDRaw ? Number(llmIDRaw) : undefined;
  const botID = decomposeBotSelect.value || undefined;
  const { response, data } = await decomposeProject(activeProject.id, llmID, botID);
  decomposeBtn.disabled = false;
  if (!response.ok) {
    setStatus(decomposeStatusEl, data.error || "拆解失败", true);
    return;
  }
  // Reload from server so we get the merged features list (old
  // sprints + the just-appended new sprint), not just the new
  // batch from the response.
  await openProject(activeProject.id);
  // Surface which LLM produced this run + the new sprint number
  // so A/B comparison + iteration history are obvious.
  const llmTag = data.llm_used
    ? `（${escapeHTML(data.llm_used.name)} · ${escapeHTML(data.llm_used.model)}）`
    : "";
  const newCount = data.features.length;
  const sprintTag = data.new_sprint ? ` → Sprint ${data.new_sprint}` : "";
  setStatus(decomposeStatusEl, `拆出 ${newCount} 个新功能${sprintTag} ${llmTag}`, false);
}

async function toggleFeature(featureID: string, selected: boolean): Promise<void> {
  if (!activeProject) return;
  const f = activeFeatures.find((x) => x.id === featureID);
  if (!f) return;
  f.selected = selected;
  renderFeatures();
  await updateProjectFeature(activeProject.id, featureID, {
    title: f.title,
    description: f.description,
    complexity: f.complexity,
    selected,
  });
}

async function generatePlan(): Promise<void> {
  if (!activeProject) return;
  generatePlanBtn.disabled = true;
  setStatus(planStatusEl, "生成中...", false);
  const { response, data } = await generateProjectPlan(activeProject.id);
  generatePlanBtn.disabled = false;
  if (!response.ok) {
    setStatus(planStatusEl, "生成失败", true);
    return;
  }
  setStatus(planStatusEl, `创建 ${data.created_count} 个新任务（已存在的功能不重复创建）`, false);
  void openProject(activeProject.id);
}

async function changeTaskStatus(taskID: string, status: TaskStatus): Promise<void> {
  if (!activeProject) return;
  const t = activeTasks.find((x) => x.id === taskID);
  if (!t) return;
  t.status = status;
  await updateProjectTask(activeProject.id, taskID, { status });
}

// syncDispatchAllBotOptions populates the "派全部" bot dropdown
// with ONLY currently-attached bots. botToolMap[id] is non-empty
// when fetchAgentBotStatus reported attached=true; everything
// else is filtered out. Picking a detached bot would 503 from
// the pickup handler's agent precheck, which is not the failure
// mode the user can act on through this UI.
function syncDispatchAllBotOptions(): void {
  const attached = availableBots.filter((b) => (botToolMap[b.bot_user_id] || "") !== "");
  if (attached.length === 0) {
    dispatchAllBotSelect.innerHTML = `<option value="">没有 attached bot（请先 polar-agent attach）</option>`;
    return;
  }
  const preferred = activeProject?.code_provider || "";
  const sorted = [...attached].sort((a, b) => {
    const at = botToolMap[a.bot_user_id] || "";
    const bt = botToolMap[b.bot_user_id] || "";
    const aMatch = preferred && at === preferred ? 1 : 0;
    const bMatch = preferred && bt === preferred ? 1 : 0;
    if (aMatch !== bMatch) return bMatch - aMatch;
    return a.name.localeCompare(b.name);
  });
  const previous = dispatchAllBotSelect.value;
  const opts = ['<option value="">选 bot…</option>'];
  for (const b of sorted) {
    const tool = botToolMap[b.bot_user_id] || "";
    opts.push(`<option value="${escapeHTML(b.bot_user_id)}">${escapeHTML(b.name)} [${escapeHTML(tool)}]</option>`);
  }
  dispatchAllBotSelect.innerHTML = opts.join("");
  if (previous && sorted.some((b) => b.bot_user_id === previous)) {
    dispatchAllBotSelect.value = previous;
  } else if (preferred) {
    const match = sorted.find((b) => (botToolMap[b.bot_user_id] || "") === preferred);
    if (match) {
      dispatchAllBotSelect.value = match.bot_user_id;
    }
  }
}

// dispatchAllTodos sequentially picks up every todo task to the
// selected bot. Sequential (not parallel) on purpose: tasks in
// one project share the workdir under $workdir/<project_id>, so
// a fan-out would have N kimi-cli processes fighting over the
// same --continue session. The bot replies async; we don't wait
// for chat to finish, just wait for the pickup HTTP call to
// return so each new chat thread is ordered.
async function dispatchAllTodos(): Promise<void> {
  if (!activeProject) return;
  const botID = dispatchAllBotSelect.value;
  if (!botID) {
    setStatus(dispatchAllStatusEl, "请先选 bot", true);
    return;
  }
  const todos = activeTasks.filter((t) => t.status === "todo");
  if (todos.length === 0) {
    setStatus(dispatchAllStatusEl, "没有 todo 任务", true);
    return;
  }
  if (!window.confirm(`将 ${todos.length} 个 todo 任务全部派给该 bot？\n（顺序派单，每条任务等上一条 pickup 返回再发下一条）`)) {
    return;
  }
  dispatchAllBtn.disabled = true;
  let done = 0;
  let failed = 0;
  let lastError = "";
  for (const t of todos) {
    setStatus(dispatchAllStatusEl, `派单中 ${done + 1}/${todos.length} · ${escapeHTML(t.title)}`, false);
    try {
      const { response, data } = await pickupProjectTask(activeProject.id, t.id, botID);
      if (response.ok) {
        const idx = activeTasks.findIndex((x) => x.id === t.id);
        if (idx >= 0) {
          activeTasks[idx] = data.task;
        }
        done++;
      } else {
        failed++;
        const err = (data as { error?: string }).error;
        if (err) lastError = err;
        // Stop on first failure — when the precheck (e.g. agent
        // not attached, 503) fails for one task, every following
        // task will fail the same way. No point hammering.
        if (response.status === 503) {
          break;
        }
      }
    } catch (e) {
      failed++;
      lastError = String(e);
    }
    renderTasks();
  }
  setStatus(
    dispatchAllStatusEl,
    failed === 0
      ? `✅ 全部 ${done} 条派单完成`
      : `${done} 条成功 / ${failed} 条失败${lastError ? "：" + lastError : ""}`,
    failed > 0,
  );
}

// finishAll / cancelAll wrap the bulk-status endpoint with a
// confirm prompt + status text. Sourced from one helper to keep
// behavior consistent — the only difference is the target status
// and the from_statuses filter.
async function bulkSetTaskStatus(
  target: "done" | "blocked",
  fromStatuses: string[],
  confirmMsg: string,
  successPrefix: string,
): Promise<void> {
  if (!activeProject) return;
  const matched = activeTasks.filter((t) => fromStatuses.includes(t.status));
  if (matched.length === 0) {
    setStatus(dispatchAllStatusEl, "没有符合条件的任务", true);
    return;
  }
  if (!window.confirm(confirmMsg.replace("{n}", String(matched.length)))) {
    return;
  }
  finishAllBtn.disabled = true;
  cancelAllBtn.disabled = true;
  try {
    const { response, data } = await bulkUpdateProjectTaskStatus(
      activeProject.id,
      target,
      fromStatuses,
    );
    if (!response.ok) {
      setStatus(dispatchAllStatusEl, ((data as { error?: string }).error || "失败"), true);
      return;
    }
    setStatus(dispatchAllStatusEl, `${successPrefix} ${data.updated} 个任务`, false);
    // Re-fetch to surface server-side timestamp updates (started_at
    // / completed_at) which we don't recompute client-side.
    await openProject(activeProject.id);
  } finally {
    finishAllBtn.disabled = false;
    cancelAllBtn.disabled = false;
  }
}

async function finishAllTasks(): Promise<void> {
  await bulkSetTaskStatus(
    "done",
    ["todo", "in_progress", "review", "blocked"],
    "把全部 {n} 个未完成任务标记为「完成」？",
    "✓ 已完成",
  );
}

async function cancelAllTasks(): Promise<void> {
  await bulkSetTaskStatus(
    "blocked",
    ["todo", "in_progress", "review"],
    "把全部 {n} 个未完成任务标记为「阻塞 / 取消」？",
    "✗ 已取消",
  );
}

async function loadBots(): Promise<void> {
  const { response, data } = await fetchBotUsers();
  if (response.ok) {
    availableBots = data.bots || [];
  }
  // Fan out per-bot status fetches in parallel so the picker can
  // render with tool annotations + preferred-provider sort. Errors
  // are silently dropped — picker degrades to the un-sorted list
  // which is fine.
  await Promise.all(
    availableBots.map(async (b) => {
      try {
        const { response: r, data: d } = await fetchAgentBotStatus(b.bot_user_id);
        if (r.ok && d.attached) {
          botToolMap[b.bot_user_id] = (d.tool || "").trim();
        }
      } catch {
        // ignore
      }
    }),
  );
}

// loadDecomposeLLMs populates the LLM picker next to AI 拆解.
// First option is "auto"; the rest are workspace-available configs
// that have an API key. Disabled configs (no key) are surfaced
// with a "(no key)" suffix but disabled — visible to the user
// without misleading them into picking an unusable one.
async function loadDecomposeLLMs(): Promise<void> {
  const { response, data } = await fetchAvailableLLMConfigs();
  if (!response.ok) {
    return;
  }
  const configs = data.configs || [];
  const opts = ['<option value="">拆解用 LLM（自动选）</option>'];
  for (const cfg of configs) {
    const label = `${escapeHTML(cfg.name)} · ${escapeHTML(cfg.model)}${cfg.has_api_key ? "" : "（缺 API Key）"}`;
    const disabled = cfg.has_api_key ? "" : " disabled";
    opts.push(`<option value="${cfg.id}"${disabled}>${label}</option>`);
  }
  decomposeLLMSelect.innerHTML = opts.join("");
}

// loadDecomposeBots fills the bot dropdown next to "AI 拆解".
// Only **classic** bots — passthrough bots have no LLM config so
// they can't drive a chat completion call. Each option label is
// the bot name; the bot's bound LLM config is what actually runs.
function loadDecomposeBots(): void {
  const classicBots = availableBots.filter((b) => b.bot_kind === "classic");
  if (classicBots.length === 0) {
    decomposeBotSelect.innerHTML = `<option value="">Bot 角色（无可用 classic bot）</option>`;
    return;
  }
  const previous = decomposeBotSelect.value;
  const opts = ['<option value="">Bot 角色（不用 bot persona）</option>'];
  for (const b of classicBots) {
    opts.push(`<option value="${escapeHTML(b.bot_user_id)}">${escapeHTML(b.name)}（${escapeHTML(b.config_name)}）</option>`);
  }
  decomposeBotSelect.innerHTML = opts.join("");
  if (previous && classicBots.some((b) => b.bot_user_id === previous)) {
    decomposeBotSelect.value = previous;
  }
}

async function doPickup(taskID: string, botUserID: string): Promise<void> {
  if (!activeProject) return;
  pickerOpenForTaskID = null;
  const { response, data } = await pickupProjectTask(activeProject.id, taskID, botUserID);
  if (!response.ok) {
    window.alert(("error" in data && (data as { error?: string }).error) || "派单失败");
    renderTasks();
    return;
  }
  // Replace the task in-place rather than refetching the whole
  // project — the response carries the updated task and we already
  // know it's transitioned to in_progress.
  const idx = activeTasks.findIndex((x) => x.id === taskID);
  if (idx >= 0) {
    activeTasks[idx] = data.task;
  }
  renderTasks();
}

async function doDeleteProject(): Promise<void> {
  if (!activeProject) return;
  if (!window.confirm(`确认删除项目「${activeProject.name}」？该操作不可恢复，所有功能 / 任务一同清除。`)) return;
  const { response } = await deleteProject(activeProject.id);
  if (!response.ok) {
    window.alert("删除失败");
    return;
  }
  activeProjectID = null;
  activeProject = null;
  activeFeatures = [];
  activeTasks = [];
  await loadProjects();
  if (projects.length > 0) void openProject(projects[0].id);
  else renderProjectPanel();
}

function openCreateModal(): void {
  createName.value = "";
  createDesc.value = "";
  createRuntimeEnv.value = "";
  createBusinessScenario.value = "";
  createCodeProvider.value = "";
  createGitRemoteUrl.value = "";
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
    setStatus(createStatusEl, "请填写项目名称", true);
    return;
  }
  setStatus(createStatusEl, "创建中...", false);
  const { response, data } = await createProject({
    name,
    description: createDesc.value.trim(),
    runtime_env: createRuntimeEnv.value,
    business_scenario: createBusinessScenario.value,
    code_provider: createCodeProvider.value,
    git_remote_url: createGitRemoteUrl.value.trim(),
  });
  if (!response.ok) {
    setStatus(createStatusEl, "创建失败", true);
    return;
  }
  closeCreateModal();
  await loadProjects();
  void openProject(data.project.id);
}

// ---- wiring -------------------------------------------------------

newBtn.addEventListener("click", openCreateModal);
agentTokensBtn.addEventListener("click", () => void openAgentTokensModal());
agentTokensModalCloseBtn.addEventListener("click", () => setModalOpen(agentTokensModal, false));
agentTokensModal.addEventListener("click", (e) => {
  const t = e.target as HTMLElement;
  if (t.classList.contains("modal-backdrop")) setModalOpen(agentTokensModal, false);
});
agentTokenCreateBtn.addEventListener("click", () => void doCreateAgentToken());
agentTokenCopyBtn.addEventListener("click", () => {
  const raw = agentTokenJustCreatedRawEl.textContent || "";
  if (raw) {
    void navigator.clipboard.writeText(raw).then(() => {
      const orig = agentTokenCopyBtn.textContent;
      agentTokenCopyBtn.textContent = "已复制";
      setTimeout(() => {
        agentTokenCopyBtn.textContent = orig;
      }, 1200);
    });
  }
});

async function openAgentTokensModal(): Promise<void> {
  agentTokenJustCreatedEl.hidden = true;
  agentTokenNameInput.value = "";
  setModalOpen(agentTokensModal, true);
  await loadAgentTokens();
}

async function loadAgentTokens(): Promise<void> {
  agentTokensListEl.innerHTML = `<div class="chat-empty">加载中...</div>`;
  const { response, data } = await fetchAgentTokens();
  if (!response.ok) {
    agentTokensListEl.innerHTML = `<div class="chat-empty">加载失败</div>`;
    return;
  }
  renderAgentTokens(data.tokens);
}

function renderAgentTokens(tokens: AgentToken[]): void {
  if (tokens.length === 0) {
    agentTokensListEl.innerHTML = `<div class="chat-empty">还没有 token，上面输个名字然后「＋ 新建 token」</div>`;
    return;
  }
  agentTokensListEl.innerHTML = tokens
    .map((t) => {
      const created = new Date(t.created_at).toLocaleString();
      const lastUsed = t.last_used_at ? new Date(t.last_used_at).toLocaleString() : "—";
      const revoked = t.revoked_at ? `<span style="color:#c33; font-size:11px;">已撤销</span>` : "";
      return `
        <div style="padding:8px 10px; background:${t.revoked_at ? "#f7f7f9" : "#fff"}; border:1px solid var(--border,#ececf0); border-radius:6px; display:flex; gap:8px; align-items:center;">
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500; font-size:13px;">${escapeHTML(t.name || "(no name)")} ${revoked}</div>
            <div style="font-size:11px; color:var(--text-muted,#888);">创建 ${escapeHTML(created)} · 最后使用 ${escapeHTML(lastUsed)}</div>
          </div>
          ${
            t.revoked_at
              ? ""
              : `<button type="button" class="btn-inline" data-revoke-token="${escapeHTML(t.id)}" style="color:#c33; font-size:12px;">撤销</button>`
          }
        </div>`;
    })
    .join("");
  agentTokensListEl.querySelectorAll<HTMLButtonElement>("[data-revoke-token]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const id = btn.getAttribute("data-revoke-token");
      if (id) void doRevokeAgentToken(id);
    });
  });
}

async function doCreateAgentToken(): Promise<void> {
  const name = agentTokenNameInput.value.trim() || "polar-agent";
  agentTokenCreateBtn.disabled = true;
  const { response, data } = await createAgentToken(name);
  agentTokenCreateBtn.disabled = false;
  if (!response.ok) {
    window.alert("创建失败");
    return;
  }
  agentTokenJustCreatedRawEl.textContent = data.raw;
  agentTokenJustCreatedEl.hidden = false;
  agentTokenNameInput.value = "";
  await loadAgentTokens();
}

async function doRevokeAgentToken(id: string): Promise<void> {
  if (!window.confirm("撤销该 token？已运行的 polar-agent 下次重连时会被拒。")) return;
  const { response } = await revokeAgentToken(id);
  if (!response.ok) {
    window.alert("撤销失败");
    return;
  }
  await loadAgentTokens();
}
createModalCloseBtn.addEventListener("click", closeCreateModal);
createModal.addEventListener("click", (e) => {
  const t = e.target as HTMLElement;
  if (t.classList.contains("modal-backdrop")) closeCreateModal();
});
createForm.addEventListener("submit", (e) => void submitCreate(e));
saveDescBtn.addEventListener("click", () => void saveDescription());
saveGitRemoteBtn.addEventListener("click", () => void saveGitRemoteUrl());
decomposeBtn.addEventListener("click", () => void decompose());

// ── Section collapse ────────────────────────────────────────────
//
// Each section's hidden state lives in localStorage so the user's
// preference survives reloads. The chevron is a tiny stateless
// "▼" / "▶" toggle: ▼ = expanded, ▶ = collapsed.

const COLLAPSE_FEATURES_KEY = "polar_projects_features_collapsed";
const COLLAPSE_TASKS_KEY = "polar_projects_tasks_collapsed";

function applyCollapse(btn: HTMLButtonElement, body: HTMLElement, collapsed: boolean): void {
  body.style.display = collapsed ? "none" : "";
  btn.textContent = collapsed ? "▶" : "▼";
}

function readCollapse(key: string): boolean {
  try {
    return window.localStorage.getItem(key) === "1";
  } catch {
    return false;
  }
}
function writeCollapse(key: string, collapsed: boolean): void {
  try {
    window.localStorage.setItem(key, collapsed ? "1" : "0");
  } catch {
    // Storage might be unavailable in private mode; collapsing
    // still works for the current session, just doesn't persist.
  }
}

// Restore on page load.
applyCollapse(featuresCollapseBtn, featuresBodyEl, readCollapse(COLLAPSE_FEATURES_KEY));
// tasksListEl is the collapsable body of the tasks section
// (the dispatch row above it is the header and stays visible).
applyCollapse(tasksCollapseBtn, tasksListEl, readCollapse(COLLAPSE_TASKS_KEY));

featuresCollapseBtn.addEventListener("click", () => {
  const next = !readCollapse(COLLAPSE_FEATURES_KEY);
  writeCollapse(COLLAPSE_FEATURES_KEY, next);
  applyCollapse(featuresCollapseBtn, featuresBodyEl, next);
});
tasksCollapseBtn.addEventListener("click", () => {
  const next = !readCollapse(COLLAPSE_TASKS_KEY);
  writeCollapse(COLLAPSE_TASKS_KEY, next);
  applyCollapse(tasksCollapseBtn, tasksListEl, next);
});
generatePlanBtn.addEventListener("click", () => void generatePlan());
deleteBtn.addEventListener("click", () => void doDeleteProject());
dispatchAllBtn.addEventListener("click", () => void dispatchAllTodos());
finishAllBtn.addEventListener("click", () => void finishAllTasks());
cancelAllBtn.addEventListener("click", () => void cancelAllTasks());
filterSearchEl.addEventListener("input", renderProjectList);
filterStatusEl.addEventListener("change", renderProjectList);
filterCodeProviderEl.addEventListener("change", renderProjectList);
dispatchAllBotSelect.addEventListener("change", () => {
  // Re-evaluate disabled state when the bot selection changes
  // without redrawing the whole list.
  const todoCount = activeTasks.filter((t) => t.status === "todo").length;
  dispatchAllBtn.disabled = todoCount === 0 || !dispatchAllBotSelect.value;
});

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
  await Promise.all([loadProjects(), loadBots(), loadDecomposeLLMs()]);
  // loadDecomposeBots reads availableBots which loadBots populated;
  // sequential so it sees the data.
  loadDecomposeBots();
  if (projects.length > 0) {
    void openProject(projects[0].id);
  }
}

void init();
