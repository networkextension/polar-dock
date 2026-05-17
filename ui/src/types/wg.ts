// Mirrors internal/app/dock/wg_store.go.

import type { ErrorResponse } from "./dashboard.js";

export type WGHub = {
  pubkey: string;
  endpoint: string;
  wg_ip: string;
  mesh_cidr: string;
  keepalive_sec: number;
  refresh_sec: number;
  updated_at: string;
};

export type WGSite = {
  id: number;
  slug: string;
  s_index: number;
  label: string;
  public_ip: string;
  lan_cidr: string;
  created_at: string;
  device_count?: number;
};

export type WGToken = {
  id: number;
  label: string;
  token_prefix: string;
  expires_at?: string;
  consumed_at?: string;
  consumed_by_device_id?: number;
  revoked_at?: string;
  created_at: string;
  created_by_user_id?: string;
  plaintext?: string;
};

export type WGLanAddr = { iface: string; cidr: string };

export type WGDevice = {
  id: number;
  device_id: string;
  site_id: number;
  d_index: number;
  device_ip: string;
  pubkey: string;
  hostname: string;
  os: string;
  arch: string;
  agent_ver: string;
  wg_listen_port: number;
  lan_addrs?: WGLanAddr[];
  wg_endpoint: string;
  token_expires_at?: string;
  created_at: string;
  last_seen_at?: string;
  removed_at?: string;
  site_slug?: string;
};

export type WGBundle = {
  id: number;
  version: string;
  blob_uri: string;
  blob_sha256: string;
  size_bytes: number;
  is_latest: boolean;
  notes: string;
  added_at: string;
  added_by_user_id: string;
};

export type WGHubResponse = ErrorResponse & { hub?: WGHub; configured?: boolean };
export type WGTokenListResponse = ErrorResponse & { tokens?: WGToken[] };
export type WGTokenCreateResponse = ErrorResponse & { token?: WGToken; warning?: string };
export type WGDeviceListResponse = ErrorResponse & { devices?: WGDevice[] };
export type WGSiteListResponse = ErrorResponse & { sites?: WGSite[] };
export type WGBundleListResponse = ErrorResponse & { bundles?: WGBundle[] };
export type WGBundleResponse = ErrorResponse & { bundle?: WGBundle; warning?: string; existing?: WGBundle };
