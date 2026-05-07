// Projects page — phase 1.2 of the AI-driven workflow.
//
// One page covers the full pre-coding pipeline:
//   create project → write description → AI decompose into features →
//   user picks which features → generate plan → tasks list shows up.
//
// Per-task chat thread (1:1) wiring + bot pickup come in phase 1.3.

import {
  createProject,
  decomposeProject,
  deleteProject,
  fetchProject,
  fetchProjects,
  generateProjectPlan,
  updateProject,
  updateProjectFeature,
  updateProjectTask,
} from "./api/projects.js";
import { fetchCurrentUser, logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type {
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

const nameEl = byId<HTMLElement>("projectsName");
const statusPillEl = byId<HTMLElement>("projectsStatusPill");
const createdAtEl = byId<HTMLElement>("projectsCreatedAt");
const deleteBtn = byId<HTMLButtonElement>("projectsDeleteBtn");

const descEl = byId<HTMLTextAreaElement>("projectsDescription");
const saveDescBtn = byId<HTMLButtonElement>("projectsSaveDescBtn");
const decomposeBtn = byId<HTMLButtonElement>("projectsDecomposeBtn");
const decomposeStatusEl = byId<HTMLElement>("projectsDecomposeStatus");

const featuresSection = byId<HTMLElement>("projectsFeaturesSection");
const featuresListEl = byId<HTMLElement>("projectsFeaturesList");
const featuresCountEl = byId<HTMLElement>("projectsFeaturesCount");
const generatePlanBtn = byId<HTMLButtonElement>("projectsGeneratePlanBtn");
const planStatusEl = byId<HTMLElement>("projectsPlanStatus");

const tasksSection = byId<HTMLElement>("projectsTasksSection");
const tasksListEl = byId<HTMLElement>("projectsTasksList");
const tasksCountEl = byId<HTMLElement>("projectsTasksCount");

const newBtn = byId<HTMLButtonElement>("projectsNewBtn");
const createModal = byId<HTMLElement>("projectsCreateModal");
const createModalCloseBtn = byId<HTMLButtonElement>("projectsCreateModalCloseBtn");
const createForm = byId<HTMLFormElement>("projectsCreateForm");
const createName = byId<HTMLInputElement>("projectsCreateName");
const createDesc = byId<HTMLTextAreaElement>("projectsCreateDescription");
const createStatusEl = byId<HTMLElement>("projectsCreateStatus");

let projects: Project[] = [];
let activeProjectID: string | null = null;
let activeProject: Project | null = null;
let activeFeatures: ProjectFeature[] = [];
let activeTasks: ProjectTask[] = [];

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

function renderProjectList(): void {
  if (projects.length === 0) {
    listEl.innerHTML = `<div class="chat-empty">还没有项目，点右上角「＋ 新建项目」开始</div>`;
    return;
  }
  listEl.innerHTML = projects
    .map((p) => {
      const active = p.id === activeProjectID ? "active" : "";
      return `
        <div class="video-studio-project-item ${active}" data-project-id="${escapeHTML(p.id)}">
          <div style="width:32px; height:32px; border-radius:8px; background:linear-gradient(135deg,#e9e9ef,#cfd0d6); display:flex; align-items:center; justify-content:center; font-size:14px; color:#666; flex-shrink:0;">✦</div>
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">${escapeHTML(p.name)}</div>
            <div style="font-size:11px; color:var(--text-muted,#888);">${STATUS_LABEL[p.status] ?? p.status}</div>
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
  statusPillEl.textContent = STATUS_LABEL[activeProject.status] ?? activeProject.status;
  createdAtEl.textContent = `创建于 ${new Date(activeProject.created_at).toLocaleString()}`;
  descEl.value = activeProject.description;
  clearStatus(decomposeStatusEl);
  clearStatus(planStatusEl);

  renderFeatures();
  renderTasks();
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

  featuresListEl.innerHTML = activeFeatures
    .map((f) => {
      const c = COMPLEXITY_COLOR[f.complexity];
      const cl = COMPLEXITY_LABEL[f.complexity];
      return `
        <label style="display:flex; gap:10px; align-items:flex-start; padding:10px; background:${f.selected ? "#fff" : "#f7f7f9"}; border:1px solid var(--border,#ececf0); border-radius:6px; cursor:pointer;">
          <input type="checkbox" ${f.selected ? "checked" : ""} data-feature-toggle="${escapeHTML(f.id)}" style="margin-top:3px; flex-shrink:0;" />
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
  tasksCountEl.textContent = `共 ${activeTasks.length} 个`;
  tasksListEl.innerHTML = activeTasks
    .map((t) => {
      const opts = TASK_STATUS_OPTIONS.map(
        (s) => `<option value="${s}" ${s === t.status ? "selected" : ""}>${TASK_STATUS_LABEL[s]}</option>`,
      ).join("");
      return `
        <div style="padding:10px; background:#fff; border:1px solid var(--border,#ececf0); border-radius:6px; display:flex; gap:10px; align-items:flex-start;">
          <div style="flex:1; min-width:0;">
            <div style="font-weight:500; font-size:14px;">${escapeHTML(t.title)}</div>
            <div style="font-size:12px; color:var(--text-muted,#888); margin-top:3px;">${escapeHTML(t.description)}</div>
            ${
              t.chat_thread_id
                ? `<a href="/chat.html#thread=${t.chat_thread_id}" style="font-size:11px; margin-top:4px; display:inline-block;">→ 进入会话</a>`
                : `<div style="font-size:11px; color:var(--text-muted,#888); margin-top:4px;">（未派给 bot）</div>`
            }
          </div>
          <select class="input" style="font-size:12px; padding:4px 6px; width:auto;" data-task-status="${escapeHTML(t.id)}">${opts}</select>
        </div>`;
    })
    .join("");
  tasksListEl.querySelectorAll<HTMLSelectElement>("[data-task-status]").forEach((sel) => {
    sel.addEventListener("change", () => {
      const id = sel.getAttribute("data-task-status");
      if (id) void changeTaskStatus(id, sel.value as TaskStatus);
    });
  });
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
  const { response, data } = await decomposeProject(activeProject.id);
  decomposeBtn.disabled = false;
  if (!response.ok) {
    setStatus(decomposeStatusEl, data.error || "拆解失败", true);
    return;
  }
  activeFeatures = data.features;
  setStatus(decomposeStatusEl, `拆出 ${activeFeatures.length} 个功能`, false);
  renderFeatures();
  // Reload project so status flips to planning.
  void openProject(activeProject.id);
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
createModalCloseBtn.addEventListener("click", closeCreateModal);
createModal.addEventListener("click", (e) => {
  const t = e.target as HTMLElement;
  if (t.classList.contains("modal-backdrop")) closeCreateModal();
});
createForm.addEventListener("submit", (e) => void submitCreate(e));
saveDescBtn.addEventListener("click", () => void saveDescription());
decomposeBtn.addEventListener("click", () => void decompose());
generatePlanBtn.addEventListener("click", () => void generatePlan());
deleteBtn.addEventListener("click", () => void doDeleteProject());

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
  await loadProjects();
  if (projects.length > 0) {
    void openProject(projects[0].id);
  }
}

void init();
