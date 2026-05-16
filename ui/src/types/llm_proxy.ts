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

export type LLMProxyCall = {
  id: number;
  token_id: number;
  workspace_id: string;
  model_requested: string;
  model_resolved: string;
  llm_config_id?: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  latency_ms: number;
  status_code: number;
  error_text?: string;
  created_at: string;
};

export type LLMProxyCallListResponse = ErrorResponse & {
  calls?: LLMProxyCall[];
};

export type LLMProxyCallAggregate = {
  model: string;
  calls: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  ok_count: number;
  error_count: number;
};

export type LLMProxyCallAggregateResponse = ErrorResponse & {
  aggregate?: LLMProxyCallAggregate[];
  days?: number;
};
