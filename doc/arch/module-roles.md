# 模块角色 & 带角色的邀请码

> 落地 Plane **POLAROPERA-77**（线上邀请码注册：把「邀请码→角色」落到 dock 侧）。
> 相关：`internal-api-v1.md` §1、`permission-model.md`、polar-opera `packages/sdk/src/auth-identity.ts`。

## 为什么不是 `users.role`

dock 的 `users.role` 是**平台角色**：`admin` 就是站点管理员（legacy 口径，`AdminMiddleware`
现在走 `IsSiteAdmin`，但 `POLAR_LEGACY_ADMIN_ROLE=1` 仍可回退到它，且 llm-bot、dashboard、
post 等处仍直接比对 `role == "admin"`）。

风洞（`opera`）自己有一套角色：`admin` / `researcher` / `audience`。如果把「风洞管理员」写成
`users.role='admin'`，这个人就顺带成了**整个平台**的站点管理员 —— 一次越权。

所以模块角色是**另一根轴**：

```
module_user_roles(module, user_id, role, granted_by, source_code, granted_at)
PRIMARY KEY (module, user_id)
```

只对拥有它的模块可见。

## 谁能看到

| 出口 | 字段 | 说明 |
| --- | --- | --- |
| `GET /internal/v1/auth/verify` | `role` / `platform_role` / `module` / `module_role` | 按 HMAC 认证过的 `X-Polar-Plugin-Name` 解析。有授权时覆写 `role`，插件零改动即可拿到；`platform_role` 保留原 `users.role`。插件读不到、也伪造不了别的模块的角色。 |
| `GET /api/me` | `module_roles: {"opera":"researcher"}` | 给登录门户用：登录后据此决定落到哪个模块首页。 |
| `POST /api/register` | `module` / `module_role` | 邀请码带角色时返回，注册即落地。 |

没有授权的人 `module_role` 为空、`role` 不变 —— **从没发过授权的插件行为完全不变**。

## 邀请码

`invite_codes` 增加 `module` / `role` / `note` / `expires_at`。带 `module+role` 的码在注册被兑换时
写入一条 `module_user_roles`。

- 码面前缀（`WT-ADMIN-…` / `WT-CREATOR-…` / `WT-AUDIENCE-…`）**只是给人看的**：兑换永远查库，
  自己编一个 `WT-ADMIN-xxx` 拿不到任何东西。前缀与 polar-opera `classifyInviteCode()` 对齐，
  改动任一侧都要同步（`module_roles_test.go` 钉住了这三个前缀）。
- 词表是**封闭**的：`opera` 只认 `admin` / `researcher` / `audience`。前端把 `researcher` 叫
  `creator`（主创），那是 UI 的词，dock 侧不收 —— 两套词共存迟早漂移。
- 只要**填了**邀请码就必须校验：不认识 → `403 auth.invite_invalid`，不静默降级成普通注册。
  （以前只在「站点要求邀请码」时才校验。）
- 一次性；支持 `expires_in_days` 过期；未兑换的可停用。

## 管理端点（站点管理员）

```bash
# 发 5 个主创码，90 天过期
POST /api/site-settings/invite-codes
     {"count":5,"module":"opera","role":"researcher","expires_in_days":90,"note":"2026 秋季场"}
  → {"codes":[{"code":"WT-CREATOR-A1B2C3D4E5", ...}]}     # 返回的是带横杠的可读形式

# 停用一个还没被用掉的码
POST /api/site-settings/invite-codes/disable   {"code":"WT-CREATOR-A1B2C3D4E5"}

# 直接给已有账号授予/撤销（存量用户回填走这里）
GET    /api/site-settings/module-roles?module=opera
POST   /api/site-settings/module-roles   {"module":"opera","role":"admin","email":"a@b.c"}
DELETE /api/site-settings/module-roles?module=opera&user_id=u_abc
```

`GET` 会顺带返回该模块的合法角色列表（`roles`），管理页据此渲染下拉而不是自由输入框。

## 上线注意：存量账号

polar-opera 的 `deps.py` 目前把「非 admin 非 audience」一律当 `researcher`，也就是**任何 dock
账号登录风洞都是主创**。本次改动本身不改变这一点（没有授权 → `module_role` 为空 → `role` 仍是
`user` → deps.py 仍映射成 researcher）。

真正收紧发生在 polar-opera 侧改用 `module_role` 之后。那一步上线**之前**，要先给现有的主创 /
管理员补授权，否则他们会掉成观众：

```bash
POST /api/site-settings/module-roles {"module":"opera","role":"admin","email":"<管理员邮箱>"}
POST /api/site-settings/module-roles {"module":"opera","role":"researcher","email":"<主创邮箱>"}
```

站点管理员本人不受影响（`platform_role=admin`，且可以自己给自己发码）。

---

## 角色只说「他是谁」，不说「他在哪」—— 团队归属

模块角色回答的是身份问题。但下游插件（风洞）的每一次读写都按 **team 作用域**，
所以只发角色不给团队，得到的是一个**能登录、什么也做不了**的账号：注册成功、
`module_roles` 里有 `opera: researcher`，一进风洞却被挡在「你还不在任何团队里」。

2026-09-15 线上真出过：`sx520@163.com` 兑了 `WT-CREATOR-` 码，拿到 researcher，
落地无团队。同期 41 个有 opera 角色的账号里，**34 个不在任何 team**。

根因是 `handleRegister` 全程不碰 `teams` / `team_members`（`createUser` 连个人区
都不建 —— 那是懒创建的）。这条链路现在补上了。

### 邀请码带 team

```bash
POST /api/site-settings/invite-codes
{"count":5,"module":"opera","role":"researcher",
 "team_id":"<team>","team_role":"member"}
```

- `team_id` 省略 → 兑码时回落到 `site_settings.default_team_id`（**仅当多租户关闭**）
- `team_role` 只接受 **`member` / `viewer`**，发码与入库两处都拦

🔴 **为什么必须拦 owner/admin**：`IsSiteAdmin` 的判据是「root team 成员**且** role ∈
{owner, admin}」（`perm_site_admin.go`）。单租户部署里 default team 往往**就是** root
team，放行 `admin` 等于让一张普通邀请码批量制造站点管理员。

### 多租户开关

`site_settings` 两个字段，`PUT /api/site-settings` 可改：

| 字段 | 默认 | 含义 |
|---|---|---|
| `multi_tenant_enabled` | `false` | 关 = 单租户；不带 team 的码落到 default team |
| `default_team_id` | `''` | 单租户下的落脚团队；关多租户时必填，否则 400 |

开多租户后，不带 team 的码**故意不给团队** —— 多个租户共存时没有「默认」可言。

### 把已有账号加进团队

此前只有「发邮件邀请等对方接受」和「兑码注册」两条路，都要对方动作。存量账号
（角色早就有、团队从没配）没有任何入口，只能手写 SQL。现在：

```bash
POST /api/teams/<team_id>/members {"user_id":"<uid>","role":"member"}
```

team admin 权限，`role` 同样限 `member` / `viewer`；主工作区（home team）拒绝加人。
提升成 admin 仍走 `PUT /api/teams/:id/members/:user_id`。
