import { makeDefaultAvatar } from "./avatar.js";
import { hydrateIcons } from "./icons.js";
import { applyI18n, t } from "./i18n.js";
import { installWorkspaceSwitcher } from "./workspace.js";

type SiteSettings = {
  name?: string;
  description?: string;
  icon_url?: string;
};

type SidebarUser = {
  username?: string;
  role?: string;
  icon_url?: string;
};

const fallbackSite: Required<SiteSettings> = {
  name: "Polar-",
  description: "AI-assisted product prototyping workspace",
  icon_url: "",
};

function resolveSite(site?: SiteSettings): Required<SiteSettings> {
  return {
    name: site?.name?.trim() || fallbackSite.name,
    description: site?.description?.trim() || fallbackSite.description,
    icon_url: site?.icon_url || "",
  };
}

export function renderSiteBrand(site?: SiteSettings): void {
  const safeSite = resolveSite(site);
  const iconSrc = safeSite.icon_url || makeDefaultAvatar(safeSite.name, 160);

  document.querySelectorAll<HTMLElement>("[data-site-brand]").forEach((container) => {
    const nameEl = container.querySelector<HTMLElement>("[data-site-name]");
    const descEl = container.querySelector<HTMLElement>("[data-site-description]");
    const iconEl = container.querySelector<HTMLImageElement>("[data-site-icon]");

    if (nameEl) {
      nameEl.textContent = safeSite.name;
    }
    if (descEl) {
      descEl.textContent = safeSite.description;
    }
    if (iconEl) {
      iconEl.src = iconSrc;
      iconEl.alt = `${safeSite.name} ${t("brand.icon")}`;
    }
  });

  // Pages that opt in via <title data-site-title>...</title> get the
  // configured site name written into document.title. Other pages
  // keep whatever static title their HTML already has (e.g.
  // <title data-i18n-title="login.title">Login</title>).
  const titleEl = document.querySelector<HTMLTitleElement>("title[data-site-title]");
  if (titleEl) {
    titleEl.textContent = safeSite.name;
  }
}

const SIDEBAR_COLLAPSED_KEY = "lp_sidebar_collapsed";

export function initSettingsButton(): void {
  // On the dashboard, dashboard.ts handles [data-open-settings-center] directly.
  // On every other page, redirect to dashboard with ?settings= so the modal opens there.
  const isDashboard = window.location.pathname.endsWith("/dashboard.html") || window.location.pathname === "/";
  if (isDashboard) {
    return;
  }
  document.querySelectorAll<HTMLElement>("[data-open-settings-center]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const target = btn.dataset.settingsTarget || "personalization";
      window.location.href = `/dashboard.html?settings=${encodeURIComponent(target)}`;
    });
  });
}

export function initSidebarToggle(): void {
  const topbar = document.querySelector<HTMLElement>(".lp-topbar");
  const app = document.querySelector<HTMLElement>(".lp-app");
  if (!topbar || !app) {
    return;
  }
  if (localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "1") {
    app.classList.add("sidebar-collapsed");
  }
  const btn = document.createElement("button");
  btn.className = "lp-sidebar-toggle";
  btn.title = "Toggle sidebar";
  btn.setAttribute("aria-label", "Toggle sidebar");
  btn.textContent = "☰";
  btn.addEventListener("click", () => {
    const collapsed = app.classList.toggle("sidebar-collapsed");
    localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? "1" : "0");
  });
  topbar.insertBefore(btn, topbar.firstChild);
}

export async function hydrateSiteBrand(): Promise<void> {
  applyI18n();
  // Inline-SVG icon hydration runs early so the nav doesn't show a
  // brief Unicode-fallback glyph when the page first paints. Re-run
  // is cheap; pages that mutate the DOM after their own bootstrap
  // (e.g. project list rendering) can call hydrateIcons() again on
  // the changed subtree.
  hydrateIcons();
  initSidebarToggle();
  initSettingsButton();
  await hydrateSidebarFoot();
  // Workspace switcher (T4): runs after the foot hydrate so the
  // sidebar is fully laid out. No-op on pages without .lp-nav (e.g.,
  // login / register / public invite preview).
  void installWorkspaceSwitcher();

  if (!document.querySelector("[data-site-brand]")) {
    return;
  }

  try {
    const response = await fetch("/api/site-settings", { credentials: "include" });
    if (!response.ok) {
      renderSiteBrand();
      return;
    }
    const data = await response.json();
    renderSiteBrand(data.site);
  } catch {
    renderSiteBrand();
  }
}

export function renderSidebarFoot(user?: SidebarUser): void {
  const nameEl = document.getElementById("lpFootName");
  const roleEl = document.getElementById("lpFootRole");
  const avatarEl = document.getElementById("lpFootAvatar");

  if (!nameEl && !roleEl && !avatarEl) {
    return;
  }

  const username = (user?.username || "").trim();
  if (nameEl) {
    nameEl.textContent = username || "—";
  }
  if (roleEl) {
    roleEl.textContent = user?.role === "admin" ? "Administrator" : "Member";
  }
  if (avatarEl) {
    const avatar = username ? username.slice(0, 1).toUpperCase() : "U";
    if (user?.icon_url) {
      avatarEl.style.backgroundImage = `url(${user.icon_url})`;
      avatarEl.style.backgroundSize = "cover";
      avatarEl.style.backgroundPosition = "center";
      avatarEl.textContent = "";
    } else {
      avatarEl.style.backgroundImage = "";
      avatarEl.style.backgroundSize = "";
      avatarEl.style.backgroundPosition = "";
      avatarEl.textContent = avatar;
    }
  }

  const isAdmin = user?.role === "admin";
  document.querySelectorAll<HTMLElement>("[data-admin-nav]").forEach((el) => {
    el.hidden = !isAdmin;
  });
}

export async function hydrateSidebarFoot(): Promise<void> {
  const hasFoot = document.getElementById("lpFootName") || document.getElementById("lpFootRole") || document.getElementById("lpFootAvatar");
  if (!hasFoot) {
    return;
  }

  try {
    const response = await fetch("/api/me", { credentials: "include" });
    if (!response.ok) {
      return;
    }
    const data = await response.json();
    renderSidebarFoot(data);
  } catch {
    // Keep static placeholders on network failure.
  }
}
