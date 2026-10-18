-- A store for strangers to try: it has an expiry, a cap, and counts how far people get.
-- The cap and expiry live on the tenant itself, so a customer's real store is just a
-- tenant with no expiry: same mechanism, different numbers.

ALTER TABLE tenant
  ADD COLUMN IF NOT EXISTS expires_at  timestamptz,          -- NULL = no expiry
  ADD COLUMN IF NOT EXISTS max_entries integer,              -- NULL = no cap
  ADD COLUMN IF NOT EXISTS max_claim   integer,              -- length of one claim
  ADD COLUMN IF NOT EXISTS ref         text;                 -- where the trial user came from

-- Funnel: counts steps, keeps no claim content. What people try is their business.
CREATE TABLE IF NOT EXISTS demo_event (
  id        bigserial PRIMARY KEY,
  tenant_id text NOT NULL,
  kind      text NOT NULL CHECK (kind IN (
              'grant_created', 'first_write', 'reached_refusal',
              'reached_promote', 'reached_act2', 'returned')),
  ref       text,
  at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS demo_event_tenant_idx ON demo_event (tenant_id, id);
CREATE UNIQUE INDEX IF NOT EXISTS demo_event_once_idx ON demo_event (tenant_id, kind)
  WHERE kind <> 'returned';

GRANT SELECT, INSERT ON demo_event TO tk_api;
GRANT USAGE ON demo_event_id_seq TO tk_api;
GRANT UPDATE ON tenant TO tk_api;
