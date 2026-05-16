// Type definitions for /api/llm-proxy/* — see internal/app/dock/llm_proxy_store.go.

import type { ErrorResponse } from "./dashboard.js";

export type LLMProxyToken = {
  id: number;
  workspace_id: string;
  owner_user_id: string;
  name: string;
  prefix: string;
  default_llm_config_id?: number;
  rate_limit_rpm: number;
  monthly_quota_usd: number;
  scopes: string;
  revoked_at?: string;
  created_at: string;
  last_used_at?: string;
  // Set only on POST response — the freshly-generated plaintext bearer.
  // Never returned by list/get endpoints.
  plaintext?: string;
};

export type LLMProxyTokenListResponse = ErrorResponse & {
  tokens?: LLMProxyToken[];
};

export type LLMProxyTokenCreatePayload = {
  name: string;
  default_llm_config_id?: number;
  rate_limit_rpm?: number;
  monthly_quota_usd?: number;
  scopes?: string;
};

export type LLMProxyTokenCreateResponse = ErrorResponse & {
  token?: LLMProxyToken;
  warning?: string;
};

export type LLMProxyTokenRevokeResponse = ErrorResponse & {
  token?: LLMProxyToken;
};
