# teams 模块设计文档：协同工作区与权限基础设施

平台至今为止所有资源（应用、证书、ASC config、对话、帖子）都按 `user_id` 单一主体归属。多人协同、跨账号共享资源、邀请审批 —— 这些需求每来一次都得绕过现有模型用脏办法解决。本文档定义平台级的 **teams（协同工作区）** 基础设施，把「资源属于谁」从「user_id」升级到「team_id」，为后续 ascpool 这类多租户场景打底。

## 1. 目标与非目标

**目标**：

- 用户可以创建 / 加入 / 切换 team；切换后看到的所有资源按当前 team 过滤
- Team 有清晰的角色权限（owner / admin / member / viewer）
- 邀请流程支持「邀请尚未注册的邮箱」与「邀请已注册用户」两种入口，统一一条接受逻辑
- 现有所有模块（iosdist / macapp / chat / posts / 后续 ascpool）的资源迁到 `team_id` 拥有
- 每个用户出生自带一个「我的工作区」，零摩擦上手，团队是显式升级而非默认形态

**非目标**：

- 不做组织（org）/ 部门（department）这一层 —— team 是扁平的协作单元，不嵌套
- 不做 ABAC / 表达式权限策略 —— v1 角色固定，资源粒度授权后续按需加
- 不做多租户隔离的物理切分（每 team 独立 DB / schema）—— 仍然单 schema，靠 `team_id` 列做逻辑隔离
- 不做计费 / 配额 —— 等 ascpool 落地后视需求再加

## 2. 名称与 UI 策略

数据模型层面统一叫 **team**。但用户面 UI 上：

- 用户的默认 / 个人工作区显示为「**我的工作区**」（`kind = 'personal'`），不出现「team」字眼
- 显式协作的工作区显示为「**Acme Mobile**」之类的具体团队名称（`kind = 'team'`）
- 顶部 workspace 切换器对两种 kind 一视同仁

后端 API 不区分两种 kind 的 endpoint —— 都是 `/api/teams/*`，但 personal 团队不允许调邀请 / 改成员相关的接口（中间件统一拒），且不能被删除（被 user 注销时一并删）。

## 3. 数据模型

### 3.1 `teams`

```sql
CREATE TABLE IF NOT EXISTS teams (
    id              text PRIMARY KEY,                 -- nanoid / random url-safe
    slug            text NOT NULL UNIQUE,             -- URL 用 (/teams/<slug>)
    name            text NOT NULL,
    description     text NOT NULL DEFAULT '',
    avatar_url      text NOT NULL DEFAULT '',
    kind            text NOT NULL DEFAULT 'team',     -- 'personal' | 'team'
    owner_user_id   text NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_teams_owner ON teams(owner_user_id);
```

约定：personal team 的 `slug` 自动生成 `personal-<user_id 前 8 位>`，name 默认「我的工作区」。owner 删号时级联删 personal team；team 类型不允许 owner 删号未转让。

### 3.2 `team_members`

```sql
CREATE TABLE IF NOT EXISTS team_members (
    team_id     text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        text NOT NULL,                        -- 'owner' | 'admin' | 'member' | 'viewer'
    joined_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_team_members_user ON team_members(user_id);
```

不变量：

- 每个 team 至少一个 `owner` 角色（owner 退出 / 转让前必须先指定继任 owner）
- Personal team 永远只有一个 member，role = `owner`，user_id = team.owner_user_id
- `team_members.role = 'owner'` 必须与 `teams.owner_user_id` 一致；转让 owner 时两侧同时改

### 3.3 `team_invitations`

```sql
CREATE TABLE IF NOT EXISTS team_invitations (
    id                 text PRIMARY KEY,
    team_id            text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    email              text NOT NULL,                 -- 受邀邮箱（不必是已注册用户）
    role               text NOT NULL,                 -- 接受后落 team_members.role
    inviter_user_id    text NOT NULL REFERENCES users(id),
    token              text NOT NULL UNIQUE,          -- 邀请链接里的不可猜密
    status             text NOT NULL DEFAULT 'pending',  -- 'pending'|'accepted'|'declined'|'revoked'|'expired'
    expires_at         timestamptz NOT NULL,          -- 默认 now() + 7 days
    accepted_user_id   text REFERENCES users(id),
    accepted_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_team_invitations_team ON team_invitations(team_id);
CREATE INDEX IF NOT EXISTS idx_team_invitations_email ON team_invitations(lower(email));
CREATE INDEX IF NOT EXISTS idx_team_invitations_token ON team_invitations(token);
```

约定：

- 同一 (team_id, email) 同一时间只允许一条 `status='pending'` 的邀请；二次发邀直接刷新现有记录的 `expires_at` 与 `token`
- 接受时校验：`status='pending'` AND `expires_at > now()`
- 邀请到期 / 撤销 / 接受后 token 失效，复用直接 404

### 3.4 索引：现有模块的 `team_id` 列

每个迁到 team_id 的表都加：

```sql
ALTER TABLE iosdist_apps ADD COLUMN IF NOT EXISTS team_id text;
CREATE INDEX IF NOT EXISTS idx_iosdist_apps_team ON iosdist_apps(team_id);
-- 同样的模式对 iosdist_certificates / iosdist_asc_configs / macapp_* / 后续 ascpool_*
```

迁移期 `team_id` 可空；统一期之后 `NOT NULL`。具体迁移脚本见 §7。

## 4. 角色与权限

### 4.1 角色矩阵（v1）

| 能力 | owner | admin | member | viewer |
|---|---|---|---|---|
| 邀请成员 | ✓ | ✓ | ✗ | ✗ |
| 改成员角色 | ✓ | ✓（不能改 owner / 不能改自己 / 不能造新 owner） | ✗ | ✗ |
| 踢出成员 | ✓ | ✓（不能踢 owner / 不能踢自己） | ✗ | ✗ |
| 改 team 元信息（名 / 描述 / 头像） | ✓ | ✓ | ✗ | ✗ |
| 转让 owner | ✓ | ✗ | ✗ | ✗ |
| 删除 team | ✓ | ✗ | ✗ | ✗ |
| 创建 team 内资源（应用 / 证书 / ASC config 等） | ✓ | ✓ | ✓ | ✗ |
| 编辑 / 删除 team 内资源 | ✓ | ✓ | 自己创建的 | ✗ |
| 查看 team 内资源 | ✓ | ✓ | ✓ | ✓ |
| 自己退出 team | ✓（必须先转让） | ✓ | ✓ | ✓ |

### 4.2 实现：中间件

```go
// internal/app/dock/teams_middleware.go
func RequireTeamRole(minRole ...string) gin.HandlerFunc {
    // 1. 从 X-Workspace-Id header 拿 team_id
    // 2. 查 team_members(team_id, user_id) 拿 role
    // 3. 不在 team / role 不够 → 403
    // 4. 把 team_id + role 注入 context 供 handler 用
}
```

业务 handler 取 `c.MustGet("teamID")` 替代以前的 `c.MustGet("userID")` 做资源过滤。`creator_user_id` 字段保留作审计用，不参与权限判断。

### 4.3 资源粒度的特别约定

- 列表 / 详情接口：按 `team_id = current_workspace_id` 过滤，无视 creator
- member 角色编辑别人创建的资源：v1 默认**不允许**（仅 admin+ 能编辑别人的），v2 按需放开

## 5. API 列表

### 5.1 Team CRUD

| Method | Path | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/api/teams` | 登录 | 创建 team（自动把创建者设为 owner） |
| GET | `/api/teams` | 登录 | 列出我所在的 team（含 personal） |
| GET | `/api/teams/:id` | member+ | team 详情 + 我的角色 |
| PUT | `/api/teams/:id` | admin+ | 改元信息 |
| DELETE | `/api/teams/:id` | owner | 删除（前置：所有资源已迁走 / 删完） |
| POST | `/api/teams/:id/transfer` | owner | 转让 owner 给某成员 |

### 5.2 成员管理

| Method | Path | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/api/teams/:id/members` | member+ | 成员列表 |
| PUT | `/api/teams/:id/members/:user_id` | admin+ | 改角色 |
| DELETE | `/api/teams/:id/members/:user_id` | admin+ 或本人 | 踢出 / 自己退出 |

### 5.3 邀请

| Method | Path | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/api/teams/:id/invitations` | admin+ | 创建邀请（发邮件 + 站内通知） |
| GET | `/api/teams/:id/invitations` | admin+ | 列出 pending 邀请 |
| DELETE | `/api/teams/:id/invitations/:inv_id` | admin+ | 撤销邀请 |
| GET | `/api/invitations/:token` | 公开 | 公开预览邀请（team 名 + 邀请人，未登录可看，决定是否注册） |
| POST | `/api/invitations/:token/accept` | 登录 | 接受 |
| POST | `/api/invitations/:token/decline` | 登录 / 公开 | 拒绝（公开模式下不暴露 team 详情） |
| GET | `/api/invitations/inbox` | 登录 | 我收到的所有 pending 邀请（按当前 user 邮箱匹配 + 已登录但未接受的 token） |

### 5.4 全局 header

```
X-Workspace-Id: <team_id>
```

所有 `/api/*` 请求（不含 `/api/teams/*` 自身和 `/api/invitations/*`）都要带这个 header。中间件解析 → 注入 ctx → 资源过滤。前端 fetch 包装统一塞这个 header，切换 workspace 时改它。

不带 header → fallback 到当前用户的 personal team（兼容旧接口 / 兜底）。

## 6. 邀请流程详细

### 6.1 创建邀请

```
admin 在 team 设置页输入 email + role
  ↓
POST /api/teams/:id/invitations { email, role }
  ↓
1. 校验：email 当前没有 active membership 在该 team
2. 校验：该 (team, email) 没有 pending 邀请；如有则 reuse 并刷新 expires_at + 重发邮件
3. 生成 token (32 字节 url-safe)
4. INSERT team_invitations
5. 发邮件 (邮件模板见 §6.3)
6. 站内通知（如果 email 已注册）
7. 返回邀请记录给前端展示
```

### 6.2 接受邀请的三条路径

**路径 A：受邀邮箱已注册 + 当前已登录**

```
点邮件链接 → /invite/<token>
  ↓
GET /api/invitations/<token> 拿到邀请预览
  ↓
显示「Alice 邀请你以 admin 角色加入 Acme Mobile，[接受][拒绝]」
  ↓
点接受 → POST /api/invitations/<token>/accept
  ↓
1. 校验 token 有效 + 当前登录 user 的邮箱匹配邀请邮箱
2. INSERT team_members
3. UPDATE team_invitations SET status='accepted', accepted_user_id, accepted_at
4. 返回 { team_id, redirect_to: "/teams/<slug>" }
  ↓
前端切换 workspace 到该 team + 跳到 team 页
```

**路径 B：受邀邮箱已注册 + 未登录**

```
点邮件链接 → /invite/<token>
  ↓
公开预览页（无需登录可看 team 名 + 邀请人）
  ↓
点「登录后接受」 → 跳登录页（带 ?return_to=/invite/<token>&prefill_email=invited@...）
  ↓
登录成功 → 自动跳回 /invite/<token> → 路径 A 的接受逻辑
```

**路径 C：受邀邮箱未注册**

```
点邮件链接 → /invite/<token>
  ↓
公开预览页 + 「注册并加入」按钮
  ↓
跳注册页（邮箱字段预填且禁改，挂 ?invite_token=<token>）
  ↓
注册成功（邮箱必须严格等于邀请邮箱）→ 自动调 accept
  ↓
落 team_members + 跳 workspace
```

### 6.3 邮件模板

```
主题：Alice 邀请你加入 Acme Mobile

Alice (alice@acme.com) 邀请你以 admin 身份加入工作区「Acme Mobile」。

加入后你将能：
- 共同管理 Acme Mobile 的应用与发布资源
- 协同审核外测发布

接受邀请: https://platform.example.com/invite/<token>
（邀请将在 7 天后过期）

如果你不认识此发件人，可以忽略此邮件或点击此处拒绝邀请：
https://platform.example.com/invite/<token>/decline

—— Polar 平台
```

文案需要 i18n（参考 `i18n.md`），先做中英双语两版。

### 6.4 已注册用户的「站内通知」捷径

如果邮箱已注册：除了发邮件，同时往 `/api/invitations/inbox` 推一条。前端顶部铃铛角标 +1，点开看到「Acme Mobile 邀请你加入 → 接受 / 拒绝」，省得用户去翻邮件。

## 7. 现有模块迁移

### 7.1 迁移分两阶段

**Phase A（兼容期，不破坏旧代码）**：

1. 加 `teams` / `team_members` / `team_invitations` 三张表
2. 用一个 `BackfillPersonalTeams()` 启动期 hook：扫 `users` 表，每个 user 没 personal team 就建一个，建完往 `team_members` 写 owner 自己
3. 给每个旧模块的资源表加 `team_id text NULL` 列 + index
4. 用一个 `BackfillResourceTeams()` 启动期 hook（idempotent，扫 team_id IS NULL 的 row）：把每行 `team_id` 设为 `creator_user_id` 的 personal team
5. 业务 handler 优先按 `team_id` 过滤，回退按 `user_id` 过滤兼容老调用

**Phase B（统一期）**：

1. 所有写路径都带 `team_id`（中间件强制）
2. 把每个表的 `team_id` 改 `NOT NULL`
3. handler 删 user_id 过滤分支，统一 team_id
4. user_id 列改名为 `creator_user_id`（语义降级为审计字段）

迁移涉及的现有表（按当前 codebase 盘点）：

| 模块 | 表 |
|---|---|
| iosdist | `iosdist_apps` / `iosdist_certificates` / `iosdist_asc_configs` / `iosdist_test_requests` / `iosdist_sign_tasks` |
| macapp（即将） | 全套 `macapp_*` 设计阶段就带 team_id，零迁移 |
| ascpool（即将） | 全套 `ascpool_*` 同上 |
| chat | 暂不迁，chat 是用户私域不是团队资源 |
| posts | 暂不迁，posts 是个人发布不是团队资源 |
| llm_configs | 迁，因为有共享配置的需求 |
| markdown_entries | 暂不迁 |

> chat / posts / markdown_entries 保留 user_id 单主体，跟 team 模型并存。Workspace 切换不影响这些「私域」模块的入口，它们继续按 user_id 过滤。

### 7.2 启动期 backfill 的实现位置

`internal/app/dock/app.go` 的服务启动流程，新增：

```go
backfillPersonalTeams(ctx, s.db)        // step 1: 每个 user 自带 personal team
backfillResourceTeamIDs(ctx, s.db)      // step 2: 旧资源行的 team_id 落到 creator 的 personal team
```

跑成 idempotent —— 重复启动只会写新增的 user / 新增的 row。

## 8. UI 变更

### 8.1 顶部 Workspace 切换器

```
┌─────────────────────────────────────────────────────┐
│ Polar  [▼ 我的工作区   ]    搜索  通知  头像        │
└─────────────────────────────────────────────────────┘
            ↓ 点开
       ┌──────────────────────┐
       │ ✓ 我的工作区          │
       │   Acme Mobile (admin)│
       │   Side Project       │
       │ ─────────────────────│
       │ + 创建新工作区        │
       │ + 接受邀请...        │
       └──────────────────────┘
```

切换后立刻刷新当前页面，所有列表按新 workspace 过滤。当前选中的 workspace_id 写 `localStorage` 持久化，下次进站自动恢复。

### 8.2 Team 设置页 `/teams/:slug/settings`

Tab 结构：

- **基本信息**：名称 / 描述 / 头像（admin+）
- **成员**：列表 + 角色编辑 + 踢出（admin+），自己的退出按钮（所有人）
- **邀请**：发邀请 + pending 邀请列表 + 撤销（admin+）
- **危险区**：转让 owner / 删除 team（owner only）

### 8.3 邀请 inbox

顶部铃铛点开 → 「邀请」tab → 列出我的 pending 邀请 → 一键接受 / 拒绝。

### 8.4 注册页改造

`/signup?invite_token=<token>` 时：

- 预填邮箱 + 禁止修改
- 注册成功后自动 accept → 跳 workspace

## 9. 落地里程碑

每条一个 PR：

- **T0** 本设计文档（`teams.md`）
- **T1** `teams` / `team_members` / `team_invitations` 三张表 + `BackfillPersonalTeams()` 启动 hook
- **T2** Team CRUD API + 创建 / 详情 / 编辑 UI（不含邀请功能）
- **T3** 邀请流程：API + 邮件模板 + 公开预览页 + 接受 / 拒绝 + 注册带 invite_token
- **T4** Workspace 切换器 + `X-Workspace-Id` header + 前端 fetch 包装
- **T5** `RequireTeamRole` 中间件 + 角色权限矩阵实施
- **T6** 现有模块迁移：iosdist 加 team_id + backfill + handler 切到 team_id 过滤；macapp / ascpool 设计期直接 team_id 拥有
- **T7** 转让 owner / 删除 team / 退出 team / 邀请 inbox 角标

T1-T5 跑完后 ascpool 才开工（ascpool.md 起草中，依赖本文档）。

## 10. 已知坑 / 待解决

- **邀请邮件发送**：当前 codebase 没有现成的邮件发送 helper（参考 `auth.md` 没有覆盖）。T3 必须先补一个 SMTP / SendGrid / Resend 适配器。临时方案：dev 环境打到日志里，prod 上线前接通道。
- **token 不可猜**：`crypto/rand.Read(32)` + base64 url-safe encode，长度 ≥ 43 字符。
- **slug 冲突**：team name 转 slug 时若冲突自动加随机后缀 `acme-mobile-x7k2`。Personal team slug 永远是 `personal-<user_id 前 8 位>`，不冲突。
- **owner 删号**：personal team 跟着删；team 类型必须先转让或删 team，否则注销动作前置检查拒。
- **viewer 角色实际场景少**：v1 留 schema 但 UI 上先不暴露选项，admin 邀请只能选 admin / member。后面验证有需求再开 viewer。
- **跨 team 资源迁移**：用户在 team A 建的应用想转到 team B —— v1 不做，资源跟 team 锁死，需要时复制粘贴。
- **审计**：所有改 team_members / 删资源的动作落审计日志（`team_audit_log` 表），v1 留 schema 不展示，将来出 audit UI 时直接用。
- **频率限制**：邀请 API 要限速（每 user 每分钟最多 10 条），防滥发。
- **chat / posts 不迁**：明确划分「团队资源」和「私域内容」，不要被 workspace 切换器绑架。

## 11. 跟 ascpool 的衔接

ascpool.md 的「team」概念直接复用本文档定义的 `teams` 表，差异点：

- ASC 凭据（`ascpool_team_credentials` 表）外键到 `teams.id`，每 team 至多一份（也可设计成多份配多 team account，第一版一份足够）
- ascpool 引入新角色 `reviewer`（专做外测审核，不能管成员），扩展 `team_members.role` 取值集合，中间件矩阵补这一行
- Share pool 是 `teams.share_enabled` 布尔字段（teams 表加该列，普通 team 默认 false，ascpool 场景下 admin 显式打开）

ascpool.md 起草时会明确这层依赖。

---

> 文档位置：repo 根 `teams.md`，与 `iosapp.md` / `macapp.md` 平级。落地过程中如果与本文档偏差超过一个 PR 的范围（比如重命名表、改角色矩阵），优先回过来更新。
