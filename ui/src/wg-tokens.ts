// /wg-tokens.html — admin UI for wg-mac join control plane.
//
// Three tabs:
//   - 🔑 Tokens   — list, create (TTL + label), revoke; plaintext modal once
//   - 💻 Devices  — list (with last-seen + endpoint), force-remove
//   - 📦 Bundles  — list, upload (file + version + set_latest), mark-latest
//
// Plus a Hub config card above the tabs (configure pubkey/endpoint).

import {
  createWGToken,
  deleteWGBundle,
  fetchWGHub,
  listWGBundles,
  listWGDevices,
  listWGTokens,
  removeWGDevice,
  revokeWGToken,
  setWGBundleLatest,
  updateWGHub,
  uploadWGBundle,
} from "./api/wg.js";
import { logout } from "./api/session.js";
import { byId } from "./lib/dom.js";
import { hydrateSiteBrand, hydrateSidebarFoot } from "./lib/site.js";
import { bindThemeSync, initStoredTheme } from "./lib/theme.js";
import type { WGBundle, WGDevice, WGHub, WGToken } from "./types/wg.js";

initStoredTheme();
bindThemeSync();

const logoutBtn = byId<HTMLButtonElement>("logoutBtn");
logoutBtn?.addEventListener("click", async () => {
  await logout();
  window.location.href = "/login.html";
});

void hydrateSiteBrand();
void hydrateSidebarFoot();

// ---- tabs ----

const tabs = ["tokens", "devices", "bundles"] as const;
type TabKey = (typeof tabs)[number];

function switchTab(target: TabKey): void {
  tabs.forEach((t) => {
    const btn = byId<HTMLButtonElement>(`tab${t.charAt(0).toUpperCase() + t.slice(1)}`);
    const pane = byId<HTMLElement>(`pane${t.charAt(0).toUpperCase() + t.slice(1)}`);
    if (btn) btn.classList.toggle("lp-tab-active", t === target);
    if (pane) pane.hidden = t !== target;
  });
}
tabs.forEach((t) => {
  const btn = byId<HTMLButtonElement>(`tab${t.charAt(0).toUpperCase() + t.slice(1)}`);
  btn?.addEventListener("click", () => switchTab(t));
});

// ---- shared formatters ----

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

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

// ---- Hub card ----

const hubInfo = byId<HTMLElement>("hubInfo");
const hubStatusBadge = byId<HTMLElement>("hubStatusBadge");
const hubNotConfiguredWarning = byId<HTMLElement>("hubNotConfiguredWarning");
const hubRefreshBtn = byId<HTMLButtonElement>("hubRefreshBtn");
const hubConfigBtn = byId<HTMLButtonElement>("hubConfigBtn");
const hubModal = byId<HTMLElement>("hubModal");
const hubCloseBtn = byId<HTMLButtonElement>("hubCloseBtn");
const hubCancelBtn = byId<HTMLButtonElement>("hubCancelBtn");
const hubSaveBtn = byId<HTMLButtonElement>("hubSaveBtn");
const hubPubkey = byId<HTMLInputElement>("hubPubkey");
const hubEndpoint = byId<HTMLInputElement>("hubEndpoint");
const hubWGIP = byId<HTMLInputElement>("hubWGIP");
const hubMeshCIDR = byId<HTMLInputElement>("hubMeshCIDR");
const hubKeepalive = byId<HTMLInputElement>("hubKeepalive");
const hubRefresh = byId<HTMLInputElement>("hubRefresh");
const hubError = byId<HTMLElement>("hubError");

let currentHub: WGHub | null = null;

async function refreshHub(): Promise<void> {
  try {
    const { data } = await fetchWGHub();
    currentHub = data.hub ?? null;
    const configured = !!data.configured;
    hubStatusBadge.textContent = configured ? "✓ 已配置" : "⚠ 未配置";
    hubStatusBadge.style.color = configured ? "" : "var(--danger, #c00)";
    hubNotConfiguredWarning.hidden = configured;
    if (currentHub) {
      hubInfo.innerHTML = `
        <div><strong>Pubkey:</strong> <code>${currentHub.pubkey || "—"}</code></div>
        <div><strong>Endpoint:</strong> <code>${currentHub.endpoint || "—"}</code></div>
        <div><strong>Hub wg IP:</strong> <code>${currentHub.wg_ip}</code> · <strong>Mesh CIDR:</strong> <code>${currentHub.mesh_cidr}</code></div>
        <div><strong>Keepalive:</strong> ${currentHub.keepalive_sec}s · <strong>Refresh:</strong> ${currentHub.refresh_sec}s</div>
      `;
    }
  } catch (err) {
    hubInfo.textContent = `加载失败：${(err as Error).message}`;
  }
}

function openHubModal(): void {
  if (currentHub) {
    hubPubkey.value = currentHub.pubkey;
    hubEndpoint.value = currentHub.endpoint;
    hubWGIP.value = currentHub.wg_ip;
    hubMeshCIDR.value = currentHub.mesh_cidr;
    hubKeepalive.value = String(currentHub.keepalive_sec);
    hubRefresh.value = String(currentHub.refresh_sec);
  }
  hubError.textContent = "";
  hubModal.hidden = false;
}
function closeHubModal(): void {
  hubModal.hidden = true;
}

hubRefreshBtn?.addEventListener("click", () => void refreshHub());
hubConfigBtn?.addEventListener("click", openHubModal);
hubCloseBtn?.addEventListener("click", closeHubModal);
hubCancelBtn?.addEventListener("click", closeHubModal);

hubSaveBtn?.addEventListener("click", async () => {
  hubError.textContent = "";
  try {
    await updateWGHub({
      pubkey: hubPubkey.value.trim(),
      endpoint: hubEndpoint.value.trim(),
      wg_ip: hubWGIP.value.trim() || "10.88.0.1",
      mesh_cidr: hubMeshCIDR.value.trim() || "10.88.0.0/16",
      keepalive_sec: Number(hubKeepalive.value) || 25,
      refresh_sec: Number(hubRefresh.value) || 300,
    });
    closeHubModal();
    await refreshHub();
  } catch (err) {
    hubError.textContent = `保存失败：${(err as Error).message}`;
  }
});

// ---- Tokens tab ----

const tokensTable = byId<HTMLTableElement>("tokensTable");
const tokensTbody = byId<HTMLTableSectionElement>("tokensTbody");
const tokensEmpty = byId<HTMLElement>("tokensEmpty");
const tokensSummary = byId<HTMLElement>("tokensSummary");
const tokensRefreshBtn = byId<HTMLButtonElement>("tokensRefreshBtn");
const newTokenBtn = byId<HTMLButtonElement>("newTokenBtn");

const createTokenModal = byId<HTMLElement>("createTokenModal");
const createTokenCloseBtn = byId<HTMLButtonElement>("createTokenCloseBtn");
const createTokenCancelBtn = byId<HTMLButtonElement>("createTokenCancelBtn");
const createTokenSubmitBtn = byId<HTMLButtonElement>("createTokenSubmitBtn");
const createTokenLabel = byId<HTMLInputElement>("createTokenLabel");
const createTokenTTL = byId<HTMLInputElement>("createTokenTTL");
const createTokenError = byId<HTMLElement>("createTokenError");

const plaintextModal = byId<HTMLElement>("plaintextModal");
const plaintextValue = byId<HTMLInputElement>("plaintextValue");
const plaintextCopyBtn = byId<HTMLButtonElement>("plaintextCopyBtn");
const plaintextInstallCmd = byId<HTMLTextAreaElement>("plaintextInstallCmd");
const plaintextInstallCopyBtn = byId<HTMLButtonElement>("plaintextInstallCopyBtn");
const plaintextDoneBtn = byId<HTMLButtonElement>("plaintextDoneBtn");

function tokenStatusText(tok: WGToken): string {
  if (tok.revoked_at) return `🔴 已撤销 ${fmtRelative(tok.revoked_at)}`;
  if (tok.consumed_at) return `✓ 已使用 ${fmtRelative(tok.consumed_at)}`;
  if (tok.expires_at && new Date(tok.expires_at).getTime() < Date.now()) {
    return `⏰ 已过期 ${fmtRelative(tok.expires_at)}`;
  }
  return "🟢 待使用";
}

function renderTokenRow(tok: WGToken): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (tok.revoked_at) tr.style.opacity = "0.5";

  const cells = [
    tok.label,
    `<code style="font-size:11px;">${tok.token_prefix}…</code>`,
    tok.expires_at ? new Date(tok.expires_at).toISOString().slice(0, 10) : "永久",
    tokenStatusText(tok),
    tok.consumed_by_device_id ? `#${tok.consumed_by_device_id}` : "—",
    fmtRelative(tok.created_at),
  ];
  cells.forEach((html) => {
    const td = document.createElement("td");
    td.innerHTML = html;
    tr.appendChild(td);
  });
  const cellActions = document.createElement("td");
  if (!tok.revoked_at) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn-inline btn-secondary";
    btn.textContent = "撤销";
    btn.addEventListener("click", async () => {
      if (!window.confirm(`撤销 token "${tok.label}"？已绑定的设备不受影响。`)) return;
      try {
        await revokeWGToken(tok.id);
        await refreshTokens();
      } catch (err) {
        window.alert(`撤销失败：${(err as Error).message}`);
      }
    });
    cellActions.appendChild(btn);
  }
  tr.appendChild(cellActions);
  return tr;
}

async function refreshTokens(): Promise<void> {
  try {
    const { data } = await listWGTokens();
    const items = data.tokens ?? [];
    tokensTbody.innerHTML = "";
    items.forEach((tok) => tokensTbody.appendChild(renderTokenRow(tok)));
    tokensSummary.textContent = `${items.length} 个`;
    tokensTable.hidden = items.length === 0;
    tokensEmpty.hidden = items.length !== 0;
  } catch (err) {
    tokensEmpty.hidden = false;
    tokensEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

function openCreateTokenModal(): void {
  createTokenLabel.value = "";
  createTokenTTL.value = "90";
  createTokenError.textContent = "";
  createTokenModal.hidden = false;
  createTokenLabel.focus();
}
function closeCreateTokenModal(): void {
  createTokenModal.hidden = true;
}

newTokenBtn?.addEventListener("click", openCreateTokenModal);
createTokenCloseBtn?.addEventListener("click", closeCreateTokenModal);
createTokenCancelBtn?.addEventListener("click", closeCreateTokenModal);
tokensRefreshBtn?.addEventListener("click", () => void refreshTokens());

createTokenSubmitBtn?.addEventListener("click", async () => {
  createTokenError.textContent = "";
  const label = createTokenLabel.value.trim();
  if (!label) {
    createTokenError.textContent = "标签必填";
    return;
  }
  const ttl = Number(createTokenTTL.value) || 0;
  try {
    const { data } = await createWGToken(label, ttl);
    if (data.error || !data.token?.plaintext) {
      createTokenError.textContent = data.error || "服务器没返回 plaintext";
      return;
    }
    closeCreateTokenModal();
    const plaintext = data.token.plaintext;
    plaintextValue.value = plaintext;
    const origin = window.location.origin;
    plaintextInstallCmd.value = `curl -sSL ${origin}/v1/install | sudo bash -s -- --token=${plaintext}`;
    plaintextModal.hidden = false;
    await refreshTokens();
  } catch (err) {
    createTokenError.textContent = `创建失败：${(err as Error).message}`;
  }
});

plaintextCopyBtn?.addEventListener("click", async () => {
  await navigator.clipboard.writeText(plaintextValue.value);
  plaintextCopyBtn.textContent = "✓";
  setTimeout(() => (plaintextCopyBtn.textContent = "📋"), 1500);
});
plaintextInstallCopyBtn?.addEventListener("click", async () => {
  await navigator.clipboard.writeText(plaintextInstallCmd.value);
  plaintextInstallCopyBtn.textContent = "✓ 已复制";
  setTimeout(() => (plaintextInstallCopyBtn.textContent = "📋 复制命令"), 1500);
});
plaintextDoneBtn?.addEventListener("click", () => {
  plaintextModal.hidden = true;
});

// ---- Devices tab ----

const devicesTable = byId<HTMLTableElement>("devicesTable");
const devicesTbody = byId<HTMLTableSectionElement>("devicesTbody");
const devicesEmpty = byId<HTMLElement>("devicesEmpty");
const devicesSummary = byId<HTMLElement>("devicesSummary");
const devicesRefreshBtn = byId<HTMLButtonElement>("devicesRefreshBtn");
const includeRemovedCheck = byId<HTMLInputElement>("includeRemovedCheck");

function deviceStatus(dev: WGDevice): string {
  if (dev.removed_at) return `🔴 已下线 ${fmtRelative(dev.removed_at)}`;
  if (!dev.last_seen_at) return "⚪ 从未上报";
  const ageSec = Math.floor((Date.now() - new Date(dev.last_seen_at).getTime()) / 1000);
  if (ageSec < 600) return "🟢 在线";
  if (ageSec < 86400) return "🟡 待确认";
  return "🔴 离线";
}

function renderDeviceRow(dev: WGDevice): HTMLTableRowElement {
  const tr = document.createElement("tr");
  if (dev.removed_at) tr.style.opacity = "0.5";
  const cells: string[] = [
    `<code style="font-size:11px;">${dev.device_id}</code>`,
    dev.hostname || "—",
    dev.site_slug || `#${dev.site_id}`,
    `<code>${dev.device_ip}</code>`,
    dev.wg_endpoint ? `<code>${dev.wg_endpoint}</code>` : "—",
    `${dev.os}/${dev.arch} <span class="meta-subtitle">${dev.agent_ver || ""}</span>`,
    fmtRelative(dev.last_seen_at),
    deviceStatus(dev),
  ];
  cells.forEach((html) => {
    const td = document.createElement("td");
    td.innerHTML = html;
    tr.appendChild(td);
  });
  const cellActions = document.createElement("td");
  if (!dev.removed_at) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "btn-inline btn-secondary";
    btn.textContent = "强制下线";
    btn.addEventListener("click", async () => {
      if (!window.confirm(`强制下线设备 "${dev.hostname || dev.device_id}"？`)) return;
      try {
        await removeWGDevice(dev.id);
        await refreshDevices();
      } catch (err) {
        window.alert(`下线失败：${(err as Error).message}`);
      }
    });
    cellActions.appendChild(btn);
  }
  tr.appendChild(cellActions);
  return tr;
}

async function refreshDevices(): Promise<void> {
  try {
    const { data } = await listWGDevices(includeRemovedCheck.checked);
    const items = data.devices ?? [];
    devicesTbody.innerHTML = "";
    items.forEach((d) => devicesTbody.appendChild(renderDeviceRow(d)));
    devicesSummary.textContent = `${items.length} 台`;
    devicesTable.hidden = items.length === 0;
    devicesEmpty.hidden = items.length !== 0;
  } catch (err) {
    devicesEmpty.hidden = false;
    devicesEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

devicesRefreshBtn?.addEventListener("click", () => void refreshDevices());
includeRemovedCheck?.addEventListener("change", () => void refreshDevices());

// ---- Bundles tab ----

const bundlesTable = byId<HTMLTableElement>("bundlesTable");
const bundlesTbody = byId<HTMLTableSectionElement>("bundlesTbody");
const bundlesEmpty = byId<HTMLElement>("bundlesEmpty");
const bundlesSummary = byId<HTMLElement>("bundlesSummary");
const bundlesRefreshBtn = byId<HTMLButtonElement>("bundlesRefreshBtn");
const uploadBundleBtn = byId<HTMLButtonElement>("uploadBundleBtn");

const uploadBundleModal = byId<HTMLElement>("uploadBundleModal");
const uploadBundleCloseBtn = byId<HTMLButtonElement>("uploadBundleCloseBtn");
const uploadBundleCancelBtn = byId<HTMLButtonElement>("uploadBundleCancelBtn");
const uploadBundleSubmitBtn = byId<HTMLButtonElement>("uploadBundleSubmitBtn");
const uploadBundleFile = byId<HTMLInputElement>("uploadBundleFile");
const uploadBundleVersion = byId<HTMLInputElement>("uploadBundleVersion");
const uploadBundleNotes = byId<HTMLInputElement>("uploadBundleNotes");
const uploadBundleSetLatest = byId<HTMLInputElement>("uploadBundleSetLatest");
const uploadBundleError = byId<HTMLElement>("uploadBundleError");

function renderBundleRow(b: WGBundle): HTMLTableRowElement {
  const tr = document.createElement("tr");
  const cells: string[] = [
    `<code>${b.version}</code>`,
    `<code style="font-size:11px;">${b.blob_sha256.slice(0, 12)}…</code>`,
    fmtBytes(b.size_bytes),
    b.is_latest ? "✓ latest" : "",
    b.notes ? `<span class="meta-subtitle">${b.notes}</span>` : "",
    fmtRelative(b.added_at),
  ];
  cells.forEach((html) => {
    const td = document.createElement("td");
    td.innerHTML = html;
    tr.appendChild(td);
  });
  const cellActions = document.createElement("td");
  cellActions.style.whiteSpace = "nowrap";
  const dl = document.createElement("a");
  dl.href = `/v1/bundle/${encodeURIComponent(b.version)}`;
  dl.className = "btn-inline btn-secondary";
  dl.textContent = "⬇";
  dl.style.marginRight = "4px";
  cellActions.appendChild(dl);
  if (!b.is_latest) {
    const setBtn = document.createElement("button");
    setBtn.type = "button";
    setBtn.className = "btn-inline btn-secondary";
    setBtn.textContent = "设为 latest";
    setBtn.style.marginRight = "4px";
    setBtn.addEventListener("click", async () => {
      try {
        await setWGBundleLatest(b.id);
        await refreshBundles();
      } catch (err) {
        window.alert(`标记失败：${(err as Error).message}`);
      }
    });
    cellActions.appendChild(setBtn);
  }
  const delBtn = document.createElement("button");
  delBtn.type = "button";
  delBtn.className = "btn-inline btn-secondary";
  delBtn.textContent = "🗑";
  delBtn.addEventListener("click", async () => {
    if (!window.confirm(`删除 bundle "${b.version}"？已在用的设备不受影响。`)) return;
    try {
      await deleteWGBundle(b.id);
      await refreshBundles();
    } catch (err) {
      window.alert(`删除失败：${(err as Error).message}`);
    }
  });
  cellActions.appendChild(delBtn);
  tr.appendChild(cellActions);
  return tr;
}

async function refreshBundles(): Promise<void> {
  try {
    const { data } = await listWGBundles();
    const items = data.bundles ?? [];
    bundlesTbody.innerHTML = "";
    items.forEach((b) => bundlesTbody.appendChild(renderBundleRow(b)));
    const latest = items.find((b) => b.is_latest);
    bundlesSummary.textContent = `${items.length} 个 · latest=${latest?.version ?? "—"}`;
    bundlesTable.hidden = items.length === 0;
    bundlesEmpty.hidden = items.length !== 0;
  } catch (err) {
    bundlesEmpty.hidden = false;
    bundlesEmpty.textContent = `加载失败：${(err as Error).message}`;
  }
}

function openUploadBundleModal(): void {
  uploadBundleFile.value = "";
  uploadBundleVersion.value = "";
  uploadBundleNotes.value = "";
  uploadBundleSetLatest.checked = true;
  uploadBundleError.textContent = "";
  uploadBundleModal.hidden = false;
}
function closeUploadBundleModal(): void {
  uploadBundleModal.hidden = true;
}

uploadBundleBtn?.addEventListener("click", openUploadBundleModal);
uploadBundleCloseBtn?.addEventListener("click", closeUploadBundleModal);
uploadBundleCancelBtn?.addEventListener("click", closeUploadBundleModal);
bundlesRefreshBtn?.addEventListener("click", () => void refreshBundles());

uploadBundleSubmitBtn?.addEventListener("click", async () => {
  uploadBundleError.textContent = "";
  const file = uploadBundleFile.files?.[0];
  if (!file) {
    uploadBundleError.textContent = "请选择 .tar.gz 文件";
    return;
  }
  const form = new FormData();
  form.append("file", file);
  const version = uploadBundleVersion.value.trim();
  if (version) form.append("version", version);
  const notes = uploadBundleNotes.value.trim();
  if (notes) form.append("notes", notes);
  if (uploadBundleSetLatest.checked) form.append("set_latest", "1");
  try {
    uploadBundleSubmitBtn.disabled = true;
    uploadBundleSubmitBtn.textContent = "上传中...";
    const { ok, data } = await uploadWGBundle(form);
    if (!ok || data.error) {
      uploadBundleError.textContent = data.error || "上传失败";
      return;
    }
    closeUploadBundleModal();
    await refreshBundles();
  } catch (err) {
    uploadBundleError.textContent = `上传失败：${(err as Error).message}`;
  } finally {
    uploadBundleSubmitBtn.disabled = false;
    uploadBundleSubmitBtn.textContent = "上传";
  }
});

// ---- initial load + polling ----

void refreshHub();
void refreshTokens();
void refreshDevices();
void refreshBundles();

// Refresh the devices tab every 15s so heartbeat freshness updates
// without a manual click. Tokens + bundles change rarely.
window.setInterval(() => {
  if (!byId<HTMLElement>("paneDevices").hidden) {
    void refreshDevices();
  }
}, 15_000);
