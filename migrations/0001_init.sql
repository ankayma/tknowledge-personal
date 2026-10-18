-- t-knowledge-system: schema + roles for the `tk` database.
-- Idempotent (safe to re-run, even when the roles already exist from deploy/initdb).

DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tk_api') THEN
    CREATE ROLE tk_api LOGIN PASSWORD 'dev';
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tk_checker') THEN
    CREATE ROLE tk_checker LOGIN PASSWORD 'dev';
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tk_cdc') THEN
    CREATE ROLE tk_cdc LOGIN REPLICATION PASSWORD 'dev';
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_type WHERE typname = 'move_t') THEN
    CREATE TYPE move_t AS ENUM ('propose','import','promote','demote','refute','reapprove','reject','refer');
  END IF;
END $$;

CREATE TABLE IF NOT EXISTS ledger (
  id          bigserial PRIMARY KEY,
  claim_id    bigint NOT NULL,            -- id of the claim's first row; on the first row: claim_id = id
  statement   text   NOT NULL,
  mark        char(1) NOT NULL CHECK (mark IN ('T','A')),
  basis       jsonb,                      -- {kind, refs:[claim_id], note, valid_until?}
  signer      text,                       -- NULL only when move = 'propose'
  producer    text   NOT NULL,            -- the person or agent that produced the content
  move        move_t NOT NULL,
  supersedes  bigint REFERENCES ledger(id),
  cause_id    text,                       -- NULL only when move = 'propose'
  reason      text,
  ts          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ledger_claim_id_id_idx ON ledger (claim_id, id DESC);

CREATE TABLE IF NOT EXISTS flags (
  id         bigserial PRIMARY KEY,
  claim_id   bigint NOT NULL,
  kind       text   NOT NULL,   -- unanchored | premise_demoted | expired_basis | unsigned | self_signed_agent | broken_chain | unjustified_row | previously_refuted
  evidence   jsonb  NOT NULL,   -- {failing_id, reason, ...}
  to_signer  text,              -- signer of the claim
  ts         timestamptz NOT NULL DEFAULT now(),
  closed_by  bigint REFERENCES ledger(id)
);

DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_publication WHERE pubname = 'tk_ledger_pub') THEN
    CREATE PUBLICATION tk_ledger_pub FOR TABLE ledger;
  END IF;
END $$;

GRANT INSERT, SELECT ON ledger TO tk_api;
GRANT SELECT ON flags TO tk_api;
GRANT USAGE ON ledger_id_seq TO tk_api;

GRANT SELECT ON ledger TO tk_checker;
GRANT INSERT, UPDATE ON flags TO tk_checker;
GRANT USAGE ON flags_id_seq TO tk_checker;

-- No role has UPDATE/DELETE on ledger. Test T-06 checks this by trying and expecting an error.
