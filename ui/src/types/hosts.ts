// Type definitions for the Host module (Phase 0).
// Mirror the Go-side structs in internal/app/dock/hosts_store.go.

import type { ErrorResponse } from "./dashboard.js";

export type AdvertisedSkill = {
  kind: string;
  version?: string;
  capabilities?: Record<string, unknown>;
};

export type Host = {
  id: string;
  workspace_id: string;
  slug: string;
  name: string;
  agent_token_id?: string;
  os: string;
  arch: string;
  last_seen_ip?: string;
  advertised_skills?: AdvertisedSkill[];
  last_seen_at?: string;
  created_at: string;
};

export type HostListResponse = ErrorResponse & {
  hosts?: Host[];
};

export type HostDetailResponse = ErrorResponse & {
  host?: Host;
};

export type HostEnrollPayload = {
  name: string;
};

export type HostEnrollResponse = ErrorResponse & {
  token?: string;
  expires_at?: string;
  host_name?: string;
  install_hint?: string;
};
