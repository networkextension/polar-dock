import { buildAssetUrl, resolveAvatar } from "./lib/avatar.js";
import { hydrateSiteBrand } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import { byId } from "./lib/dom.js";
import { t } from "./lib/i18n.js";
import { logout } from "./api/session.js";

const markdownList = byId<HTMLElement>("markdownList");
const markdownLoadMoreBtn = byId<HTMLButtonElement>("markdownLoadMoreBtn");

type PublicMarkdownEntry = {
  id: number;
  user_id: string;
  username: string;
  user_icon?: string;
  title: string;
  summary?: string;
  cover_url?: string;
  uploaded_at: string;
};

type Scope = "public" | "workspace";

let nextOffset = 0;
let hasMore = true;
let scope: Scope = "public";

initStoredTheme();
bindThemeSync();

function formatTime(value: string): string {
  return new Date(value).toLocaleString();
}

function escapeHtml(value: string): string {
  return value
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function createMarkdownCard(entry: PublicMarkdownEntry): HTMLElement {
  const card = document.createElement("a");
  card.className = "markdown-card panel";
  card.href = `/markdown.html?id=${entry.id}`;

  const avatar = resolveAvatar(entry.username, entry.user_icon, 64);
  const cover = entry.cover_url
    ? `<img class="markdown-card-cover" src="${buildAssetUrl(entry.cover_url)}" alt="${escapeHtml(entry.title)}" loading="lazy" />`
    : "";
  const summary = entry.summary
    ? `<div class="markdown-card-summary">${escapeHtml(entry.summary)}</div>`
    : "";
  card.innerHTML = `
    <div class="post-header">
      <div class="post-author">
        <img class="avatar-sm" src="${avatar}" alt="${entry.username}" />
        <span class="post-author-name">${entry.username}</span>
      </div>
      <div class="post-time text-meta-mono">${formatTime(entry.uploaded_at)}</div>
    </div>
    ${cover}
    <div class="markdown-card-title">${entry.title}</div>
    ${summary}
    <div class="markdown-card-meta">${t("markdowns.clickToView")}</div>
  `;

  return card;
}

async function loadMarkdowns(reset = false): Promise<void> {
  if (reset) {
    nextOffset = 0;
    hasMore = true;
    markdownList.innerHTML = "";
  }
  if (!hasMore) {
    return;
  }

  // Scope decides which list endpoint we hit. Workspace tier needs
  // auth + the X-Workspace-Id header that http.ts auto-injects when
  // the user is logged in with an active workspace; public tier is
  // open to anonymous browsers.
  const url =
    scope === "workspace"
      ? `/api/markdown/workspace?limit=10&offset=${nextOffset}`
      : `/api/public/markdowns?limit=10&offset=${nextOffset}`;
  const response = await fetch(url, { credentials: "include" });
  const data = await response.json();
  if (!response.ok) {
    markdownList.innerHTML = `<div class='post-empty'>${t("markdowns.loadFailed")}</div>`;
    return;
  }

  const entries: PublicMarkdownEntry[] = data.entries || [];
  if (reset && entries.length === 0) {
    markdownList.innerHTML = `<div class='post-empty'>${t("markdowns.noPosts")}</div>`;
    hasMore = false;
    markdownLoadMoreBtn.style.display = "none";
    return;
  }

  entries.forEach((entry) => {
    markdownList.appendChild(createMarkdownCard(entry));
  });

  hasMore = Boolean(data.has_more);
  nextOffset = Number(data.next_offset || 0);
  markdownLoadMoreBtn.style.display = hasMore ? "inline-flex" : "none";
}

markdownLoadMoreBtn.addEventListener("click", () => {
  void loadMarkdowns(false);
});

// Scope tabs: clicking switches the data source and resets the list.
// `is-active` styles match the existing .btn-ghost convention; only
// one tab is active at a time.
document.querySelectorAll<HTMLButtonElement>("[data-scope]").forEach((btn) => {
  btn.addEventListener("click", () => {
    const next = btn.dataset.scope as Scope;
    if (!next || next === scope) return;
    scope = next;
    document.querySelectorAll<HTMLButtonElement>("[data-scope]").forEach((b) => {
      b.classList.toggle("is-active", b.dataset.scope === scope);
    });
    void loadMarkdowns(true);
  });
});

void hydrateSiteBrand();
void loadMarkdowns(true);

// Logout
document.getElementById("logoutBtn")?.addEventListener("click", async () => {
  try { await logout(); } finally { window.location.replace("/login.html"); }
});

