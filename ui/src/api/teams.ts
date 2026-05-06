import { requestJson } from "./http.js";
import type {
  TeamCreatePayload,
  TeamCreateResponse,
  TeamDetailResponse,
  TeamListResponse,
  TeamMembersResponse,
  TeamUpdatePayload,
  TeamUpdateResponse,
} from "../types/teams.js";

export async function fetchTeams() {
  return requestJson<TeamListResponse>("/api/teams");
}

export async function fetchTeam(id: string) {
  return requestJson<TeamDetailResponse>(`/api/teams/${encodeURIComponent(id)}`);
}

export async function createTeam(payload: TeamCreatePayload) {
  return requestJson<TeamCreateResponse>("/api/teams", {
    method: "POST",
    body: payload,
  });
}

export async function updateTeam(id: string, payload: TeamUpdatePayload) {
  return requestJson<TeamUpdateResponse>(`/api/teams/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: payload,
  });
}

export async function deleteTeam(id: string) {
  return requestJson<{ ok: boolean }>(`/api/teams/${encodeURIComponent(id)}`, {
    method: "DELETE",
  });
}

export async function fetchTeamMembers(id: string) {
  return requestJson<TeamMembersResponse>(`/api/teams/${encodeURIComponent(id)}/members`);
}
