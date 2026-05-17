// Typed fetch wrappers for /api/admin/wg-*.

import { requestJson } from "./http.js";
import type {
  WGBundleListResponse,
  WGBundleResponse,
  WGDeviceListResponse,
  WGHub,
  WGHubResponse,
  WGSiteListResponse,
  WGTokenCreateResponse,
  WGTokenListResponse,
} from "../types/wg.js";

// ---- hub ----

export async function fetchWGHub() {
  return requestJson<WGHubResponse>("/api/admin/wg-hub");
}

export async function updateWGHub(payload: Partial<WGHub>) {
  return requestJson<WGHubResponse>("/api/admin/wg-hub", {
    method: "PUT",
    body: payload,
  });
}

// ---- tokens ----

export async function listWGTokens() {
  return requestJson<WGTokenListResponse>("/api/admin/wg-tokens");
}

export async function createWGToken(label: string, ttlDays: number) {
  return requestJson<WGTokenCreateResponse>("/api/admin/wg-tokens", {
    method: "POST",
    body: { label, ttl_days: ttlDays },
  });
}

export async function revokeWGToken(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-tokens/${id}`, {
    method: "DELETE",
  });
}

// ---- devices ----

export async function listWGDevices(includeRemoved = false) {
  const qs = includeRemoved ? "?include_removed=1" : "";
  return requestJson<WGDeviceListResponse>(`/api/admin/wg-devices${qs}`);
}

export async function removeWGDevice(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-devices/${id}`, {
    method: "DELETE",
  });
}

// ---- sites ----

export async function listWGSites() {
  return requestJson<WGSiteListResponse>("/api/admin/wg-sites");
}

// ---- bundles ----

export async function listWGBundles() {
  return requestJson<WGBundleListResponse>("/api/admin/wg-bundles");
}

// Multipart upload — raw fetch (matches uploadFirmware in api/library.ts).
export async function uploadWGBundle(form: FormData): Promise<{ ok: boolean; status: number; data: WGBundleResponse }> {
  const resp = await fetch("/api/admin/wg-bundles/upload", {
    method: "POST",
    body: form,
    headers: {
      "X-Workspace-Id": localStorage.getItem("polar_active_workspace_id") || "",
    },
    credentials: "include",
  });
  const data: WGBundleResponse = await resp.json().catch(() => ({} as WGBundleResponse));
  return { ok: resp.ok, status: resp.status, data };
}

export async function setWGBundleLatest(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-bundles/${id}/latest`, {
    method: "PUT",
  });
}

export async function deleteWGBundle(id: number) {
  return requestJson<{ ok: boolean; error?: string }>(`/api/admin/wg-bundles/${id}`, {
    method: "DELETE",
  });
}
