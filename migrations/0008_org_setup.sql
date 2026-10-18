-- Org, configuration, seats.
-- users / teams / team_members / org reserve room for seats & licenses (later):
-- created in this migration so later features do not have to alter tables. Plural
-- names because `user` is a Postgres keyword.

CREATE TABLE IF NOT EXISTS users (
  id            text PRIMARY KEY,                 -- grant.user_id
  email         text,
  name          text,
  seat_type     text NOT NULL DEFAULT 'approver' CHECK (seat_type IN ('approver', 'contributor', 'reader')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_write_at timestamptz                       -- not written yet (seats count over 30 days: later phase)
);

CREATE TABLE IF NOT EXISTS teams (
  id         bigserial PRIMARY KEY,
  name       text NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS team_members (
  team_id    bigint NOT NULL REFERENCES teams(id),
  user_id    text   NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (team_id, user_id)
);

-- A single row.
CREATE TABLE IF NOT EXISTS org (
  org_id        text PRIMARY KEY,
  name          text NOT NULL,
  license_json  jsonb NOT NULL,
  license_sig   text,                             -- NULL: free license generated locally, unsigned
  last_sync_at  timestamptz,
  admin_user_id text NOT NULL REFERENCES users(id),
  grant_id      text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS org_single_row ON org ((true));

-- Configuration: append-only; the current value is the newest row per key.
CREATE TABLE IF NOT EXISTS org_config (
  id       bigserial PRIMARY KEY,
  key      text  NOT NULL,
  value    jsonb NOT NULL,
  set_by   text  NOT NULL,
  grant_id text  NOT NULL,
  ts       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS org_config_key_id_idx ON org_config (key, id DESC);

ALTER TABLE namespaces ADD COLUMN IF NOT EXISTS added_by text; -- NULL: seeded by the migration

CREATE OR REPLACE TRIGGER org_config_append_only BEFORE UPDATE OR DELETE ON org_config FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER org_config_no_truncate BEFORE TRUNCATE ON org_config FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER namespaces_append_only BEFORE UPDATE OR DELETE ON namespaces FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER namespaces_no_truncate BEFORE TRUNCATE ON namespaces FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();

GRANT SELECT, INSERT ON users, teams, team_members, org, org_config, namespaces TO tk_api;
GRANT USAGE ON teams_id_seq, org_config_id_seq TO tk_api;
