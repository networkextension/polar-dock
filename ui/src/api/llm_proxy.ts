// Typed fetch wrappers for /api/llm-proxy/tokens.

import { requestJson } from "./http.js";
import type {
  LLMProxyTokenCreatePayload,
  LLMProxyTokenCreateResponse,
  LLMProxyTokenListResponse,
  LLMProxyTokenRevokeResponse,
} from "../types/llm_proxy.js";

export async function listProxyTokens() {
  return requestJson<LLMProxyTokenListResponse>("/api/llm-proxy/tokens");
}

export async function createProxyToken(payload: LLMProxyTokenCreatePayload) {
  return requestJson<LLMProxyTokenCreateResponse>("/api/llm-proxy/tokens", {
    method: "POST",
    body: payload,
  });
}

export async function revokeProxyToken(id: number) {
  return requestJson<LLMProxyTokenRevokeResponse>(`/api/llm-proxy/tokens/${id}`, {
    method: "DELETE",
  });
}
