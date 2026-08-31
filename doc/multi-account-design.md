# 多账号 / 多 endpoint 设计方案（iOS，免 logout 切换）

## 目标与约束

- 一个 app 同时登录**多个账号**;账号可分布在**不同 API endpoint**(zen.4950.store / opera.daotian.ai / 自建 dock …)。
- **无需 logout** 即可在账号间切换(Slack-workspace / Gmail-账号 体验):切换是即时的、不清任何凭据。
- 同一 endpoint 下也能并存**多个不同用户**(不只是多 endpoint)。
- 现状:app(`polarstart`/`polarmac`)用 **PolarKit(polar-ios-sdk)**,它是 `APIClient.shared` 单例 + 全局 `HTTPCookieStorage.shared` + 全局 baseURL/workspace,**结构上挡死多账号**。

## 命门:认证模型从「共享 cookie」改为「每账号 Bearer token」

这是整个方案的地基,不改这一条其余都白搭。

- PolarKit 现在靠 `URLSession` 自动捕获 `Set-Cookie` 进 `HTTPCookieStorage.shared`。该 store 是**进程全局、且每 domain 一份 cookie**。→ 同一 endpoint 登录第二个账号会**覆盖**第一个的 `access_token` cookie。多 endpoint 勉强能共存(不同 domain),但同 endpoint 多账号必崩。
- dock 的鉴权 `extractAccessToken` **先认 `Authorization: Bearer`、再认 cookie**。所以正解:
  1. 登录时从响应的 `Set-Cookie` 里**取出 `access_token` / `refresh_token` 的值**(不依赖系统 cookie store);
  2. 按账号存进 **Keychain**;
  3. 每请求带 `Authorization: Bearer <该账号的 access_token>` + `X-Workspace-Id: <该账号的 workspace>`;
  4. URLSession 关掉 cookie(`httpShouldSetCookies=false`、`httpCookieAcceptPolicy=.never`),彻底脱离全局 cookie store。
- 这样每个账号是**完全自包含的凭据**,与 endpoint、与其他账号都正交。

> polar-sdk-swift 的 `URLSessionHTTPTransport` 已经是 `httpShouldSetCookies=false`,且每 `PolarClient` 自带隔离 `CookieJar` + 可选 `KeychainCredentialStore` —— 它本就按这个模型设计。

## 数据模型

```
Account                       // 非密元数据,存 plist/UserDefaults(可明文)
  id: UUID                    // 本地账号主键(账号系统的稳定 key)
  endpoint: URL               // baseURL
  userID / username / email / avatarURL   // 来自 /api/me,用于切换器展示
  activeWorkspaceID: String?  // 该账号当前选中的 workspace(X-Workspace-Id)
  addedAt / lastUsedAt: Date

凭据(密)→ Keychain,key = account.id:
  accessToken / refreshToken / accessExpiresAt

AccountStore
  accounts: [Account]
  activeAccountID: UUID?      // 当前活跃账号;切换 = 改这个指针
```

- 唯一性:`(endpoint, userID)` 去重(同 endpoint 同用户不重复加);但允许同 endpoint 不同用户、同用户不同 endpoint。
- Keychain 项用 `kSecAttrAccessibleAfterFirstUnlock`;敏感部署可叠加 biometric access-control。元数据非密,放普通存储即可。

## 会话与客户端:从单例到「每账号一个 Session」

```
PolarSession                  // 一个账号的活跃运行时
  account: Account
  client: <每实例 baseURL + Bearer token 的 API client>
  services: { Auth, Profile, Chat, Music, ... }  // 都绑定到本 client
  ws: ChatWebSocket?          // 该账号的实时连接

SessionManager (app 单例,唯一的全局)
  sessions: [UUID: PolarSession]   // 已激活的账号会话(懒加载)
  active: PolarSession?            // 当前活跃
  func switchTo(accountID)         // 改 active 指针 + 通知 UI;不动任何凭据
  func addAccount(endpoint, login) // 走登录 → 建 Account + Session
  func removeAccount(id)           // 删该账号 Keychain + 元数据 + 断 ws
```

切换账号 = `SessionManager.active = sessions[id]`,发一个 `activeAccountChanged` 通知,UI 重新绑定到新 session 的 services。**没有 logout 概念**;只有「移除账号」(显式删凭据)。

## SDK 选型(两条路,给出推荐)

**推荐:以 polar-sdk-swift(PolarAPI)为多账号地基。** 它已经具备:
- `PolarClient(config: PolarConfig(baseURL, workspaceID), transport: URLSessionHTTPTransport(), cookies: CookieJar())` —— N 个并存、互相隔离;
- `CredentialStore` 协议 + `KeychainCredentialStore`(token 持久化);
- `CookieJar.snapshot()/restore()`(如需保留 cookie 兜底);
- 注入式 `HTTPTransport`(便于加 per-account 日志 / header / 重试)。

做法:app 引入 PolarAPI,新建 `SessionManager` + `PolarSession` 包一层,**新功能(切换器、加账号、refresh)直接用 PolarAPI**;旧屏逐屏迁移(PolarKit completion-handler → PolarAPI async),或先用一个薄 bridge 让旧屏读 active session 的 client。

**备选(低改动、留技术债):de-singleton PolarKit。** 把 `APIClient.shared` 改成 `APIClient(config:)` 实例、加 Bearer、services 改成 init 注入 client。好处是保留现有 completion-handler 屏不用重写;坏处是 polar-sdk-swift 已经把这套做完了,等于重复造轮子。**建议只在「短期内不想动 async 迁移」时选它**,并把它当过渡。

> 两个 SDK 可短期并存:`SessionManager` 内部用 PolarAPI 的 `PolarClient`;尚未迁移的旧屏继续走 PolarKit,但 PolarKit 必须先 de-singleton 才能避免和多账号的全局 cookie 打架。所以**无论哪条路,第一步都是干掉 PolarKit 的全局 cookie 依赖。**

## Session 生命周期

- **加账号**:输入 endpoint(预置列表 + 自定义)→ 登录(email/pwd 或 passkey)→ 抓 token 存 Keychain → `GET /api/me` 填元数据 → 建 Account+Session → 设为 active。
- **切换**:改 active 指针(即时)。切到的账号若 token 已过期 → **懒刷新**(用其 refresh_token 打 `/api/token/refresh`)再进入。
- **401 处理(每账号独立)**:活跃账号请求 401 → 用该账号 refresh_token 刷新 → 成功则重放;刷新也失败 → 把该账号标记 `needsReauth`(在切换器上打角标),**不删账号、不踢出其他账号**,只在该账号界面内联提示重新登录。
- **后台刷新**:仅活跃账号定时/proactive 刷新(token <1h 时);非活跃账号切换时再刷,省电省请求。
- **移除账号**:删 Keychain + 元数据 + 断 ws;若删的是 active,自动切到 `lastUsedAt` 最近的另一账号(无账号则回登录页)。

## UX

- **账号切换器**:头像横排 / 下拉 sheet(仿 Slack 左栏);每项显示头像、用户名、endpoint 域名、未读角标、`needsReauth` 警示。
- **加账号入口**:切换器底部「+ 添加账号」→ endpoint 选择(zen/opera/自定义 URL)→ 登录。
- **当前账号标识**:导航栏头像 = active 账号;长按快速切换。
- **无 logout 按钮**:设置里是「移除此账号」;主流程永远不需要登出。

## 横切关注点

- **Workspace**:`X-Workspace-Id` 改为**每账号**的 `activeWorkspaceID`(不再全局)。切账号即切 workspace 上下文。
- **推送**:每账号注册 device token(APNs token 对 endpoint 注册);通知 payload 带 `account hint`/endpoint,点开路由到对应账号并切换。
- **WebSocket(聊天)**:每账号一条 WS;活跃账号保持热连,非活跃账号可断开(省电)或保留 N 个最近的「温连」用于角标。
- **深链 / Universal Links**:解析出 endpoint+资源 → 命中或新增对应账号再打开。
- **缓存隔离**:本地缓存(图片、列表、草稿)按 `account.id` 命名空间隔离,切换不串数据。

## 现状平滑升级

首次升级到多账号版本时:把现有「全局 baseURL + 当前 cookie/token」**包装成 Account #0**,迁移其凭据进 Keychain,设为 active。用户无感,老会话继续可用。

## 分期落地

- **P0 — 地基**:PolarKit de-singleton(或引入 PolarAPI);Bearer 认证 + Keychain 凭据;`Account`/`AccountStore`/`SessionManager`/`PolarSession`;现状升级包装。**此时仍单账号,但结构已就位、cookie 全局依赖已除。**
- **P1 — 多账号 + 切换器**:加账号流程、切换器 UI、每账号 workspace、移除账号、懒刷新/401。
- **P2 — 横切**:per-account 推送、WS、缓存隔离、深链。
- **P3 — 迁移收尾**:旧屏全部迁到 active-session 的 services;移除 PolarKit 单例残留 / 完成 PolarAPI 统一。

## 关键风险

1. **Bearer token 是不透明 session token**(非 JWT,服务端 Redis 查表),30min/24h 过期 + 30天 refresh —— refresh 逻辑必须 per-account 健壮,否则切回旧账号会 401。
2. **同 endpoint 多账号**只有切到 Bearer 才成立;只要还有任何代码路径依赖 `HTTPCookieStorage.shared`,就会串号。**P0 必须把全局 cookie 彻底铲除。**
3. polar-sdk-swift 是 async,PolarKit 是 completion-handler;混用期要有清晰边界,别让一个屏一半 async 一半 completion。
