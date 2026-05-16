// /proxy-tokens.html — admin-gated CRUD for LLM proxy tokens.
//
// Workflow:
//   - On load: fetch tokens + llm_configs in parallel, render the table.
//   - "+ 新建 Token" opens the create modal pre-populated with the
//     llm_config picker. Submit -> backend returns plaintext ONCE ->
//     show the plaintext-shown-once modal with copy buttons.
//   - Each row's "撤销" button hits DELETE /api/llm-proxy/tokens/:id;
//     revoked tokens stay in the list but show as dimmed + read-only.

import { fetchLLMConfigs } from "./api/dashboard.js";
import {
  createProxyToken,
  listProxyTokens,
  revokeProxyToken,
} from "./api/llm_proxy.js";
import { logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, renderSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type { LLMConfig } from "./types/dashboard.js";
import type { LLMProxyToken } from "./types/llm_proxy.js";

initStoredTheme();
bindThemeSync();

const logoutBtn = byId<HTMLButtonElement>("logoutBtn");
const newTokenBtn = byId<HTMLButtonElement>("newTokenBtn");
const tokenTable = byId<HTMLTableElement>("tokenTable");
const tokenTbody = byId<HTMLTableSectionElement>("tokenTbody");
const tokenEmpty = byId<HTMLElement>("tokenEmpty");
const tokenLoading = byId<HTMLElement>("tokenLoading");
const tokenCount = byId<HTMLElement>("tokenCount");

const createModal = byId<HTMLElement>("createModal");
const createCloseBtn = byId<HTMLButtonElement>("createCloseBtn");
const createCancelBtn = byId<HTMLButtonElement>("createCancelBtn");
const createSubmitBtn = byId<HTMLButtonElement>("createSubmitBtn");
const createName = byId<HTMLInputElement>("createName");
const createLLMConfig = byId<HTMLSelectElement>("createLLMConfig");
const createRPM = byId<HTMLInputElement>("createRPM");
const createError = byId<HTMLElement>("createError");

const plaintextModal = byId<HTMLElement>("plaintextModal");
const plaintextValue = byId<HTMLInputElement>("plaintextValue");
const plaintextBaseURL = byId<HTMLInputElement>("plaintextBaseURL");
const plaintextCopyBtn = byId<HTMLButtonElement>("plaintextCopyBtn");
const plaintextBaseCopyBtn = byId<HTMLButtonElement>("plaintextBaseCopyBtn");
const plaintextDoneBtn = byId<HTMLButtonElement>("plaintextDoneBtn");

let llmConfigById = new Map<number, LLMConfig>();

function fmtRelative(iso?: string): string {
  if (!iso) return "—";
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return iso;
  const ageSec = Math.floor((Date.now() - t) / 1000);
  if (ageSec < 60) return `${ageSec}s 前`;
  if (ageSec < 3600) return `${Math.floor(ageSec / 60)}m 前`;
  if (ageSec < 86400) return `${Math.floor(ageSec / 3600)}h 前`;
  return `${Math.floor(ageSec / 86400)}d 前`;
}

function setBaseURLInPlaintextModal(): void {
  const origin = window.location.origin;
  plaintextBaseURL.value = `${origin}/api/proxy/v1`;
}

function renderTokenRow(tok: LLMProxyToken): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (tok.revoked_at) tr.style.opacity = "0.5";

  const cellName = document.createElement("td");
  cellName.textContent = tok.name;

  const cellPrefix = document.createElement("td");
  const code = document.createElement("code");
  code.textContent = tok.prefix + "…";
  code.style.fontSize = "11px";
  cellPrefix.appendChild(code);

  const cellLLM = document.createElement("td");
  if (tok.default_llm_config_id != null) {
    const cfg = llmConfigById.get(tok.default_llm_config_id);
    cellLLM.textContent = cfg ? `${cfg.name} · ${cfg.model}` : `#${tok.default_llm_config_id} (已删除)`;
  } else {
    cellLLM.textContent = "—";
  }

  const cellRPM = document.createElement("td");
  cellRPM.textContent = String(tok.rate_limit_rpm);

  const cellCreated = document.createElement("td");
  cellCreated.textContent = fmtRelative(tok.created_at);

  const cellLastUsed = document.createElement("td");
  cellLastUsed.textContent = fmtRelative(tok.last_used_at);

  const cellStatus = document.createElement("td");
  cellStatus.textContent = tok.revoked_at ? `🔴 已撤销 ${fmtRelative(tok.revoked_at)}` : "✓ 启用";

  const cellActions = document.createElement("td");
  if (!tok.revoked_at) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn-inline btn-secondary";
    btn.textContent = "撤销";
    btn.addEventListener("click", () => onRevoke(tok));
    cellActions.appendChild(btn);
  }

  tr.append(cellName, cellPrefix, cellLLM, cellRPM, cellCreated, cellLastUsed, cellStatus, cellActions);
  return tr;
}

async function refreshTokenList(): Promise<void> {
  tokenLoading.hidden = false;
  tokenTable.hidden = true;
  tokenEmpty.hidden = true;

  try {
    const { data } = await listProxyTokens();
    const items = data.tokens ?? [];
    tokenTbody.innerHTML = "";
    items.forEach((tok) => tokenTbody.appendChild(renderTokenRow(tok)));
    tokenCount.textContent = `${items.length} 个`;
    tokenTable.hidden = items.length === 0;
    tokenEmpty.hidden = items.length !== 0;
  } catch (err) {
    tokenEmpty.hidden = false;
    tokenEmpty.textContent = `加载失败：${(err as Error).message}`;
  } finally {
    tokenLoading.hidden = true;
  }
}

async function loadLLMConfigs(): Promise<void> {
  try {
    const { data } = await fetchLLMConfigs();
    const configs = data.configs ?? [];
    llmConfigById = new Map(configs.map((c) => [c.id, c]));

    createLLMConfig.innerHTML = "";
    if (configs.length === 0) {
      const opt = document.createElement("option");
      opt.value = "";
      opt.textContent = "—— 当前 workspace 还没有 llm_config，先去 /bots.html 创建一个 ——";
      opt.disabled = true;
      createLLMConfig.appendChild(opt);
      return;
    }
    configs
      .filter((c) => c.has_api_key && (c.provider_kind === "text" || !c.provider_kind))
      .forEach((cfg) => {
        const opt = document.createElement("option");
        opt.value = String(cfg.id);
        opt.textContent = `${cfg.name} · ${cfg.model}`;
        createLLMConfig.appendChild(opt);
      });
  } catch (err) {
    createLLMConfig.innerHTML = "";
    const opt = document.createElement("option");
    opt.value = "";
    opt.textContent = `加载失败：${(err as Error).message}`;
    opt.disabled = true;
    createLLMConfig.appendChild(opt);
  }
}

function openCreateModal(): void {
  createName.value = "";
  createRPM.value = "60";
  createError.textContent = "";
  createModal.hidden = false;
  createName.focus();
}

function closeCreateModal(): void {
  createModal.hidden = true;
}

async function onSubmitCreate(): Promise<void> {
  const name = createName.value.trim();
  if (!name) {
    createError.textContent = "name 必填";
    return;
  }
  const llmConfigID = parseInt(createLLMConfig.value, 10);
  if (!Number.isFinite(llmConfigID) || llmConfigID <= 0) {
    createError.textContent = "请选择一个 LLM 配置";
    return;
  }
  const rpm = parseInt(createRPM.value, 10) || 60;

  createSubmitBtn.disabled = true;
  createError.textContent = "";
  try {
    const { data } = await createProxyToken({
      name,
      default_llm_config_id: llmConfigID,
      rate_limit_rpm: rpm,
    });
    if (data.error) {
      createError.textContent = data.error;
      return;
    }
    const tok = data.token;
    if (!tok || !tok.plaintext) {
      createError.textContent = "服务端未返回 plaintext，请联系管理员";
      return;
    }
    closeCreateModal();
    plaintextValue.value = tok.plaintext;
    setBaseURLInPlaintextModal();
    plaintextModal.hidden = false;
    void refreshTokenList();
  } catch (err) {
    createError.textContent = (err as Error).message;
  } finally {
    createSubmitBtn.disabled = false;
  }
}

async function onRevoke(tok: LLMProxyToken): Promise<void> {
  if (!confirm(`确认撤销 token "${tok.name}" (${tok.prefix}…)？\n撤销后无法恢复，依赖该 token 的客户端会立即收到 401。`)) {
    return;
  }
  try {
    await revokeProxyToken(tok.id);
    void refreshTokenList();
  } catch (err) {
    alert(`撤销失败：${(err as Error).message}`);
  }
}

async function copyToClipboard(value: string, btn: HTMLButtonElement): Promise<void> {
  try {
    await navigator.clipboard.writeText(value);
    const prev = btn.textContent;
    btn.textContent = "✓ 已复制";
    setTimeout(() => {
      btn.textContent = prev;
    }, 1500);
  } catch {
    // Fallback: select for manual copy
    const input = btn.previousElementSibling as HTMLInputElement | null;
    input?.select();
  }
}

newTokenBtn.addEventListener("click", openCreateModal);
createCloseBtn.addEventListener("click", closeCreateModal);
createCancelBtn.addEventListener("click", closeCreateModal);
createSubmitBtn.addEventListener("click", () => void onSubmitCreate());
plaintextDoneBtn.addEventListener("click", () => {
  plaintextModal.hidden = true;
  plaintextValue.value = "";
});
plaintextCopyBtn.addEventListener("click", () => void copyToClipboard(plaintextValue.value, plaintextCopyBtn));
plaintextBaseCopyBtn.addEventListener("click", () => void copyToClipboard(plaintextBaseURL.value, plaintextBaseCopyBtn));
logoutBtn.addEventListener("click", () => void logout().then(() => (window.location.href = "/login.html")));

async function bootstrap(): Promise<void> {
  await hydrateSiteBrand();
  await renderSidebarFoot();
  await loadLLMConfigs();
  await refreshTokenList();
}

void bootstrap();
