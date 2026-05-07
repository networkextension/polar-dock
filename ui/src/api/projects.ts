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
