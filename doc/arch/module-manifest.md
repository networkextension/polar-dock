# 模块权限清单（module manifest）

> 落地 Plane **POLAROPERA-48**。设计文档：windtunnel-2027 `模块权限清单与开放API令牌设计-2026-10-21.md`。
> 相关：`internal-api-v1.md` §1（verify 的 `scopes`）、`open-api-tokens.md`、`module-roles.md`。
> 代码：`internal/app/dock/module_manifest.go`。

## 为什么

以前模块的权限面全写死在 dock：opera 的角色在 `moduleRolePolicies`，token 的 scope 只来自 dock
自己 `/api` 的编译期目录 —— 签发时 `knownScope` 拒收 `opera:…`，开放 API token 没法限到「只读场次」。
现在由**模块自己**在启动时把角色、scope、套餐和路由表交给 dock。

dock **不替模块执行 scope**：verify 把 token 里属于本模块的那部分 scope 交给模块
（`filterScopesForModule`），模块拿自己的路由表去判。dock 存路由表只用于展示与自洽校验。

## 接口

`PUT /internal/v1/modules/{module}/manifest`（HMAC 插件认证；`{module}` 与 body 的 `module` 都必须等于
`X-Polar-Plugin-Name`，否则 403）。

```json
{
  "module": "opera",
  "manifest_version": "<string，例如内容 sha256；空则 dock 用规范化 sha>",
  "roles":   [{"id": "admin", "name": "管理员", "invite_prefix": "WT-ADMIN"}],
  "scopes":  [{"id": "opera:sessions:read", "group": "场次", "name": "读场次 / 阶段 / 时间轴", "sensitive": false}],
  "presets": [{"id": "site-machine", "name": "现场机", "scopes": ["opera:sessions:read"]}],
  "routes":  [{"method": "GET", "path": "/api/opera/v1/sessions/{session_id}", "scope": "opera:sessions:read"}]
}
```

| 状态 | body |
|---|---|
| 200 | `{"ack": true, "changed": <bool>, "version": "<manifest_version>"}` — 规范化 sha 不变时 `changed=false`，不写库 |
| 400 | `{"error": "<原因>"}` — 校验失败、JSON 错、body 超过 256 KB |
| 403 | `{"error": "…"}` — 模块名与调用方不符 |

模块应在每次启动后 PUT 一次；之后只在 `manifest_version` 变化（或收到 404，即老版本 dock）时重发。

## 校验

- `module` == 调用方，≠ `admin`，匹配 `^[a-z][a-z0-9_-]{0,31}$`。
- scope id：`<module>:<res>:<action>`（module 之后至少两段，每段 `[a-z0-9_]+`），或恰好 `<module>:*`。
  **两段式一律拒收**（`opera:write`、`hosts:write`），这样模块 scope 永远不会长得像 dock 自己的
  `domain:action`；与 dock 目录完全相同的 id 也拒收。
- scope / role / preset id 不得重复；preset 与 route 只能引用已声明的 scope；route 的 method ∈
  GET/HEAD/POST/PUT/PATCH/DELETE/OPTIONS，path 以 `/` 开头。
- role id 匹配 `^[a-z][a-z0-9_-]{0,31}$`；`invite_prefix` 可省，填了须匹配 `^[A-Z][A-Z0-9_-]{0,31}$`。
- 上限：scope ≤ 200、preset ≤ 50、route ≤ 1000、role ≤ 50、body ≤ 256 KB。

## 存储与注册表

- 表 `module_manifests(module PK, version, body_sha256, body JSONB, updated_at)`（`store.go` `openDB()`）。
- 进程内注册表：启动时（`NewServer`）从库加载，之后每 5 分钟重载一次（多副本收敛）；PUT 成功即时更新本进程。
- redis 镜像：`{prefix}:manifest:{module}`（无 TTL，JSON body）。

## 谁读注册表

| 位置 | 行为 |
|---|---|
| `knownScope`（token 签发校验） | 接受 dock 自身 scope、`*`、已登记模块声明的 scope、`<已登记模块>:*` |
| `GET /api/api-token-scopes` | 新增 `modules: [{module, display_name, groups:[{group, scopes:[{id,name,sensitive}]}], presets:[{id,name,scopes}]}]`；`display_name` 取 `plugin_modules.display_name`，缺省为模块名；旧字段 `groups` / `wildcard` 不变 |
| `moduleRoleVocabulary` / `validateModuleRole` | manifest 声明了 roles 时以它为封闭词表，邀请码前缀取 `invite_prefix`，缺省 `UPPER(module)-UPPER(role)`；没有 manifest 或 roles 为空时退回写死的 `moduleRolePolicies` |

**opera 注意**：opera 的 manifest 必须给三个角色带上 `WT-ADMIN` / `WT-CREATOR` / `WT-AUDIENCE`，
否则 manifest 一登记，新发的码前缀会变成 `OPERA-RESEARCHER-…`，polar-opera 的 `classifyInviteCode()` 不认。

## 与 dock 自身 scope 的关系

dock 的 `/api` 中间件按精确匹配判 scope（`apiscopes.AllowsScope`，`*` 不含 admin）。一把只带
`opera:*` 或 `opera:…:…` 的 token 调 dock 自己的 `/api` 会因为缺 scope 被 403 —— 模块 scope 不会顺带解锁 dock。
