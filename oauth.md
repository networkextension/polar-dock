# oauth 模块设计文档：第三方 OAuth 接入（正式版）

平台目前只有「邮箱 + 密码 + Passkey」一条登录路径，资源接入（ASC、GitHub、Cloudflare R2 等）也都是用户在控制台手动粘贴 API Key 进来。这份文档定义平台层 **正式版 OAuth** 的统一接入方案，覆盖两类用途：

1. **登录 OAuth**（sign-in）—— 用户用 Google / GitHub / Apple ID / 微信 等第三方账号登录平台。
2. **资源 OAuth**（resource）—— 用户授权平台代他/她调用第三方 API：
    - GitHub（拉 / 创 / push 仓库 —— 给将来的「polar 管理用户 repo」流程用）
    - Apple Developer / ASC（替代「上传 .p8 文件 + Issuer + Key ID」的体验）
    - Cloudflare（R2 bucket 自动配置）
    - 其他

MVP 阶段（用户已确认）：repo 走「polar 内部 local filepath」、sign-in 走密码 / Passkey，所以本模块**整体推迟落地**。本 doc 锁定形态，留给后续真要做时直接照着实现。

## 1. 范围与非目标

**目标**：

- 一套统一抽象同时支持 sign-in + resource OAuth，避免每接一家服务就重写一套
- 凭据加密落库（access token / refresh token / scopes），跟 ASC `.p8` 同一把 KEK
- 自动 refresh token rotation（用户不需要重新授权）
- CSRF 防护（state）+ PKCE（SPA flow）+ scopes 最小化
- 多 provider 共存：用户可同时连 GitHub + Google + Apple 多家
- 取消授权 / 重连 流程清晰

**非目标**：

- SAML / SSO 企业身份联邦（不在 OAuth 范畴；要做单独再设计）
- IdP（自己当 OAuth provider 给别人用）—— 平台只做 OAuth client
- 设备码 flow（device code）—— CLI 用得到，但优先级低于 web flow
- 自带证书的 mTLS / Apple App Attest —— 跟 OAuth 正交

## 2. 整体架构

```
[用户在控制台]
    ↓ 点「连接 GitHub」/ 「用 Google 登录」
[前端跳到 /oauth/<provider>/start?intent=signin|connect]
    ↓ 平台后端写 state cookie + redirect 到 provider authorize URL
[provider 授权页]
    ↓ 用户同意 → 跳回平台 /oauth/<provider>/callback?code=...&state=...
[平台后端校验 state，用 code 换 token]
    ↓
按 intent 分流：
  ├── signin: 拿 provider 的 user info，
  │       创建/匹配 platform user，写 session cookie
  └── connect: 把 token 落 oauth_connections 表，绑当前登录的 user
```

跨页面 / 跨 provider 走同一组 endpoint，靠 path 参数区分 provider。

## 3. 数据模型

### 3.1 `oauth_providers`（运营配置）

平台管理员配，列存放 client_id / client_secret / scopes 等 per-provider 静态参数。**不放在 .env 是因为可能会有多个**（一个是「用 Google 登录」一个是「连接 Google Drive」），且要支持运行时旋转 secret。

```sql
CREATE TABLE oauth_providers (
    id TEXT PRIMARY KEY,
    -- 'github_signin' / 'github_repo' / 'google_signin' / 'apple_signin' / 'apple_asc' / 'cloudflare_r2' / etc.
    name TEXT NOT NULL UNIQUE,
    -- 'github' / 'google' / 'apple' / 'cloudflare' / ... 用来选 Go 端的 provider 适配器
    kind TEXT NOT NULL,
    -- 'signin' (intent: 创建 / 登录 user) | 'resource' (intent: 拿 token 给 user 持久持有)
    intent TEXT NOT NULL,
    client_id TEXT NOT NULL,
    client_secret_enc BYTEA NOT NULL,            -- AES-GCM, KEK = IOSDIST_RESOURCE_KEY
    authorize_url TEXT NOT NULL,                 -- 默认从适配器拿，允许覆盖（自部署 GHE 等）
    token_url TEXT NOT NULL,
    revoke_url TEXT NOT NULL DEFAULT '',
    user_info_url TEXT NOT NULL DEFAULT '',
    scopes TEXT NOT NULL DEFAULT '',             -- 空格分隔
    -- redirect_uri = <publicBaseURL>/oauth/<id>/callback，由代码生成不入库
    pkce_required BOOLEAN NOT NULL DEFAULT FALSE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 3.2 `oauth_connections`（每用户的授权关系）

```sql
CREATE TABLE oauth_connections (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL REFERENCES oauth_providers(id) ON DELETE RESTRICT,
    remote_user_id TEXT NOT NULL,                -- provider 那边的稳定用户 id（不是 email）
    remote_email TEXT NOT NULL DEFAULT '',
    remote_login TEXT NOT NULL DEFAULT '',       -- 'octocat' / 'foo@gmail.com' 显示用
    access_token_enc BYTEA NOT NULL,
    refresh_token_enc BYTEA,                      -- 可空（不是所有 provider 都给 refresh token）
    expires_at TIMESTAMPTZ,
    scopes TEXT NOT NULL DEFAULT '',
    -- raw user info JSON 缓存，省得每次调 user_info 端点
    profile JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_id, remote_user_id)
);
CREATE INDEX idx_oauth_connections_user ON oauth_connections(user_id);
```

设计点：
- `(provider_id, remote_user_id)` 唯一 —— 同一个 GitHub 账号不能绑两个平台 user
- 用户可以在不同 provider 各连一份（各自一行）
- access_token / refresh_token 全程加密落库，复用 ASC 加密用的 `IOSDIST_RESOURCE_KEY`
- profile 缓存 provider 上一份的用户名 / 头像，省调用次数

### 3.3 `oauth_state_pending`（CSRF 防护）

state 用一次性 token 避免在内存里维护映射（多实例部署也安全）。

```sql
CREATE TABLE oauth_state_pending (
    state TEXT PRIMARY KEY,                       -- 32 random bytes base64-url
    provider_id TEXT NOT NULL REFERENCES oauth_providers(id) ON DELETE CASCADE,
    intent TEXT NOT NULL,                          -- 'signin' | 'connect'
    user_id TEXT REFERENCES users(id) ON DELETE CASCADE,  -- connect 时填，signin 时 NULL
    pkce_verifier TEXT NOT NULL DEFAULT '',
    return_to TEXT NOT NULL DEFAULT '/',
    expires_at TIMESTAMPTZ NOT NULL,                -- now + 10min
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_oauth_state_expires ON oauth_state_pending(expires_at);
```

启动时清过期 row（跟 `expireOldInvitations` 同套路）。

## 4. provider 适配器（Go 端）

```go
// internal/app/dock/oauth_provider.go
type OAuthProvider interface {
    Kind() string  // "github" | "google" | "apple" | ...
    AuthorizeURL(state, codeChallenge string) string
    ExchangeCode(ctx context.Context, code, codeVerifier string) (*OAuthTokens, error)
    Refresh(ctx context.Context, refreshToken string) (*OAuthTokens, error)
    FetchUserInfo(ctx context.Context, accessToken string) (*OAuthUserInfo, error)
}

type OAuthTokens struct {
    AccessToken  string
    RefreshToken string  // 可空
    ExpiresIn    int     // 秒；0 表示不过期或未告知
    Scopes       []string
}

type OAuthUserInfo struct {
    RemoteID    string  // 稳定 ID（GitHub: numeric id，Google: sub claim，Apple: sub claim）
    Email       string
    EmailVerified bool
    Login       string  // 显示名（GitHub username / Google name）
    AvatarURL   string
    Raw         map[string]any  // 原始 JSON
}
```

**初版适配器清单**：

- `github`：标准 OAuth 2.0 + `/user` endpoint
- `google`：OIDC + ID token 解析（不必专门 fetch user info）
- `apple`：JWT-signed client secret + Sign in with Apple；email 可能是 relay
- `cloudflare`：CF Access OAuth，给 R2 自动配置用
- `appstoreconnect`：Apple 的 ASC App-Specific Tokens；目前 ASC 只支持 .p8 私钥，OAuth 流程暂没开放，预留以后

## 5. HTTP 路由

| Method | Path | 说明 |
|---|---|---|
| GET | `/oauth/:provider/start` | 进入授权页。query: `intent=signin\|connect`, `return_to=/path` |
| GET | `/oauth/:provider/callback` | provider 回调；code → token → connection / signin |
| POST | `/api/oauth/connections` | 列出当前 user 的 connections |
| DELETE | `/api/oauth/connections/:id` | 解绑（删 connection，可选调 provider revoke） |
| POST | `/api/admin/oauth/providers` | admin 配置 provider（client_id/secret/scopes） |
| GET | `/api/admin/oauth/providers` | admin 列表 |
| PUT | `/api/admin/oauth/providers/:id` | admin 改 |
| DELETE | `/api/admin/oauth/providers/:id` | admin 删 |

**前端发起授权**：直接 `window.location.href = "/oauth/github_signin/start?intent=signin"` —— 不走 fetch（要跨域跳 provider 授权页）。

**回调处理后跳回**：`return_to` 从 state 还原；signin 默认 `/dashboard.html`，connect 默认设置页 `/dashboard.html?tab=connections`。

## 6. 关键流程

### 6.1 用 Google 登录（首次）

```
1. 未登录用户点「用 Google 登录」
2. → /oauth/google_signin/start?intent=signin&return_to=/dashboard.html
3. 平台:
   a. 生成 state (32 random bytes) + PKCE code_verifier
   b. INSERT oauth_state_pending (state, provider_id=google_signin, intent=signin, expires=now+10min)
   c. 302 to google authorize_url
4. Google 授权 → 回 /oauth/google_signin/callback?code=...&state=...
5. 平台:
   a. SELECT + DELETE oauth_state_pending WHERE state=...
   b. provider.ExchangeCode(code) → tokens
   c. provider.FetchUserInfo(access_token) → user_info
   d. 找现有 user：
      - 先查 oauth_connections WHERE (provider_id, remote_user_id) → 命中直接登录
      - 否则查 users WHERE email = user_info.Email AND email_verified=true → 自动绑（设置 oauth_connection）
      - 否则创建新 user：username 从 user_info.Login 派生；email_verified=user_info.EmailVerified
   e. INSERT/UPDATE oauth_connections
   f. 写 session cookie
   g. 302 to return_to
```

### 6.2 已登录用户连接 GitHub（resource intent）

```
1. /oauth/github_repo/start?intent=connect
2. 平台:
   a. 生成 state + verifier
   b. INSERT oauth_state_pending (state, provider_id=github_repo, intent=connect, user_id=current, ...)
   c. 302 to github authorize_url
3. github 回 /oauth/github_repo/callback?code=...
4. 平台:
   a. ExchangeCode → tokens (含 scope=repo)
   b. FetchUserInfo → remote_user_id, login
   c. UPSERT oauth_connections (provider_id, remote_user_id) DO UPDATE access_token+refresh_token+expires
   d. 302 to /dashboard.html?tab=connections&connected=github
```

### 6.3 调用第三方 API 时的 token 复用

```go
// internal/app/dock/oauth_client.go
func (s *Server) getOAuthTokenForUser(ctx context.Context, userID, providerID string) (string, error) {
    conn, err := s.getOAuthConnection(userID, providerID)
    if err != nil || conn == nil {
        return "", errOAuthNotConnected
    }
    if conn.ExpiresAt != nil && time.Until(*conn.ExpiresAt) < 60*time.Second {
        // 自动续期
        provider := s.lookupOAuthProvider(providerID)
        newTokens, err := provider.Refresh(ctx, decryptOAuthSecret(conn.RefreshTokenEnc))
        if err != nil {
            return "", err  // 由调用方决定是否提示用户重连
        }
        s.updateOAuthConnectionTokens(conn.ID, newTokens)
        return newTokens.AccessToken, nil
    }
    return decryptOAuthSecret(conn.AccessTokenEnc), nil
}
```

模块代码示例：

```go
// 比如 GitHub repo create flow
ghToken, err := s.getOAuthTokenForUser(ctx, userID, "github_repo")
if errors.Is(err, errOAuthNotConnected) {
    return prompt("请先在设置 → 集成 里连接 GitHub")
}
client := github.NewClient(ghToken)
client.Repositories.Create(...)
```

## 7. 安全 / 合规

- **state 一次性**：callback 处理时 DELETE，重放无效。10 分钟过期。
- **PKCE 强制**（对要求的 provider）：state 一并存 verifier，callback 用 verifier 兑换 code。
- **token 加密落库**：AES-256-GCM，KEK = `IOSDIST_RESOURCE_KEY`。decrypted token 在内存里用完就丢，不写日志。
- **scopes 最小化**：每个 provider 配置只申请实际要用的 scope（GitHub repo 只要 `repo` 不要 `admin:org`）。
- **revoke**：用户解绑时调 `provider.revoke_url` 把 token 在 provider 那边也吊销，避免「我以为已经断开了，token 还能用 30 天」。
- **email 信任策略**：自动绑既存 user 仅当 `email_verified=true` 且平台侧 `users.email_verified=true`，否则要求用户登录后手动 connect（防止「我注册时没验邮箱，被人通过 OAuth 抢号」）。

## 8. UI 入口

**登录页 `/login.html`**：

```
┌─ 邮箱 + 密码 ──────────────┐
│ [email]                    │
│ [password]    [Login]      │
└────────────────────────────┘
        — 或 —
[ Sign in with Google ]
[ Sign in with GitHub ]
[ Sign in with Apple ]
[ Use Passkey ]                (现有的)
```

**设置 → 集成 / Connections**：

```
┌─ 已连接的服务 ─────────────────────────┐
│ ✓ GitHub (octocat)         [断开]      │
│ ✓ Google (alice@gmail.com) [断开]      │
│ ○ Cloudflare              [连接 →]     │
│ ○ Apple ID                 [连接 →]    │
└────────────────────────────────────────┘
```

每个 connection 显示：provider name + remote login + 连接时间 + 上次使用时间（可选）+ 断开按钮。

## 9. 落地里程碑

每条一个 PR：

- **O0** 本设计文档
- **O1** 表 + provider 注册抽象 + admin 配置 UI（不接任何 provider）
- **O2** GitHub provider 适配器 + signin flow（验证最小可行链路）
- **O3** GitHub repo connection（resource intent）+ token refresh + getOAuthTokenForUser helper
- **O4** Google signin
- **O5** Apple Sign In with Apple（ JWT client secret 那一套）
- **O6** 取消 / 重连 / scopes 升级流程
- **O7** Cloudflare R2（如果产品要自动配 R2 bucket）
- **O8** 设备码 flow（CLI：`polar-agent login` 不开浏览器，给 token）

## 10. 与已有模块的集成点

| 模块 | 集成点 |
|---|---|
| `users` | OAuth signin 创建 / 匹配 user；email_verified 反推 |
| `auth.md` 的 session | OAuth 成功后走同一套 access cookie + refresh cookie |
| `Passkey` | 并存，互不影响。一个 user 可以有 Passkey + 多 OAuth connection |
| `iosdist_asc_configs` | 未来如果 Apple 开放 ASC OAuth，这套替代 .p8 上传 |
| `local_agent` | 设备码 flow 给 CLI 用 |
| 计划中的 `repos` | GitHub OAuth + repo create / push API |
| 计划中的 `ascpool` | 简化合作团队接入 ASC（不再让他们手动从 ASC 后台导出 .p8） |

## 11. 已知坑

- **Apple Sign in with Apple 的 client secret 是 JWT** —— 不能像普通 OAuth 那样存固定 secret，要每次签 JWT。需要 ECDSA P-256 私钥 + Apple Team ID + Service ID。把 .p8 加进 oauth_providers.client_secret_enc，签 JWT 在 Refresh / ExchangeCode 适配器内部做。
- **Apple email relay**：用户首次授权时勾「隐藏邮箱」就会得到 `xyz@privaterelay.appleid.com`。要支持，但匹配既存 user 时不能靠 email 匹配（因为 relay 邮箱跟用户真实邮箱不一样）。仅靠 remote_user_id（sub claim）匹配。
- **GitHub 没有 refresh token**（默认）：access token 长期有效（直到用户手动撤销），不需要 refresh。代码里要对 `nil refresh_token + expires_at = NULL` 容错。
- **多个 OAuth provider 同 email**：用户用 google 注册了一个 user，再用 github 同邮箱「登录」时，匹配既存用户。文档里要明确这一点，避免用户产生「为什么没创建新账号？」的惊讶。
- **scope 升级**：先连了 `repo`，后面想加 `delete_repo`，需要让用户重新走一次授权（不能后台偷偷加）。UI 要清楚告诉用户「需要重新授权才能用新功能」。
- **私有部署 GitHub Enterprise / Google Workspace**：authorize/token URL 不是写死的 `github.com`，要从 oauth_providers 表里读。**初版按全局支持设计**，省得日后改表结构。

## 12. MVP 状态（当前不做）

跟用户对齐：
- repo 走「polar 内部 local filepath」 → GitHub OAuth 推迟
- sign-in 走密码 + Passkey → 登录类 OAuth 推迟
- ASC 仍用 .p8 文件上传
- Cloudflare R2 直接用 access key 配置

本 doc 落地从 O1 开始就是「真要接第三方时」的工作。

---

> 文档位置：repo 根 `oauth.md`，与 `iosapp.md` / `teams.md` / `ascpool.md` / `macapp.md` 平级。
