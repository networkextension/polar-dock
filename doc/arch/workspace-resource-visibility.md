# Workspace resource visibility — the 上下 / 东西 / 南北 model

Status: **P0 (mounts foundation) implemented, flag-gated default-off.** Consumption
wired for one resource kind (LLM configs) as the reference; other kinds follow the
same pattern.

## Problem

A workspace is two things at once: someone's private "basement" (个人地下室 — their
home workspace) and a set of shared collaboration rooms (协作区). Resources scatter:
an LLM config, an asset folder, a prompt template set up in one workspace isn't
visible in another, so people re-create them per workspace. We want a model where
resources can be **deliberately shared across the workspace graph** without copying,
under explicit control.

Three axes (the user's framing):

- **上下向 — containment / inheritance.** Platform → workspace. "Utility" resources
  configured once at the platform level flow *down* into every workspace. Already
  shipped for LLM configs via `is_platform` (#412) + the "设为平台级" toggle (#28).
  This axis is about *defaults that cascade*.
- **东西向 — membership.** A user belongs to several collaboration workspaces with
  different people. This is the existing teams/members model. Nothing new here.
- **南北向 — sharing / mounting.** A workspace can **mount** a resource (or a whole
  resource-kind) from *another* workspace it has access to, making it visible in
  place — no copy, no re-config. This is the new mechanism this doc introduces.

`is_platform` is the special case of a mount where the source is "the platform" and
the target is "everyone". Mounts generalize it to arbitrary workspace→workspace,
whole-kind or single-resource, read-only or read-write.

## Mounts

A **mount** records: *workspace T makes resource(s) from workspace S visible to T's
members.* One row in `workspace_mounts`:

| column                | meaning                                                        |
|-----------------------|---------------------------------------------------------------|
| `workspace_id`        | the **target** — where the resource becomes visible           |
| `source_workspace_id` | the **source** — where the resource actually lives            |
| `resource_kind`       | `llm_config` \| `asset` \| `prompt` \| `*` (all kinds)        |
| `source_resource_id`  | a specific resource id, or NULL = *all of that kind* in source |
| `mode`                | `read` (default) \| `readwrite`                                |
| `created_by`, `created_at` | audit                                                    |

### Invariants

1. **You can't mount what you can't see.** Creating a mount requires the actor to be
   a member of *both* the target (team:manage) and the **source** workspace. Enforced
   in `handleCreateMount`.
2. **Mounts widen READ visibility; they never widen WRITE by default.** `mode=read`
   means mounted resources appear in list/get but edits still require rights in the
   *source* workspace. `readwrite` is reserved for a later phase (not yet consumed).
3. **No transitive mounts.** A mount resolves one hop only (T sees S's resources; it
   does not see what S itself mounted). Keeps resolution O(1) and prevents cycles.
4. **Self-contained per kind.** Each consuming query opts in explicitly via
   `mountedSourceWorkspaceIDs(target, kind)`; there is no global magic that silently
   changes every query.

## Resolution

`mountedSourceWorkspaceIDs(targetWorkspaceID, kind) ([]string, error)` returns the
source workspace ids whose `kind` resources are mounted into the target (matching
`resource_kind = kind OR '*'`). A consuming list query unions
`WHERE workspace_id = $target OR workspace_id = ANY($mountedSources) [OR is_platform]`.

`source_resource_id` (single-resource mounts) is carried in the row for the UI and
for a future per-resource filter; the P0 union is at workspace granularity.

## Phasing

- **P0 (this change).** `workspace_mounts` table (boot-created, idempotent); typed
  CRUD `GET/POST/DELETE /api/teams/:id/mounts` (team:manage + source-membership +
  audited); `mountedSourceWorkspaceIDs` resolver; **LLM-config list** unions mounted
  sources when `POLAR_WORKSPACE_MOUNTS=true` (default off → fully behavior-preserving).
- **P1.** Wire the resolver into assets + prompt listings; a mounts management UI on
  `/admin-rbac.html` (or a dedicated page); single-resource (`source_resource_id`)
  granularity in the union.
- **P2.** `readwrite` mounts (delegated editing into a source), with `Can()` checked
  against the *source* workspace; mount-aware quotas.
- **P3.** Default "home → team" mounts created automatically at onboarding so a user's
  personal utility resources surface in their teams (the 上下向 default, generalized).

## Why flag-gated

Mounts change *what resources a user sees*. That is a visibility change with real
blast radius, so P0 ships inert: the table + CRUD exist and can be populated, but no
list query consults them until `POLAR_WORKSPACE_MOUNTS=true`. Mirrors the
`POLAR_ENFORCE_TEAM_STATUS` / `POLAR_ENFORCE_LLM_ADMIN` rollout convention. Rollback =
unset the env (dock restart); the rows are harmless when unconsumed.
