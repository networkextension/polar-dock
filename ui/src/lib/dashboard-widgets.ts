// Dashboard widget rendering. Each widget = small card with a
// title + body fetched from an existing list endpoint (limit=5).
// Widgets are rendered in the user's saved order; the customize
// modal toggles which ones show + reorders.
//
// Adding a widget: add a slug to the server's dashboardWidgetCatalog
// + register a renderer here. No new API surface needed unless the
// list data shape demands it.

import { requestJson } from "../api/http.js";

export type WidgetMeta = {
  slug: string;
  name: string;
  description: string;
  admin_only?: boolean;
  default?: boolean;
};

export type DashboardLayoutResponse = {
  layout: string[];
  catalog: WidgetMeta[];
};

function escapeHTML(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function widgetCard(title: string, body: string, opts: { headerExtra?: string } = {}): string {
  return `
    <div class="dashboard-widget" style="background:#fff; border:1px solid var(--border,#ececf0); border-radius:8px; padding:12px;">
      <div style="display:flex; align-items:center; gap:8px; margin-bottom:8px;">
        <strong style="font-size:13px;">${escapeHTML(title)}</strong>
        <span style="flex:1;"></span>
        ${opts.headerExtra ?? ""}
      </div>
      <div>${body}</div>
    </div>
  `;
}

function emptyState(msg: string): string {
  return `<div style="color:var(--text-muted,#888); font-size:12px; padding:8px 0;">${escapeHTML(msg)}</div>`;
}

// ---- individual widget renderers ----

export function renderQuickStart(): string {
  // 9 tiles linking to main modules. Static — doesn't need data fetch.
  const tiles: Array<[string, string, string, string]> = [
    ["✦", "项目", "/projects.html", "AI 拆解 + 派单"],
    ["◎", "Posts", "/posts.html", "动态 / 讨论"],
    ["◈", "Markdowns", "/markdowns.html", "长文 / 笔记"],
    ["◉", "Chat", "/chat.html", "Bot 对话"],
    ["✍", "Creation", "/creation.html", "图文创作"],
    ["▶", "Video Studio", "/video-studio.html", "视频生成"],
    ["⬇", "iOS 分发", "/iosdist.html", "TF / OTA"],
    ["⌂", "工作区", "/teams.html", "团队 / 邀请"],
    ["✎", "Editor", "/editor.html", "代码编辑器"],
  ];
  const body = `
    <div style="display:grid; grid-template-columns:repeat(auto-fill, minmax(180px, 1fr)); gap:8px;">
      ${tiles
        .map(
          ([icon, label, href, desc]) => `
            <a href="${escapeHTML(href)}" style="display:flex; gap:10px; align-items:center; padding:10px; border:1px solid var(--border,#ececf0); border-radius:6px; text-decoration:none; color:inherit; background:#fafafb;">
              <span style="font-size:18px; line-height:1; opacity:0.75;">${icon}</span>
              <div style="display:flex; flex-direction:column; gap:2px; min-width:0;">
                <span style="font-weight:600; font-size:13px;">${escapeHTML(label)}</span>
                <span style="font-size:11px; color:var(--text-muted,#888);">${escapeHTML(desc)}</span>
              </div>
            </a>
          `,
        )
        .join("")}
    </div>
  `;
  return widgetCard("快速入口", body);
}

type ProjectListItem = {
  id: string;
  name: string;
  status: string;
  current_sprint: number;
  updated_at: string;
};

export async function renderRecentProjects(): Promise<string> {
  const { response, data } = await requestJson<{ projects: ProjectListItem[] }>(
    "/api/projects",
  );
  if (!response.ok) {
    return widgetCard("最近项目", emptyState("加载失败 — 可能没有 workspace 权限"));
  }
  const items = (data.projects ?? []).slice(0, 5);
  if (items.length === 0) {
    return widgetCard(
      "最近项目",
      emptyState("还没有项目。") +
        `<a href="/projects.html" style="font-size:12px;">去创建第一个 →</a>`,
    );
  }
  const body = `
    <ul style="list-style:none; padding:0; margin:0; display:flex; flex-direction:column; gap:6px;">
      ${items
        .map(
          (p) => `
            <li>
              <a href="/projects.html?project=${encodeURIComponent(p.id)}" style="display:flex; gap:8px; align-items:center; padding:6px 8px; border-radius:4px; text-decoration:none; color:inherit; background:#fafafb;">
                <span style="flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-size:13px;">${escapeHTML(p.name)}</span>
                <span style="font-size:11px; color:var(--text-muted,#888);">Sprint ${p.current_sprint || 0}</span>
                <span class="tag-chip" style="font-size:11px;">${escapeHTML(p.status)}</span>
              </a>
            </li>`,
        )
        .join("")}
    </ul>
  `;
  return widgetCard("最近项目", body, {
    headerExtra: `<a href="/projects.html" style="font-size:11px;">查看全部 →</a>`,
  });
}

type MarkdownEntry = { id: number; title: string; updated_at: string };

export async function renderRecentMarkdowns(): Promise<string> {
  const { response, data } = await requestJson<{ entries: MarkdownEntry[] }>(
    "/api/markdown?limit=5",
  );
  if (!response.ok) {
    return widgetCard("最近 Markdown", emptyState("加载失败"));
  }
  const items = (data.entries ?? []).slice(0, 5);
  if (items.length === 0) {
    return widgetCard(
      "最近 Markdown",
      emptyState("还没有 markdown entry。") +
        `<a href="/markdowns.html" style="font-size:12px;">去创建第一个 →</a>`,
    );
  }
  const body = `
    <ul style="list-style:none; padding:0; margin:0; display:flex; flex-direction:column; gap:4px;">
      ${items
        .map(
          (e) => `
            <li>
              <a href="/markdowns.html#entry-${encodeURIComponent(String(e.id))}" style="display:flex; gap:8px; align-items:center; padding:5px 8px; border-radius:4px; text-decoration:none; color:inherit; background:#fafafb;">
                <span style="flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-size:13px;">${escapeHTML(e.title || "(无标题)")}</span>
                <span style="font-size:11px; color:var(--text-muted,#888);">${new Date(e.updated_at).toLocaleDateString()}</span>
              </a>
            </li>`,
        )
        .join("")}
    </ul>
  `;
  return widgetCard("最近 Markdown", body, {
    headerExtra: `<a href="/markdowns.html" style="font-size:11px;">查看全部 →</a>`,
  });
}

type PostItem = { id: number; content: string; created_at: string };

export async function renderRecentPosts(): Promise<string> {
  const { response, data } = await requestJson<{ posts: PostItem[] }>("/api/posts?limit=5");
  if (!response.ok) {
    return widgetCard("最近 Post", emptyState("加载失败"));
  }
  const items = (data.posts ?? []).slice(0, 5);
  if (items.length === 0) {
    return widgetCard("最近 Post", emptyState("还没有 post。"));
  }
  const body = `
    <ul style="list-style:none; padding:0; margin:0; display:flex; flex-direction:column; gap:4px;">
      ${items
        .map(
          (p) => `
            <li>
              <a href="/post.html?id=${encodeURIComponent(String(p.id))}" style="display:block; padding:5px 8px; border-radius:4px; text-decoration:none; color:inherit; background:#fafafb;">
                <span style="font-size:12px; display:-webkit-box; -webkit-line-clamp:2; -webkit-box-orient:vertical; overflow:hidden;">${escapeHTML((p.content || "").slice(0, 200))}</span>
                <span style="font-size:11px; color:var(--text-muted,#888);">${new Date(p.created_at).toLocaleString()}</span>
              </a>
            </li>`,
        )
        .join("")}
    </ul>
  `;
  return widgetCard("最近 Post", body, {
    headerExtra: `<a href="/posts.html" style="font-size:11px;">查看全部 →</a>`,
  });
}

type ChatThread = {
  id: number;
  participant_username?: string;
  participant_name?: string;
  unread?: number;
  last_message_at?: string;
};

export async function renderRecentChat(): Promise<string> {
  const { response, data } = await requestJson<{ threads: ChatThread[] }>("/api/chats");
  if (!response.ok) {
    return widgetCard("最近 Chat", emptyState("加载失败"));
  }
  const items = (data.threads ?? []).slice(0, 5);
  if (items.length === 0) {
    return widgetCard("最近 Chat", emptyState("还没有对话。"));
  }
  const body = `
    <ul style="list-style:none; padding:0; margin:0; display:flex; flex-direction:column; gap:4px;">
      ${items
        .map((t) => {
          const name = t.participant_name || t.participant_username || `Thread ${t.id}`;
          const unread = (t.unread ?? 0) > 0 ? `<span style="background:#c33; color:#fff; border-radius:10px; padding:1px 6px; font-size:10px;">${t.unread}</span>` : "";
          return `
            <li>
              <a href="/chat.html?thread=${encodeURIComponent(String(t.id))}" style="display:flex; gap:8px; align-items:center; padding:5px 8px; border-radius:4px; text-decoration:none; color:inherit; background:#fafafb;">
                <span style="flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-size:13px;">${escapeHTML(name)}</span>
                ${unread}
              </a>
            </li>`;
        })
        .join("")}
    </ul>
  `;
  return widgetCard("最近 Chat", body, {
    headerExtra: `<a href="/chat.html" style="font-size:11px;">全部 →</a>`,
  });
}

type IOSApp = {
  id: number;
  name: string;
  bundle_id: string;
  latest_build_state?: string;
  updated_at: string;
};

export async function renderIOSDistBuilds(): Promise<string> {
  const { response, data } = await requestJson<{ apps: IOSApp[] }>("/api/iosdist/apps");
  if (!response.ok) {
    return widgetCard("iOS 分发构建", emptyState("加载失败 — 需要 admin 权限"));
  }
  const items = (data.apps ?? []).slice(0, 5);
  if (items.length === 0) {
    return widgetCard("iOS 分发构建", emptyState("没有 iosdist app。"));
  }
  const body = `
    <ul style="list-style:none; padding:0; margin:0; display:flex; flex-direction:column; gap:4px;">
      ${items
        .map(
          (a) => `
            <li>
              <a href="/iosdist.html#app-${encodeURIComponent(String(a.id))}" style="display:flex; gap:8px; align-items:center; padding:5px 8px; border-radius:4px; text-decoration:none; color:inherit; background:#fafafb;">
                <span style="flex:1; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; font-size:13px;">${escapeHTML(a.name)}</span>
                <span style="font-size:11px; color:var(--text-muted,#888);">${escapeHTML(a.bundle_id)}</span>
              </a>
            </li>`,
        )
        .join("")}
    </ul>
  `;
  return widgetCard("iOS 分发构建", body, {
    headerExtra: `<a href="/iosdist.html" style="font-size:11px;">全部 →</a>`,
  });
}

// Render the entire dashboard's enabled widgets in order. Renders
// each card synchronously when possible (quick_start) and awaits
// async fetches in parallel for a fast first paint.
export async function renderDashboardWidgets(layout: string[], container: HTMLElement): Promise<void> {
  const tasks = layout.map(async (slug): Promise<{ slug: string; html: string }> => {
    switch (slug) {
      case "quick_start":
        return { slug, html: renderQuickStart() };
      case "recent_projects":
        return { slug, html: await renderRecentProjects() };
      case "recent_markdowns":
        return { slug, html: await renderRecentMarkdowns() };
      case "recent_posts":
        return { slug, html: await renderRecentPosts() };
      case "recent_chat":
        return { slug, html: await renderRecentChat() };
      case "iosdist_builds":
        return { slug, html: await renderIOSDistBuilds() };
      default:
        return { slug, html: "" };
    }
  });
  const results = await Promise.all(tasks);
  container.innerHTML = results.map((r) => r.html).join("");
}

// ---- layout API ----

export async function fetchDashboardLayout() {
  return requestJson<DashboardLayoutResponse>("/api/dashboard/layout");
}

export async function saveDashboardLayout(layout: string[]) {
  return requestJson<{ layout: string[] }>("/api/dashboard/layout", {
    method: "PUT",
    body: { layout },
  });
}
