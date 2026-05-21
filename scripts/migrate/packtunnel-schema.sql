-- ============================================================
-- polar_packtunnel schema — end-state (post-T6).
--
-- Apply:
--   CREATE DATABASE polar_packtunnel OWNER ideamesh;
--   psql -d polar_packtunnel -f scripts/migrate/packtunnel-schema.sql
--
-- Cross-DB refs (TEXT, resolved via dock SDK):
--   - owner_user_id  → /internal/v1/users/:id
--   - workspace_id   → /internal/v1/teams/:id
-- ============================================================

CREATE TABLE IF NOT EXISTS packtunnel_profiles (
    id TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL,
    workspace_id TEXT,
    name TEXT NOT NULL,
    profile_type TEXT NOT NULL,
    server_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    auth_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    options_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    transport_config JSONB,
    priority INTEGER NOT NULL DEFAULT 0,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    editable BOOLEAN NOT NULL DEFAULT TRUE,
    source TEXT NOT NULL DEFAULT 'local',
    country_code TEXT NOT NULL DEFAULT '',
    country_flag TEXT NOT NULL DEFAULT '',
    is_active BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_packtunnel_profiles_owner_updated_at
    ON packtunnel_profiles(owner_user_id, updated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_packtunnel_profiles_workspace
    ON packtunnel_profiles(workspace_id, updated_at DESC);

-- packtunnel_rule_files: end-state per-workspace, not per-user. Dock's
-- migration history dropped the owner_user_id PK and replaced with a
-- partial unique on workspace_id; fresh DB just goes straight there.
CREATE TABLE IF NOT EXISTS packtunnel_rule_files (
    id BIGSERIAL PRIMARY KEY,
    owner_user_id TEXT NOT NULL,
    workspace_id TEXT,
    file_name TEXT NOT NULL,
    stored_name TEXT NOT NULL,
    file_path TEXT NOT NULL,
    file_size BIGINT NOT NULL DEFAULT 0,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    uploaded_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_packtunnel_rule_files_workspace
    ON packtunnel_rule_files(workspace_id) WHERE workspace_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_packtunnel_rule_files_uploaded_at
    ON packtunnel_rule_files(uploaded_at DESC);
