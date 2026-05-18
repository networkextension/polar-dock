// /llm-billing.html — LLM 定价 + 团队配额 + 申请 single-page UI.
//
// Members see:
//   - Marketplace section: every shared/platform LLM config + pricing +
//     my workspace's budget/spend/status. "申请" button posts an
//     application which auto-approves and creates a team_quota row at
//     the platform-default budget.
//
// Admins additionally see:
//   - A "✏ 价格" button on every marketplace row (open pricing modal).
//   - Team Quotas section: all (workspace, config) budgets, editable.
//   - Applications section: history of every workspace's requests.

import { requestJson } from "./api/http.js";
import { logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSidebarFoot, hydrateSiteBrand } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";

initStoredTheme();
bindThemeSync();

interface MarketRow {
  id: number;
  name: string;
  model: string;
  is_platform: boolean;
  owned_by_my_workspace: boolean;
  price_per_million_input_usd?: number | null;
  price_per_million_output_usd?: number | null;
  my_monthly_budget_usd?: number | null;
  my_month_spend_usd?: number | null;
  my_credit_balance_usd?: number | null;
  my_has_recharge?: boolean;
  my_application_status?: string;
}

interface TeamQuotaRow {
  id: number;
  llm_config_id: number;
  workspace_id: string;
  monthly_budget_usd: number;
  month_spend_usd: number;
}

interface ApplicationRow {
  id: number;
  llm_config_id: number;
  workspace_id: string;
  requested_by_user_id: string;
  status: string;
  note: string;
  applied_at: string;
  decided_at?: string;
  granted_budget_usd: number;
}

interface MeResponse {
  role?: string;
}

const billingStatus = byId<HTMLElement>("billingStatus");

const marketTable = byId<HTMLTableElement>("marketTable");
const marketTbody = byId<HTMLTableSectionElement>("marketTbody");
const marketEmpty = byId<HTMLElement>("marketEmpty");
const marketLoading = byId<HTMLElement>("marketLoading");
const marketCount = byId<HTMLElement>("marketCount");

const adminPricingSection = byId<HTMLElement>("adminPricingSection");
const adminQuotasSection = byId<HTMLElement>("adminQuotasSection");
const adminAppsSection = byId<HTMLElement>("adminAppsSection");
const quotasTable = byId<HTMLTableElement>("quotasTable");
const quotasTbody = byId<HTMLTableSectionElement>("quotasTbody");
const quotasEmpty = byId<HTMLElement>("quotasEmpty");
const quotasCount = byId<HTMLElement>("quotasCount");
const appsTable = byId<HTMLTableElement>("appsTable");
const appsTbody = byId<HTMLTableSectionElement>("appsTbody");
const appsEmpty = byId<HTMLElement>("appsEmpty");
const appsCount = byId<HTMLElement>("appsCount");

const pricingModal = byId<HTMLElement>("pricingModal");
const pricingTitle = byId<HTMLElement>("pricingTitle");
const pricingCloseBtn = byId<HTMLButtonElement>("pricingCloseBtn");
const pricingCancelBtn = byId<HTMLButtonElement>("pricingCancelBtn");
const pricingSubmitBtn = byId<HTMLButtonElement>("pricingSubmitBtn");
const pricingInput = byId<HTMLInputElement>("pricingInput");
const pricingOutput = byId<HTMLInputElement>("pricingOutput");
const pricingError = byId<HTMLElement>("pricingError");

const applyModal = byId<HTMLElement>("applyModal");
const applyTitle = byId<HTMLElement>("applyTitle");
const applyCloseBtn = byId<HTMLButtonElement>("applyCloseBtn");
const applyCancelBtn = byId<HTMLButtonElement>("applyCancelBtn");
const applySubmitBtn = byId<HTMLButtonElement>("applySubmitBtn");
const applyNote = byId<HTMLInputElement>("applyNote");
const applyError = byId<HTMLElement>("applyError");

const quotaModal = byId<HTMLElement>("quotaModal");
const quotaTitle = byId<HTMLElement>("quotaTitle");
const quotaCloseBtn = byId<HTMLButtonElement>("quotaCloseBtn");
const quotaCancelBtn = byId<HTMLButtonElement>("quotaCancelBtn");
const quotaSubmitBtn = byId<HTMLButtonElement>("quotaSubmitBtn");
const quotaBudget = byId<HTMLInputElement>("quotaBudget");
const quotaError = byId<HTMLElement>("quotaError");

const rechargeModal = byId<HTMLElement>("rechargeModal");
const rechargeTitle = byId<HTMLElement>("rechargeTitle");
const rechargeCloseBtn = byId<HTMLButtonElement>("rechargeCloseBtn");
const rechargeCancelBtn = byId<HTMLButtonElement>("rechargeCancelBtn");
const rechargeSubmitBtn = byId<HTMLButtonElement>("rechargeSubmitBtn");
const rechargeAmount = byId<HTMLInputElement>("rechargeAmount");
const rechargeNote = byId<HTMLInputElement>("rechargeNote");
const rechargeError = byId<HTMLElement>("rechargeError");

const logoutBtn = byId<HTMLButtonElement>("logoutBtn");

let isAdmin = false;
let myWorkspaceID = "";
let pricingTargetId = 0;
let applyTargetId = 0;
let quotaTarget: { llm_config_id: number; workspace_id: string } | null = null;
let rechargeTarget: { llm_config_id: number; workspace_id: string; name: string } | null = null;
let marketRows: MarketRow[] = [];

function setStatus(text: string): void {
  if (billingStatus) {
    billingStatus.textContent = text;
  }
}

function fmtUSD(v: number | null | undefined): string {
  if (v === null || v === undefined) return "—";
  return `$${v.toFixed(4)}`;
}

function fmtBudget(v: number | null | undefined): string {
  if (v === null || v === undefined) return "—";
  return `$${v.toFixed(2)}`;
}

function openModal(el: HTMLElement | null): void {
  if (el) el.hidden = false;
}

function closeModal(el: HTMLElement | null): void {
  if (el) el.hidden = true;
}

function renderMarket(): void {
  if (marketLoading) marketLoading.hidden = true;
  if (!marketTbody || !marketTable || !marketEmpty || !marketCount) return;
  marketTbody.innerHTML = "";
  if (marketRows.length === 0) {
    marketTable.hidden = true;
    marketEmpty.hidden = false;
    marketCount.textContent = "0";
    return;
  }
  marketTable.hidden = false;
  marketEmpty.hidden = true;
  marketCount.textContent = String(marketRows.length);

  for (const row of marketRows) {
    const tr = document.createElement("tr");

    const tdName = document.createElement("td");
    tdName.textContent = row.name;
    if (row.is_platform) {
      const tag = document.createElement("span");
      tag.className = "badge";
      tag.style.marginLeft = "6px";
      tag.textContent = "platform";
      tdName.appendChild(tag);
    }
    tr.appendChild(tdName);

    const tdModel = document.createElement("td");
    tdModel.textContent = row.model;
    tr.appendChild(tdModel);

    const tdIn = document.createElement("td");
    tdIn.textContent = fmtUSD(row.price_per_million_input_usd);
    tr.appendChild(tdIn);
    const tdOut = document.createElement("td");
    tdOut.textContent = fmtUSD(row.price_per_million_output_usd);
    tr.appendChild(tdOut);

    const tdSpend = document.createElement("td");
    tdSpend.textContent = fmtBudget(row.my_month_spend_usd);
    tr.appendChild(tdSpend);

    const tdBudget = document.createElement("td");
    tdBudget.textContent = fmtBudget(row.my_monthly_budget_usd);
    tr.appendChild(tdBudget);

    const tdBalance = document.createElement("td");
    const bal = row.my_credit_balance_usd ?? 0;
    if (row.my_has_recharge) {
      const color = bal <= 0 ? "var(--danger, #c00)" : (bal < 5 ? "var(--warn, #c80)" : "var(--ok, #0a7a3a)");
      tdBalance.innerHTML = `<span style="color: ${color}; font-weight: 600;">${fmtBudget(bal)}</span>`;
    } else {
      tdBalance.innerHTML = '<span class="meta-subtitle">未充值</span>';
    }
    tr.appendChild(tdBalance);

    const tdStatus = document.createElement("td");
    const hasBudget = row.my_monthly_budget_usd !== null && row.my_monthly_budget_usd !== undefined;
    const used = row.my_month_spend_usd ?? 0;
    const budget = row.my_monthly_budget_usd ?? 0;
    // Priority: credit exhausted > monthly exceeded > opened > owned-no-cap > applied-pending > none
    if (row.my_has_recharge && bal <= 0) {
      tdStatus.innerHTML = '<span style="color: var(--danger, #c00);">余额耗尽</span>';
    } else if (hasBudget && budget > 0 && used >= budget) {
      tdStatus.innerHTML = '<span style="color: var(--danger, #c00);">月度超额</span>';
    } else if (row.my_has_recharge) {
      tdStatus.innerHTML = '<span style="color: var(--ok, #0a7a3a);">✓ 已充值</span>';
    } else if (hasBudget) {
      tdStatus.innerHTML = '<span style="color: var(--ok, #0a7a3a);">✓ 已开通</span>';
    } else if (row.owned_by_my_workspace) {
      tdStatus.innerHTML = '<span style="color: var(--ok, #0a7a3a);">✓ 自有 · 未设额度</span>';
    } else if (row.my_application_status) {
      tdStatus.textContent = row.my_application_status;
    } else {
      tdStatus.innerHTML = '<span class="meta-subtitle">未申请</span>';
    }
    tr.appendChild(tdStatus);

    const tdAct = document.createElement("td");
    tdAct.style.whiteSpace = "nowrap";
    if (!hasBudget) {
      const btn = document.createElement("button");
      btn.className = "btn-inline btn-primary";
      btn.textContent = "申请";
      btn.addEventListener("click", () => openApplyModal(row));
      tdAct.appendChild(btn);
    }
    if (isAdmin) {
      const btnP = document.createElement("button");
      btnP.className = "btn-inline btn-secondary";
      btnP.style.marginLeft = "6px";
      btnP.textContent = "✏ 价格";
      btnP.addEventListener("click", () => openPricingModal(row));
      tdAct.appendChild(btnP);

      const btnR = document.createElement("button");
      btnR.className = "btn-inline btn-secondary";
      btnR.style.marginLeft = "6px";
      btnR.textContent = "💰 充值";
      btnR.addEventListener("click", () => openRechargeModal(row, myWorkspaceID));
      tdAct.appendChild(btnR);
    }
    tr.appendChild(tdAct);

    marketTbody.appendChild(tr);
  }
}

async function loadMarket(): Promise<void> {
  setStatus("加载市场…");
  try {
    const { data } = await requestJson<{ items: MarketRow[] }>("/api/llm-configs/marketplace");
    marketRows = data.items ?? [];
    renderMarket();
    setStatus("");
  } catch (err) {
    setStatus("加载市场失败");
    console.error(err);
  }
}

function openApplyModal(row: MarketRow): void {
  applyTargetId = row.id;
  if (applyTitle) applyTitle.textContent = `申请：${row.name}`;
  if (applyNote) applyNote.value = "";
  if (applyError) applyError.textContent = "";
  openModal(applyModal);
}

async function submitApply(): Promise<void> {
  if (!applyTargetId) return;
  if (applyError) applyError.textContent = "";
  try {
    const note = applyNote?.value?.trim() ?? "";
    await requestJson<{ application: ApplicationRow; granted_budget_usd: number }>(
      "/api/llm-config-applications",
      {
        method: "POST",
        body: { llm_config_id: applyTargetId, note },
      },
    );
    closeModal(applyModal);
    await loadMarket();
    if (isAdmin) {
      await Promise.all([loadQuotas(), loadApplications()]);
    }
  } catch (err) {
    if (applyError) applyError.textContent = `申请失败：${(err as Error).message}`;
  }
}

function openPricingModal(row: MarketRow): void {
  pricingTargetId = row.id;
  if (pricingTitle) pricingTitle.textContent = `编辑单价：${row.name}`;
  if (pricingInput) pricingInput.value = row.price_per_million_input_usd != null ? String(row.price_per_million_input_usd) : "";
  if (pricingOutput) pricingOutput.value = row.price_per_million_output_usd != null ? String(row.price_per_million_output_usd) : "";
  if (pricingError) pricingError.textContent = "";
  openModal(pricingModal);
}

async function submitPricing(): Promise<void> {
  if (!pricingTargetId) return;
  if (pricingError) pricingError.textContent = "";
  const inVal = pricingInput?.value?.trim() ?? "";
  const outVal = pricingOutput?.value?.trim() ?? "";
  const body: { price_per_million_input_usd: number | null; price_per_million_output_usd: number | null } = {
    price_per_million_input_usd: inVal === "" ? null : Number(inVal),
    price_per_million_output_usd: outVal === "" ? null : Number(outVal),
  };
  try {
    await requestJson(`/api/admin/llm-configs/${pricingTargetId}/pricing`, {
      method: "PUT",
      body,
    });
    closeModal(pricingModal);
    await loadMarket();
  } catch (err) {
    if (pricingError) pricingError.textContent = `保存失败：${(err as Error).message}`;
  }
}

function openQuotaModal(row: TeamQuotaRow): void {
  quotaTarget = { llm_config_id: row.llm_config_id, workspace_id: row.workspace_id };
  if (quotaTitle) quotaTitle.textContent = `编辑预算：${row.workspace_id} · LLM #${row.llm_config_id}`;
  if (quotaBudget) quotaBudget.value = String(row.monthly_budget_usd);
  if (quotaError) quotaError.textContent = "";
  openModal(quotaModal);
}

async function submitQuota(): Promise<void> {
  if (!quotaTarget) return;
  if (quotaError) quotaError.textContent = "";
  const v = Number(quotaBudget?.value ?? 0);
  if (Number.isNaN(v) || v < 0) {
    if (quotaError) quotaError.textContent = "预算必须是非负数";
    return;
  }
  try {
    await requestJson("/api/admin/llm-config-team-quotas", {
      method: "PUT",
      body: {
        llm_config_id: quotaTarget.llm_config_id,
        workspace_id: quotaTarget.workspace_id,
        monthly_budget_usd: v,
      },
    });
    closeModal(quotaModal);
    await Promise.all([loadQuotas(), loadMarket()]);
  } catch (err) {
    if (quotaError) quotaError.textContent = `保存失败：${(err as Error).message}`;
  }
}

async function loadQuotas(): Promise<void> {
  if (!quotasTbody || !quotasTable || !quotasEmpty || !quotasCount) return;
  try {
    const { data } = await requestJson<{ items: TeamQuotaRow[] }>("/api/admin/llm-config-team-quotas");
    const items = data.items ?? [];
    quotasTbody.innerHTML = "";
    if (items.length === 0) {
      quotasTable.hidden = true;
      quotasEmpty.hidden = false;
      quotasCount.textContent = "0";
      return;
    }
    quotasTable.hidden = false;
    quotasEmpty.hidden = true;
    quotasCount.textContent = String(items.length);
    for (const row of items) {
      const tr = document.createElement("tr");
      const tdWs = document.createElement("td");
      tdWs.textContent = row.workspace_id;
      tr.appendChild(tdWs);
      const tdCfg = document.createElement("td");
      tdCfg.textContent = `#${row.llm_config_id}`;
      tr.appendChild(tdCfg);
      const tdSpend = document.createElement("td");
      tdSpend.textContent = fmtBudget(row.month_spend_usd);
      tr.appendChild(tdSpend);
      const tdBudget = document.createElement("td");
      tdBudget.textContent = fmtBudget(row.monthly_budget_usd);
      tr.appendChild(tdBudget);
      const tdAct = document.createElement("td");
      const btn = document.createElement("button");
      btn.className = "btn-inline btn-secondary";
      btn.textContent = "✏ 改预算";
      btn.addEventListener("click", () => openQuotaModal(row));
      tdAct.appendChild(btn);
      tr.appendChild(tdAct);
      quotasTbody.appendChild(tr);
    }
  } catch (err) {
    console.error(err);
  }
}

async function loadApplications(): Promise<void> {
  if (!appsTbody || !appsTable || !appsEmpty || !appsCount) return;
  try {
    const { data } = await requestJson<{ items: ApplicationRow[] }>("/api/admin/llm-config-applications");
    const items = data.items ?? [];
    appsTbody.innerHTML = "";
    if (items.length === 0) {
      appsTable.hidden = true;
      appsEmpty.hidden = false;
      appsCount.textContent = "0";
      return;
    }
    appsTable.hidden = false;
    appsEmpty.hidden = true;
    appsCount.textContent = String(items.length);
    for (const row of items) {
      const tr = document.createElement("tr");
      const tdT = document.createElement("td");
      tdT.textContent = new Date(row.applied_at).toLocaleString();
      tr.appendChild(tdT);
      const tdWs = document.createElement("td");
      tdWs.textContent = row.workspace_id;
      tr.appendChild(tdWs);
      const tdU = document.createElement("td");
      tdU.textContent = row.requested_by_user_id;
      tr.appendChild(tdU);
      const tdCfg = document.createElement("td");
      tdCfg.textContent = `#${row.llm_config_id}`;
      tr.appendChild(tdCfg);
      const tdS = document.createElement("td");
      tdS.textContent = row.status;
      tr.appendChild(tdS);
      const tdG = document.createElement("td");
      tdG.textContent = fmtBudget(row.granted_budget_usd);
      tr.appendChild(tdG);
      const tdN = document.createElement("td");
      tdN.textContent = row.note;
      tr.appendChild(tdN);
      appsTbody.appendChild(tr);
    }
  } catch (err) {
    console.error(err);
  }
}

function openRechargeModal(row: MarketRow, workspaceID: string): void {
  rechargeTarget = { llm_config_id: row.id, workspace_id: workspaceID, name: row.name };
  if (rechargeTitle) rechargeTitle.textContent = `充值：${row.name}（当前余额 ${fmtBudget(row.my_credit_balance_usd ?? 0)}）`;
  if (rechargeAmount) rechargeAmount.value = "";
  if (rechargeNote) rechargeNote.value = "";
  if (rechargeError) rechargeError.textContent = "";
  openModal(rechargeModal);
}

async function submitRecharge(): Promise<void> {
  if (!rechargeTarget) return;
  if (rechargeError) rechargeError.textContent = "";
  const amt = Number(rechargeAmount?.value ?? 0);
  if (Number.isNaN(amt) || amt === 0) {
    if (rechargeError) rechargeError.textContent = "金额必须为非零数字（允许负数清退）";
    return;
  }
  try {
    await requestJson("/api/admin/llm-config-team-quotas/recharge", {
      method: "POST",
      body: {
        llm_config_id: rechargeTarget.llm_config_id,
        workspace_id: rechargeTarget.workspace_id,
        amount_usd: amt,
        note: rechargeNote?.value?.trim() ?? "",
      },
    });
    closeModal(rechargeModal);
    await loadMarket();
  } catch (err) {
    if (rechargeError) rechargeError.textContent = `充值失败：${(err as Error).message}`;
  }
}

function readActiveWorkspaceID(): string {
  try {
    return localStorage.getItem("polar_active_workspace_id") || "";
  } catch {
    return "";
  }
}

async function bootstrap(): Promise<void> {
  hydrateSiteBrand();
  hydrateSidebarFoot();
  try {
    const { data } = await requestJson<MeResponse>("/api/me");
    isAdmin = data?.role === "admin";
  } catch {
    isAdmin = false;
  }
  myWorkspaceID = readActiveWorkspaceID();
  if (isAdmin) {
    if (adminPricingSection) adminPricingSection.hidden = false;
    if (adminQuotasSection) adminQuotasSection.hidden = false;
    if (adminAppsSection) adminAppsSection.hidden = false;
  }
  await loadMarket();
  if (isAdmin) {
    await Promise.all([loadQuotas(), loadApplications()]);
  }
}

// Wire modal buttons
pricingCloseBtn?.addEventListener("click", () => closeModal(pricingModal));
pricingCancelBtn?.addEventListener("click", () => closeModal(pricingModal));
pricingSubmitBtn?.addEventListener("click", () => void submitPricing());

applyCloseBtn?.addEventListener("click", () => closeModal(applyModal));
applyCancelBtn?.addEventListener("click", () => closeModal(applyModal));
applySubmitBtn?.addEventListener("click", () => void submitApply());

quotaCloseBtn?.addEventListener("click", () => closeModal(quotaModal));
quotaCancelBtn?.addEventListener("click", () => closeModal(quotaModal));
quotaSubmitBtn?.addEventListener("click", () => void submitQuota());

rechargeCloseBtn?.addEventListener("click", () => closeModal(rechargeModal));
rechargeCancelBtn?.addEventListener("click", () => closeModal(rechargeModal));
rechargeSubmitBtn?.addEventListener("click", () => void submitRecharge());

logoutBtn?.addEventListener("click", () => {
  void logout().then(() => {
    window.location.href = "/login.html";
  });
});

void bootstrap();
