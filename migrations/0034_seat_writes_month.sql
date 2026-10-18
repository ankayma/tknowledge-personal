-- Seats and monthly write limits, counted per person (owner decided 13/10/2026).
--
-- A tenant is an organization; a member is one person in it, one seat each: free is
-- 100 writes a month, standard 1000, unlimited is not counted. A month is a calendar
-- month in TK_WRITE_TZ (default Asia/Dubai); `period` is the first day of that month,
-- computed in Go at write time. The old per-store cap (tenant.max_writes,
-- tenant_writes) stays for self-hosted installs; demo-… trial stores no longer use it.

CREATE TABLE IF NOT EXISTS member (
  tenant_id text NOT NULL,
  user_id   text NOT NULL,
  seat      text NOT NULL DEFAULT 'free' CHECK (seat IN ('free', 'standard', 'unlimited')),
  joined_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id)
);

-- Who changed whose seat, when, and why. Add-only: overwriting would lose track of who
-- gave whom unlimited writes.
CREATE TABLE IF NOT EXISTS member_seat_history (
  id        bigserial PRIMARY KEY,
  tenant_id text NOT NULL,
  user_id   text NOT NULL,
  seat_from text,
  seat_to   text NOT NULL CHECK (seat_to IN ('free', 'standard', 'unlimited')),
  by        text NOT NULL,
  reason    text,
  at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS member_seat_history_member_idx ON member_seat_history (tenant_id, user_id, id);

CREATE OR REPLACE TRIGGER member_seat_history_append_only BEFORE UPDATE OR DELETE ON member_seat_history
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER member_seat_history_no_truncate BEFORE TRUNCATE ON member_seat_history
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();

-- One row per person per month. The upsert locks the row, so two concurrent writes by
-- one person queue up instead of both going over the cap.
CREATE TABLE IF NOT EXISTS member_writes (
  tenant_id text NOT NULL,
  user_id   text NOT NULL,
  period    date NOT NULL,
  n         bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, user_id, period)
);

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['member', 'member_seat_history', 'member_writes'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    EXECUTE format($f$CREATE POLICY tenant_isolation ON %I
      USING (tenant_id = current_setting('app.tenant_id', true))
      WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$f$, t);
  END LOOP;
END $$;

GRANT SELECT, INSERT, UPDATE ON member, member_writes TO tk_api;
GRANT SELECT, INSERT ON member_seat_history TO tk_api;
GRANT USAGE ON member_seat_history_id_seq TO tk_api;

-- One-time move: whoever opened a trial store becomes a free member of it, with a
-- history row. A re-run does nothing: people who are already members are skipped.
WITH added AS (
  INSERT INTO member (tenant_id, user_id, seat)
  SELECT o.tenant_id, o.email, 'free' FROM trial_owner o
  ON CONFLICT (tenant_id, user_id) DO NOTHING
  RETURNING tenant_id, user_id
)
INSERT INTO member_seat_history (tenant_id, user_id, seat_from, seat_to, by, reason)
SELECT tenant_id, user_id, NULL, 'free', 'migration-0034', 'trial store moved to 100 writes a month'
FROM added;

-- Trial stores stop counting totals: the cap is on the seat. The old tenant_writes
-- counter stays as history. Paid stores and self-hosted installs do not change.
UPDATE tenant SET max_writes = NULL
 WHERE tier = 'trial' AND id LIKE 'demo-%' AND max_writes IS NOT NULL;
