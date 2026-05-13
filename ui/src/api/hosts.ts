// Typed fetch wrappers for /api/hosts/* — Phase 0.

import { requestJson } from "./http.js";
import type {
  Host,
  HostDetailResponse,
  HostEnrollPayload,
  HostEnrollResponse,
  HostListResponse,
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

// Re-export the Host type for convenience.
export type { Host };
