// Typed fetch wrappers for /api/hosts/* and /api/host-skills.

import { requestJson } from "./http.js";
import type {
  Host,
  HostDetailResponse,
  HostEnrollPayload,
  HostEnrollResponse,
  HostListResponse,
  HostSkill,
  HostSkillCredential,
  HostSkillCredentialListResponse,
  HostSkillListResponse,
  HostSkillWithHost,
  HostSkillsWithHostListResponse,
} from "../types/hosts.js";

export async function fetchHosts() {
  return requestJson<HostListResponse>("/api/hosts");
}

export async function fetchHost(id: string) {
  return requestJson<HostDetailResponse>(`/api/hosts/${encodeURIComponent(id)}`);
}

export async function enrollHost(payload: HostEnrollPayload) {
  return requestJson<HostEnrollResponse>("/api/hosts/enroll", {
    method: "POST",
    body: payload,
  });
}

export async function deleteHost(id: string) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/hosts/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

// P1d: workspace-scoped list of host_skill rows. Used by the bot
// edit page's binding picker.
export async function fetchHostSkillsForWorkspace() {
  return requestJson<HostSkillsWithHostListResponse>("/api/host-skills");
}

// P1d: per-host skill list. Used by the host detail page to render
// skill cards with their credential forms below.
export async function fetchHostSkillsForHost(hostID: string) {
  return requestJson<HostSkillListResponse>(`/api/hosts/${encodeURIComponent(hostID)}/skills`);
}

// P1c.4: credential vault on a single host_skill.
export async function fetchHostSkillCredentials(hostID: string, skillID: number) {
  return requestJson<HostSkillCredentialListResponse>(
    `/api/hosts/${encodeURIComponent(hostID)}/skills/${skillID}/credentials`,
  );
}

export async function putHostSkillCredential(hostID: string, skillID: number, key: string, value: string) {
  return requestJson<{ credential?: HostSkillCredential; error?: string }>(
    `/api/hosts/${encodeURIComponent(hostID)}/skills/${skillID}/credentials/${encodeURIComponent(key)}`,
    {
      method: "PUT",
      body: { value },
    },
  );
}

export async function deleteHostSkillCredential(hostID: string, skillID: number, key: string) {
  return requestJson<{ ok: boolean; error?: string }>(
    `/api/hosts/${encodeURIComponent(hostID)}/skills/${skillID}/credentials/${encodeURIComponent(key)}`,
    { method: "DELETE" },
  );
}

// P2 (Shell skill) -------------------------------------------------------

import type {
  ShellOpenResponse,
  ShellSessionListResponse,
} from "../types/hosts.js";

export async function openHostShell(
  hostID: string,
  skillID: number,
  body: { rows?: number; cols?: number; cwd?: string } = {},
) {
  return requestJson<ShellOpenResponse>(
    `/api/hosts/${encodeURIComponent(hostID)}/skills/${skillID}/shell/open`,
    { method: "POST", body },
  );
}

export async function listHostShellSessions(hostID: string) {
  return requestJson<ShellSessionListResponse>(
    `/api/hosts/${encodeURIComponent(hostID)}/skills/shell/sessions`,
  );
}

export async function kickHostShellSession(hostID: string, runID: number) {
  return requestJson<{ ok: boolean; error?: string }>(
    `/api/hosts/${encodeURIComponent(hostID)}/skills/shell/sessions/${runID}`,
    { method: "DELETE" },
  );
}

// Re-export the Host type for convenience.
export type { Host, HostSkill, HostSkillCredential, HostSkillWithHost };
