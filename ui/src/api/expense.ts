// Typed wrappers for /api/expenses + /api/expense-categories.

import { request, requestJson } from "./http.js";

export interface ExpenseCategory {
  id: number;
  workspace_id: string;
  name: string;
  icon: string;
  color: string;
  sort_order: number;
  is_hidden: boolean;
  created_at: string;
}

export interface Expense {
  id: number;
  workspace_id: string;
  created_by_user_id: string;
  amount: number;
  currency: string;
  merchant: string;
  category_id?: number | null;
  consume_time: string;
  has_detail_time: boolean;
  region: string;
  raw_image_id?: string | null;
  status: number;
  confidence: number;
  remark: string;
  created_at: string;
  updated_at: string;
}

// ---- categories ----

export async function fetchExpenseCategories() {
  return requestJson<{ categories: ExpenseCategory[] }>("/api/expense-categories");
}

export async function createExpenseCategory(body: {
  name: string;
  icon?: string;
  color?: string;
  sort_order?: number;
}) {
  return requestJson<{ category: ExpenseCategory }>("/api/expense-categories", {
    method: "POST",
    body,
  });
}

export async function updateExpenseCategory(
  id: number,
  body: Partial<{ name: string; icon: string; color: string; sort_order: number; is_hidden: boolean }>,
) {
  return requestJson<{ ok: boolean }>(`/api/expense-categories/${id}`, {
    method: "PUT",
    body,
  });
}

export async function deleteExpenseCategory(id: number) {
  return request(`/api/expense-categories/${id}`, { method: "DELETE" });
}

// ---- expenses ----

export interface ExpenseListFilter {
  start?: string;
  end?: string;
  category_id?: number;
  status?: number;
  limit?: number;
  offset?: number;
}

export async function fetchExpenses(filter: ExpenseListFilter = {}) {
  const q = new URLSearchParams();
  if (filter.start) q.set("start", filter.start);
  if (filter.end) q.set("end", filter.end);
  if (filter.category_id != null) q.set("category_id", String(filter.category_id));
  if (filter.status != null) q.set("status", String(filter.status));
  if (filter.limit != null) q.set("limit", String(filter.limit));
  if (filter.offset != null) q.set("offset", String(filter.offset));
  const s = q.toString();
  return requestJson<{ expenses: Expense[]; total: number }>(
    `/api/expenses${s ? "?" + s : ""}`,
  );
}

export async function createExpense(body: {
  amount: number;
  currency?: string;
  merchant?: string;
  category_id?: number | null;
  consume_time?: string;
  has_detail_time?: boolean;
  region?: string;
  remark?: string;
}) {
  return requestJson<{ expense: Expense }>("/api/expenses", {
    method: "POST",
    body,
  });
}

export async function updateExpense(
  id: number,
  body: Partial<{
    amount: number;
    currency: string;
    merchant: string;
    category_id: number | null;
    clear_category: boolean;
    consume_time: string;
    has_detail_time: boolean;
    region: string;
    status: number;
    remark: string;
  }>,
) {
  return requestJson<{ ok: boolean }>(`/api/expenses/${id}`, {
    method: "PUT",
    body,
  });
}

export async function deleteExpense(id: number) {
  return request(`/api/expenses/${id}`, { method: "DELETE" });
}
