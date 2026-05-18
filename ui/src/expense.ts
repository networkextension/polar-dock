// /expense.html — 家庭账本 L1 single-page UI.
//
// Two tabs: 流水 (CRUD over the workspace's expenses) + 分类 (manage
// the per-workspace category dictionary). Both seeded on first load:
// listExpenseCategories backend bootstraps 7 presets if empty.
//
// OCR/LLM extract → L2 (separate PR).

import {
  createExpense,
  createExpenseCategory,
  deleteExpense,
  deleteExpenseCategory,
  fetchExpenseCategories,
  fetchExpenses,
  updateExpense,
  updateExpenseCategory,
  type Expense,
  type ExpenseCategory,
  type ExpenseListFilter,
} from "./api/expense.js";
import { logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSidebarFoot, hydrateSiteBrand } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";

initStoredTheme();
bindThemeSync();

// ---- DOM refs ----
const tabExpensesBtn = byId<HTMLButtonElement>("tabExpenses");
const tabCategoriesBtn = byId<HTMLButtonElement>("tabCategories");
const paneExpenses = byId<HTMLElement>("paneExpenses");
const paneCategories = byId<HTMLElement>("paneCategories");

const expenseTable = byId<HTMLTableElement>("expenseTable");
const expenseTbody = byId<HTMLTableSectionElement>("expenseTbody");
const expenseEmpty = byId<HTMLElement>("expenseEmpty");
const expenseLoading = byId<HTMLElement>("expenseLoading");
const expenseCount = byId<HTMLElement>("expenseCount");
const expenseTotal = byId<HTMLElement>("expenseTotal");
const filterPeriod = byId<HTMLSelectElement>("filterPeriod");
const filterCategory = byId<HTMLSelectElement>("filterCategory");
const expenseRefreshBtn = byId<HTMLButtonElement>("expenseRefreshBtn");
const newExpenseBtn = byId<HTMLButtonElement>("newExpenseBtn");

const expenseModal = byId<HTMLElement>("expenseModal");
const expenseModalTitle = byId<HTMLElement>("expenseModalTitle");
const expenseModalCloseBtn = byId<HTMLButtonElement>("expenseModalCloseBtn");
const expenseModalCancelBtn = byId<HTMLButtonElement>("expenseModalCancelBtn");
const expenseModalSubmitBtn = byId<HTMLButtonElement>("expenseModalSubmitBtn");
const expenseAmount = byId<HTMLInputElement>("expenseAmount");
const expenseMerchant = byId<HTMLInputElement>("expenseMerchant");
const expenseCategorySel = byId<HTMLSelectElement>("expenseCategory");
const expenseConsumeTime = byId<HTMLInputElement>("expenseConsumeTime");
const expenseDateOnly = byId<HTMLInputElement>("expenseDateOnly");
const expenseRegion = byId<HTMLInputElement>("expenseRegion");
const expenseRemark = byId<HTMLTextAreaElement>("expenseRemark");
const expenseFormError = byId<HTMLElement>("expenseFormError");

const categoryTable = byId<HTMLTableElement>("categoryTable");
const categoryTbody = byId<HTMLTableSectionElement>("categoryTbody");
const categoryEmpty = byId<HTMLElement>("categoryEmpty");
const categoryCount = byId<HTMLElement>("categoryCount");
const categoryRefreshBtn = byId<HTMLButtonElement>("categoryRefreshBtn");
const newCategoryBtn = byId<HTMLButtonElement>("newCategoryBtn");

const categoryModal = byId<HTMLElement>("categoryModal");
const categoryModalTitle = byId<HTMLElement>("categoryModalTitle");
const categoryModalCloseBtn = byId<HTMLButtonElement>("categoryModalCloseBtn");
const categoryModalCancelBtn = byId<HTMLButtonElement>("categoryModalCancelBtn");
const categoryModalSubmitBtn = byId<HTMLButtonElement>("categoryModalSubmitBtn");
const categoryNameInput = byId<HTMLInputElement>("categoryName");
const categoryIconInput = byId<HTMLInputElement>("categoryIcon");
const categoryColorInput = byId<HTMLInputElement>("categoryColor");
const categorySortOrderInput = byId<HTMLInputElement>("categorySortOrder");
const categoryFormError = byId<HTMLElement>("categoryFormError");

const logoutBtn = byId<HTMLButtonElement>("logoutBtn");

// ---- state ----
let categories: ExpenseCategory[] = [];
let editingExpenseID: number | null = null;
let editingCategoryID: number | null = null;

// ---- format helpers ----
function fmtAmount(amount: number, currency: string): string {
  const symbol = currency === "CNY" ? "¥" : currency === "USD" ? "$" : currency === "JPY" ? "¥" : "";
  return `${symbol}${amount.toFixed(2)}`;
}

function fmtConsumeTime(iso: string, hasDetail: boolean): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const y = d.getFullYear();
  const mo = String(d.getMonth() + 1).padStart(2, "0");
  const da = String(d.getDate()).padStart(2, "0");
  if (!hasDetail) return `${y}-${mo}-${da}`;
  const h = String(d.getHours()).padStart(2, "0");
  const mi = String(d.getMinutes()).padStart(2, "0");
  return `${y}-${mo}-${da} ${h}:${mi}`;
}

function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

// ---- filter -> date range ----
function currentPeriodRange(): { start?: string; end?: string } {
  const v = filterPeriod.value;
  const now = new Date();
  const fmt = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  if (v === "month") {
    const start = new Date(now.getFullYear(), now.getMonth(), 1);
    const end = new Date(now.getFullYear(), now.getMonth() + 1, 1);
    return { start: fmt(start), end: fmt(end) };
  }
  if (v === "last_month") {
    const start = new Date(now.getFullYear(), now.getMonth() - 1, 1);
    const end = new Date(now.getFullYear(), now.getMonth(), 1);
    return { start: fmt(start), end: fmt(end) };
  }
  if (v === "30d") {
    const start = new Date();
    start.setDate(start.getDate() - 30);
    return { start: fmt(start) };
  }
  return {};
}

// ---- tab switching ----
function switchTab(tab: "expenses" | "categories"): void {
  if (tab === "expenses") {
    paneExpenses.hidden = false;
    paneCategories.hidden = true;
    tabExpensesBtn.classList.add("lp-tab-active");
    tabCategoriesBtn.classList.remove("lp-tab-active");
    void loadExpenses();
  } else {
    paneExpenses.hidden = true;
    paneCategories.hidden = false;
    tabExpensesBtn.classList.remove("lp-tab-active");
    tabCategoriesBtn.classList.add("lp-tab-active");
    renderCategoryTable();
  }
}

tabExpensesBtn.addEventListener("click", () => switchTab("expenses"));
tabCategoriesBtn.addEventListener("click", () => switchTab("categories"));

// ---- expenses ----

async function loadExpenses(): Promise<void> {
  expenseLoading.hidden = false;
  expenseTable.hidden = true;
  expenseEmpty.hidden = true;
  const range = currentPeriodRange();
  const filter: ExpenseListFilter = { ...range, limit: 200 };
  const catVal = filterCategory.value;
  if (catVal) filter.category_id = parseInt(catVal, 10);
  try {
    const { data } = await fetchExpenses(filter);
    const items = data.expenses ?? [];
    expenseTbody.innerHTML = "";
    items.forEach((e) => expenseTbody.appendChild(renderExpenseRow(e)));
    const totalAmt = items.reduce((s, e) => s + e.amount, 0);
    expenseCount.textContent = `${data.total} 条`;
    expenseTotal.textContent = `合计 ¥${totalAmt.toFixed(2)}`;
    expenseTable.hidden = items.length === 0;
    expenseEmpty.hidden = items.length !== 0;
  } catch (err) {
    expenseEmpty.hidden = false;
    expenseEmpty.textContent = `加载失败：${(err as Error).message}`;
  } finally {
    expenseLoading.hidden = true;
  }
}

function renderExpenseRow(e: Expense): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (e.status === 0) tr.style.background = "rgba(245, 158, 11, 0.06)"; // 待确认 amber tint

  const tdTime = document.createElement("td");
  tdTime.textContent = fmtConsumeTime(e.consume_time, e.has_detail_time);
  tdTime.style.whiteSpace = "nowrap";

  const tdMerchant = document.createElement("td");
  tdMerchant.textContent = e.merchant || "—";

  const tdCat = document.createElement("td");
  const cat = e.category_id != null ? categories.find((c) => c.id === e.category_id) : null;
  if (cat) {
    tdCat.innerHTML = `<span style="display:inline-flex; align-items:center; gap:4px;"><span>${escapeHTML(cat.icon)}</span><span>${escapeHTML(cat.name)}</span></span>`;
  } else {
    tdCat.innerHTML = '<span class="meta-subtitle">未分类</span>';
  }

  const tdAmt = document.createElement("td");
  tdAmt.style.textAlign = "right";
  tdAmt.style.fontWeight = "600";
  tdAmt.textContent = fmtAmount(e.amount, e.currency);

  const tdRegion = document.createElement("td");
  tdRegion.textContent = e.region || "—";

  const tdRemark = document.createElement("td");
  tdRemark.textContent = e.remark || "";
  tdRemark.style.maxWidth = "200px";
  tdRemark.style.overflow = "hidden";
  tdRemark.style.textOverflow = "ellipsis";
  tdRemark.style.whiteSpace = "nowrap";
  if (e.remark) tdRemark.title = e.remark;

  const tdAct = document.createElement("td");
  const editBtn = document.createElement("button");
  editBtn.type = "button";
  editBtn.className = "btn-inline btn-secondary";
  editBtn.textContent = "✎";
  editBtn.title = "编辑";
  editBtn.style.marginRight = "4px";
  editBtn.addEventListener("click", () => openExpenseModal(e));
  const delBtn = document.createElement("button");
  delBtn.type = "button";
  delBtn.className = "btn-inline btn-secondary";
  delBtn.textContent = "🗑";
  delBtn.title = "删除";
  delBtn.addEventListener("click", () => onDeleteExpense(e));
  tdAct.append(editBtn, delBtn);

  tr.append(tdTime, tdMerchant, tdCat, tdAmt, tdRegion, tdRemark, tdAct);
  return tr;
}

async function onDeleteExpense(e: Expense): Promise<void> {
  if (!confirm(`确认删除 ${fmtAmount(e.amount, e.currency)} · ${e.merchant || "—"}?`)) return;
  try {
    await deleteExpense(e.id);
    void loadExpenses();
  } catch (err) {
    alert(`删除失败：${(err as Error).message}`);
  }
}

function openExpenseModal(e: Expense | null): void {
  editingExpenseID = e ? e.id : null;
  expenseModalTitle.textContent = e ? "编辑流水" : "手工记账";
  expenseFormError.textContent = "";
  expenseAmount.value = e ? String(e.amount) : "";
  expenseMerchant.value = e?.merchant ?? "";
  expenseCategorySel.value = e?.category_id != null ? String(e.category_id) : "";
  expenseRegion.value = e?.region ?? "";
  expenseRemark.value = e?.remark ?? "";
  // Default new entry to "now"; preserve existing time on edit.
  const t = e ? new Date(e.consume_time) : new Date();
  expenseDateOnly.checked = e ? !e.has_detail_time : false;
  expenseConsumeTime.value = toDatetimeLocalString(t);
  expenseModal.hidden = false;
}

function toDatetimeLocalString(d: Date): string {
  // input[type=datetime-local] wants "YYYY-MM-DDTHH:mm" in LOCAL time
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

async function submitExpense(): Promise<void> {
  expenseFormError.textContent = "";
  const amount = parseFloat(expenseAmount.value);
  if (!Number.isFinite(amount) || amount <= 0) {
    expenseFormError.textContent = "金额必填，且 > 0";
    return;
  }
  if (!expenseConsumeTime.value) {
    expenseFormError.textContent = "请选消费时间";
    return;
  }
  const consumeISO = new Date(expenseConsumeTime.value).toISOString();
  const catStr = expenseCategorySel.value;
  const categoryID = catStr ? parseInt(catStr, 10) : null;

  try {
    if (editingExpenseID == null) {
      await createExpense({
        amount,
        merchant: expenseMerchant.value.trim(),
        category_id: categoryID,
        consume_time: consumeISO,
        has_detail_time: !expenseDateOnly.checked,
        region: expenseRegion.value.trim(),
        remark: expenseRemark.value.trim(),
      });
    } else {
      await updateExpense(editingExpenseID, {
        amount,
        merchant: expenseMerchant.value.trim(),
        category_id: categoryID ?? undefined,
        clear_category: categoryID == null,
        consume_time: consumeISO,
        has_detail_time: !expenseDateOnly.checked,
        region: expenseRegion.value.trim(),
        remark: expenseRemark.value.trim(),
      });
    }
    expenseModal.hidden = true;
    void loadExpenses();
  } catch (err) {
    expenseFormError.textContent = `保存失败：${(err as Error).message}`;
  }
}

expenseRefreshBtn.addEventListener("click", () => void loadExpenses());
newExpenseBtn.addEventListener("click", () => openExpenseModal(null));
filterPeriod.addEventListener("change", () => void loadExpenses());
filterCategory.addEventListener("change", () => void loadExpenses());
expenseModalCloseBtn.addEventListener("click", () => (expenseModal.hidden = true));
expenseModalCancelBtn.addEventListener("click", () => (expenseModal.hidden = true));
expenseModalSubmitBtn.addEventListener("click", () => void submitExpense());

// ---- categories ----

async function loadCategories(): Promise<void> {
  const { data } = await fetchExpenseCategories();
  categories = data.categories ?? [];
  // Sync the filter dropdown + the form dropdown
  filterCategory.innerHTML = '<option value="">全部</option>';
  expenseCategorySel.innerHTML = '<option value="">未分类</option>';
  for (const c of categories) {
    if (!c.is_hidden) {
      const opt = document.createElement("option");
      opt.value = String(c.id);
      opt.textContent = `${c.icon} ${c.name}`;
      filterCategory.appendChild(opt);
      expenseCategorySel.appendChild(opt.cloneNode(true));
    }
  }
}

function renderCategoryTable(): void {
  categoryCount.textContent = `${categories.length} 个`;
  categoryTbody.innerHTML = "";
  if (categories.length === 0) {
    categoryTable.hidden = true;
    categoryEmpty.hidden = false;
    return;
  }
  categoryTable.hidden = false;
  categoryEmpty.hidden = true;
  for (const c of categories) {
    const tr = document.createElement("tr");
    if (c.is_hidden) tr.style.opacity = "0.5";

    const tdIcon = document.createElement("td");
    tdIcon.style.fontSize = "20px";
    tdIcon.textContent = c.icon || "•";

    const tdName = document.createElement("td");
    tdName.textContent = c.name;

    const tdColor = document.createElement("td");
    tdColor.innerHTML = `<span style="display:inline-block; width:24px; height:16px; background:${c.color}; border-radius:3px;"></span>`;

    const tdSort = document.createElement("td");
    tdSort.textContent = String(c.sort_order);

    const tdHidden = document.createElement("td");
    tdHidden.textContent = c.is_hidden ? "✓" : "";

    const tdAct = document.createElement("td");
    const editBtn = document.createElement("button");
    editBtn.type = "button";
    editBtn.className = "btn-inline btn-secondary";
    editBtn.textContent = "✎";
    editBtn.title = "编辑";
    editBtn.style.marginRight = "4px";
    editBtn.addEventListener("click", () => openCategoryModal(c));
    const toggleBtn = document.createElement("button");
    toggleBtn.type = "button";
    toggleBtn.className = "btn-inline btn-secondary";
    toggleBtn.textContent = c.is_hidden ? "显示" : "隐藏";
    toggleBtn.style.marginRight = "4px";
    toggleBtn.addEventListener("click", async () => {
      try {
        await updateExpenseCategory(c.id, { is_hidden: !c.is_hidden });
        await loadCategories();
        renderCategoryTable();
      } catch (err) {
        alert(`操作失败：${(err as Error).message}`);
      }
    });
    const delBtn = document.createElement("button");
    delBtn.type = "button";
    delBtn.className = "btn-inline btn-secondary";
    delBtn.textContent = "🗑";
    delBtn.title = "删除（已关联的流水会变成未分类）";
    delBtn.addEventListener("click", () => onDeleteCategory(c));
    tdAct.append(editBtn, toggleBtn, delBtn);

    tr.append(tdIcon, tdName, tdColor, tdSort, tdHidden, tdAct);
    categoryTbody.appendChild(tr);
  }
}

async function onDeleteCategory(c: ExpenseCategory): Promise<void> {
  if (!confirm(`删除分类「${c.name}」?该分类下的流水会变成「未分类」。`)) return;
  try {
    await deleteExpenseCategory(c.id);
    await loadCategories();
    renderCategoryTable();
  } catch (err) {
    alert(`删除失败：${(err as Error).message}`);
  }
}

function openCategoryModal(c: ExpenseCategory | null): void {
  editingCategoryID = c ? c.id : null;
  categoryModalTitle.textContent = c ? "编辑分类" : "新建分类";
  categoryFormError.textContent = "";
  categoryNameInput.value = c?.name ?? "";
  categoryIconInput.value = c?.icon ?? "";
  categoryColorInput.value = c?.color ?? "#888888";
  categorySortOrderInput.value = String(c?.sort_order ?? 100);
  categoryModal.hidden = false;
}

async function submitCategory(): Promise<void> {
  categoryFormError.textContent = "";
  const name = categoryNameInput.value.trim();
  if (!name) {
    categoryFormError.textContent = "名称必填";
    return;
  }
  const sortOrder = parseInt(categorySortOrderInput.value, 10) || 100;
  try {
    if (editingCategoryID == null) {
      await createExpenseCategory({
        name,
        icon: categoryIconInput.value.trim(),
        color: categoryColorInput.value,
        sort_order: sortOrder,
      });
    } else {
      await updateExpenseCategory(editingCategoryID, {
        name,
        icon: categoryIconInput.value.trim(),
        color: categoryColorInput.value,
        sort_order: sortOrder,
      });
    }
    categoryModal.hidden = true;
    await loadCategories();
    renderCategoryTable();
  } catch (err) {
    categoryFormError.textContent = `保存失败：${(err as Error).message}`;
  }
}

categoryRefreshBtn.addEventListener("click", async () => {
  await loadCategories();
  renderCategoryTable();
});
newCategoryBtn.addEventListener("click", () => openCategoryModal(null));
categoryModalCloseBtn.addEventListener("click", () => (categoryModal.hidden = true));
categoryModalCancelBtn.addEventListener("click", () => (categoryModal.hidden = true));
categoryModalSubmitBtn.addEventListener("click", () => void submitCategory());

// ---- bootstrap ----

logoutBtn?.addEventListener("click", () => {
  void logout().then(() => {
    window.location.href = "/login.html";
  });
});

async function bootstrap(): Promise<void> {
  hydrateSiteBrand();
  hydrateSidebarFoot();
  await loadCategories(); // also bootstraps backend presets if empty
  await loadExpenses();
}

void bootstrap();
