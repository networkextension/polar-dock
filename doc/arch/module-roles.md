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
