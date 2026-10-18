-- T/A discipline (D-02, D-07).
-- Three classes, three stores, none of them self-referential:
--   principle     axioms, add-only, changed only by an owner amendment
--   truth_ledger  [T] passed the gate; content is immutable
--   assumption    [A] pending; the owner can edit it while it is open
-- `status` is not a column: it is computed at read time from entry_history.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Principle List. Not in the ledger because P has no basis (D-07 R2.1(d)),
-- but it must be in the system: a new user's new store is set up through MCP.
CREATE TABLE IF NOT EXISTS principle (
  code       text PRIMARY KEY,                 -- P.1 … P.n; the owner's numbering, gaps are not errors
  statement  text NOT NULL,                    -- canonical sentence [T]
  note       text,                             -- the [A] part, not used for derivation
  signer     text NOT NULL,                    -- the person, from the grant
  producer   text NOT NULL,                    -- the agent, from the grant
  grant_id   text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- [A] pending. The only table where content is edited, and only while open.
CREATE TABLE IF NOT EXISTS assumption (
  num           bigserial PRIMARY KEY,
  id            text GENERATED ALWAYS AS ('A-' || num::text) STORED UNIQUE,
  claim         text NOT NULL,
  claim_key     text NOT NULL,
  type          text NOT NULL CHECK (type IN ('A-r','A-c','A-p','A-b')),
  source_kind   text NOT NULL CHECK (source_kind IN ('owner','ai','hearsay','unverified','unknown')),
  source_detail text,
  if_obs        text,                          -- A-p: observation that raises it
  not_obs       text,                          -- A-p: observation that refutes it
  via           text,                          -- A-c: amendment · A-b: channel K that must be built
  supports      text[] NOT NULL DEFAULT '{}',
  revision      integer NOT NULL DEFAULT 1,
  signer        text NOT NULL,
  producer      text NOT NULL,
  grant_id      text NOT NULL,
  chat_id       text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT assumption_tail CHECK (
    (type = 'A-p' AND if_obs IS NOT NULL AND not_obs IS NOT NULL AND via IS NULL) OR
    (type IN ('A-c','A-b') AND via IS NOT NULL AND if_obs IS NULL AND not_obs IS NULL) OR
    (type = 'A-r' AND via IS NULL AND if_obs IS NULL AND not_obs IS NULL))
);

-- [T] passed the gate. Content never changes (D-07 R2).
CREATE TABLE IF NOT EXISTS truth_ledger (
  num           bigserial PRIMARY KEY,
  id            text GENERATED ALWAYS AS ('T-' || num::text) STORED UNIQUE,
  claim         text NOT NULL,
  claim_key     text NOT NULL,
  basis         text NOT NULL CHECK (basis IN ('fact','derivation')),
  source        text,                          -- fact: citable source
  source_date   date,                          -- fact: publication date, if the source has one
  verified_at   date NOT NULL,                 -- stamped by the machine when passing the gate
  valid_until   date,                          -- fact: expiry, if any
  premises      text[] NOT NULL DEFAULT '{}',  -- P.x and T-id
  derivation    text,                          -- the chain premises ⟹ conclusion
  supports      text[] NOT NULL DEFAULT '{}',
  promoted_from text,                          -- A-id
  supersedes    text,                          -- T-id, declared explicitly by the owner
  signer        text NOT NULL,
  producer      text NOT NULL,
  grant_id      text NOT NULL,
  chat_id       text,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT truth_basis CHECK (
    (basis = 'fact' AND source IS NOT NULL AND derivation IS NULL) OR
    (basis = 'derivation' AND derivation IS NOT NULL AND array_length(premises, 1) >= 1))
);

-- Every status change is a row here; there is no status column.
CREATE TABLE IF NOT EXISTS entry_history (
  id       bigserial PRIMARY KEY,
  entry_id text NOT NULL,                      -- A-…, T-… or P.x
  event    text NOT NULL CHECK (event IN (
             'created','updated','promoted','refuted','rechecked','contradicted_by',
             'demoted','retired','channel_built','spot_ok','superseded')),
  actor    text NOT NULL,
  note     text,
  at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS entry_history_entry_idx ON entry_history (entry_id, id DESC);

-- Groups two phrasings of the same claim without editing any entry.
CREATE TABLE IF NOT EXISTS entry_relation (
  id      bigserial PRIMARY KEY,
  from_id text NOT NULL,
  to_id   text NOT NULL,
  kind    text NOT NULL CHECK (kind IN ('restates')),
  actor   text NOT NULL,
  note    text,
  at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS assumption_claim_key_trgm ON assumption USING gin (claim_key gin_trgm_ops);
CREATE INDEX IF NOT EXISTS truth_claim_key_trgm ON truth_ledger USING gin (claim_key gin_trgm_ops);
CREATE INDEX IF NOT EXISTS truth_premises_idx ON truth_ledger USING gin (premises);

-- Append-only at the schema level, blocking even superuser: editing a [T] or an axiom
-- is not a permissions question, it is a question of whether this store means anything.
CREATE OR REPLACE FUNCTION tk_no_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on % is not allowed: rows are never removed', TG_OP, TG_TABLE_NAME
    USING ERRCODE = 'insufficient_privilege';
END $$;

CREATE OR REPLACE TRIGGER truth_ledger_append_only BEFORE UPDATE OR DELETE ON truth_ledger
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER truth_ledger_no_truncate BEFORE TRUNCATE ON truth_ledger
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER principle_append_only BEFORE UPDATE OR DELETE ON principle
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER entry_history_append_only BEFORE UPDATE OR DELETE ON entry_history
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER entry_relation_append_only BEFORE UPDATE OR DELETE ON entry_relation
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
-- assumption: editable while open (update_assumption), but never deleted.
CREATE OR REPLACE TRIGGER assumption_no_delete BEFORE DELETE ON assumption
  FOR EACH ROW EXECUTE FUNCTION tk_no_delete();
CREATE OR REPLACE TRIGGER assumption_no_truncate BEFORE TRUNCATE ON assumption
  FOR EACH STATEMENT EXECUTE FUNCTION tk_no_delete();

GRANT SELECT, INSERT ON principle, truth_ledger, entry_history, entry_relation TO tk_api;
GRANT SELECT, INSERT, UPDATE ON assumption TO tk_api;
GRANT USAGE ON assumption_num_seq, truth_ledger_num_seq, entry_history_id_seq, entry_relation_id_seq TO tk_api;
