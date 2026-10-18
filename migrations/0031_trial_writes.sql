-- Trial stores live forever, limited by write count (owner decided 3/10/2026): 1000
-- successful writes, then read-only. A calendar expiry would close the store while the
-- user is still using it, and signing back into a closed store silently gives a new
-- empty one.
--
-- max_writes NULL = no cap (paid stores, single-store installs). The counter has its
-- own table, not tenant_counter: that table's kind constraint is rebuilt on every
-- startup (0013/0019/0020), and adding another kind adds another place to trip.

ALTER TABLE tenant ADD COLUMN IF NOT EXISTS max_writes integer;

CREATE TABLE IF NOT EXISTS tenant_writes (
  tenant_id text PRIMARY KEY,
  n         bigint NOT NULL DEFAULT 0
);
GRANT SELECT, INSERT, UPDATE ON tenant_writes TO tk_api;

-- One-time move of existing demo-… trial stores to the new terms, counting from 0. A
-- re-run does nothing: rows that already have max_writes do not match. Old try-… stores
-- (already closed) and paid stores stay as they are.
UPDATE tenant
   SET expires_at = NULL, max_entries = NULL, max_writes = 1000
 WHERE tier = 'trial' AND id LIKE 'demo-%' AND max_writes IS NULL
   AND (expires_at IS NULL OR expires_at > now())  -- a store that already closed stays closed
   -- From 0034 the cap lives on each person's seat, not on the store: once the member
   -- table exists this step has nothing left to do.
   AND to_regclass('member') IS NULL;
