import { requestJson } from "./http.js";
import type {
  AgentTokenCreateResponse,
  AgentTokenListResponse,
  ProjectCreatePayload,
  ProjectCreateResponse,
  ProjectDecomposeResponse,
  ProjectDetailResponse,
  ProjectFeatureUpdatePayload,
  ProjectGeneratePlanResponse,
  ProjectListResponse,
  ProjectTask,
  ProjectTaskUpdatePayload,
  ProjectUpdatePayload,
} from "../types/projects.js";

export async function fetchProjects() {
  return requestJson<ProjectListResponse>("/api/projects");
}

export async function fetchProject(id: string) {
  return requestJson<ProjectDetailResponse>(`/api/projects/${encodeURIComponent(id)}`);
}

export async function createProject(payload: ProjectCreatePayload) {
  return requestJson<ProjectCreateResponse>("/api/projects", {
    method: "POST",
    body: payload,
  });
}

export async function updateProject(id: string, payload: ProjectUpdatePayload) {
  return requestJson<ProjectCreateResponse>(`/api/projects/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: payload,
  });
}

export async function deleteProject(id: string) {
  return requestJson<{ ok: boolean }>(`/api/projects/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export async function decomposeProject(id: string) {
  return requestJson<ProjectDecomposeResponse>(`/api/projects/${encodeURIComponent(id)}/decompose`, {
    method: "POST",
  });
}

export async function updateProjectFeature(id: string, featureID: string, payload: ProjectFeatureUpdatePayload) {
  return requestJson<{ ok: boolean }>(
    `/api/projects/${encodeURIComponent(id)}/features/${encodeURIComponent(featureID)}`,
    { method: "PUT", body: payload },
  );
}

export async function generateProjectPlan(id: string) {
  return requestJson<ProjectGeneratePlanResponse>(`/api/projects/${encodeURIComponent(id)}/plan`, {
    method: "POST",
  });
}

export async function updateProjectTask(id: string, taskID: string, payload: ProjectTaskUpdatePayload) {
  return requestJson<{ ok: boolean }>(
    `/api/projects/${encodeURIComponent(id)}/tasks/${encodeURIComponent(taskID)}`,
    { method: "PUT", body: payload },
  );
}

export type ProjectTaskPickupResponse = {
  task: ProjectTask;
  chat_thread_id: number;
  llm_thread_id: number;
  bot_name: string;
};

// Assigns a task to a bot. Backend creates / reuses the user↔bot
// chat thread, opens a fresh llm_thread titled after the task,
// drops the task description as the user's first message, and
// enqueues the AI agent so the bot starts working immediately.
export async function pickupProjectTask(id: string, taskID: string, botUserID: string) {
  return requestJson<ProjectTaskPickupResponse>(
    `/api/projects/${encodeURIComponent(id)}/tasks/${encodeURIComponent(taskID)}/pickup`,
    { method: "POST", body: { bot_user_id: botUserID } },
  );
}

// ---- agent tokens (polar-agent CLI credentials) ------------------

export async function fetchAgentTokens() {
  return requestJson<AgentTokenListResponse>("/api/agent/tokens");
}

export async function createAgentToken(name: string) {
  return requestJson<AgentTokenCreateResponse>("/api/agent/tokens", {
    method: "POST",
    body: { name },
  });
}

export async function revokeAgentToken(id: string) {
  return requestJson<{ ok: boolean }>(`/api/agent/tokens/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export type AgentBotStatus = {
  attached: boolean;
  workdir?: string;
  capabilities?: string[];
};

// Agent attach status for a single bot. Used by bots.ts to show a
// "passthrough mode" badge so users know a bot's configured model
// field is being bypassed by a local runtime (kimi-cli today).
export async function fetchAgentBotStatus(botUserID: string) {
  return requestJson<AgentBotStatus>(`/api/agent/bots/${encodeURIComponent(botUserID)}/status`);
}
