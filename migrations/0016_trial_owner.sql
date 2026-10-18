-- Who opened a trial store, so the link can be re-sent to them (owner decided 29/9).
-- No account, no password: one email maps to one store, and the resend button only
-- re-sends that store.
--
-- Email is personal data, so this table is NOT append-only like the ledger: people
-- must be able to remove themselves from it. In exchange, it is not evidence for any claim.

CREATE TABLE IF NOT EXISTS trial_owner (
  email      text PRIMARY KEY,
  tenant_id  text NOT NULL,
  ref        text,                      -- where they came from, if the link had ?ref=
  sent_count integer NOT NULL DEFAULT 0,
  last_sent  timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS trial_owner_tenant_idx ON trial_owner (tenant_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON trial_owner TO tk_api;
