-- grafana-readonly.sql — bootstrap a read-only Postgres role for Grafana.
--
-- WHY whitelist instead of GRANT SELECT ON ALL TABLES IN SCHEMA public:
-- the dock schema includes tables with sensitive columns (users.password_hash,
-- llm_configs.api_key, iosdist_certificates.password_cipher, etc.). A blanket
-- grant would expose those to whoever can read the Grafana queries / explore
-- view. We grant explicitly to the tables Phase 4 dashboard panels read from.
--
-- Run this as a Postgres superuser ONCE on the dock database:
--   psql -d ideamesh -f deploy/grafana/sql/grafana-readonly.sql
--
-- Then put the password into Grafana's datasource config (or env), and update
-- deploy/grafana/datasources.yml password placeholder.

-- Idempotent: drop-and-recreate is destructive (kills running sessions), so
-- prefer DO blocks that no-op when the role already exists.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'polar_grafana') THEN
    CREATE ROLE polar_grafana WITH LOGIN PASSWORD 'CHANGE_ME_BEFORE_RUNNING';
  END IF;
END
$$;

-- Connect privilege on the database (operator may need to swap 'ideamesh'
-- for whatever the actual db name is).
GRANT CONNECT ON DATABASE ideamesh TO polar_grafana;
GRANT USAGE ON SCHEMA public TO polar_grafana;

-- Whitelist of tables Grafana panels query. Update when adding new SQL panels.
GRANT SELECT ON
  project_research_runs,
  project_task_retrospects,
  project_tasks,
  projects,
  teams,
  chat_messages,
  chat_threads,
  markdown_entries,
  iosdist_sign_tasks,
  iosdist_apps
TO polar_grafana;

-- Future tables get GRANT'd via ALTER DEFAULT PRIVILEGES only if explicitly
-- safe; we intentionally don't ALTER DEFAULT here so the operator stays in
-- control of what Grafana can read.

-- Sanity: confirm the role can SELECT from one of the granted tables.
-- Run as the role to verify (`psql -U polar_grafana -d ideamesh`):
--   SELECT count(*) FROM project_research_runs;
