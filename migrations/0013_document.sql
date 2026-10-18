-- Retrievable evidence (owner decided 28/9): a fact must point to something other
-- people can open and look at: a public link, or a document whose bytes this store keeps.
-- "The owner said so" is not a fact. No link/doc means refusal; there is no
-- mark-it-and-trust-it door.
--
-- The store keeps bytes, not just a hash: a hash proves the file has not changed, but
-- with only a hash the evidence disappears with whatever machine deleted the file.

CREATE TABLE IF NOT EXISTS document (
  tenant_id  text NOT NULL DEFAULT 'default',
  num        bigint NOT NULL,                  -- per-store counter
  id         text GENERATED ALWAYS AS ('D-' || num::text) STORED,
  sha256     text NOT NULL,                    -- hex, 64 characters
  media_type text NOT NULL,
  filename   text,
  size       integer NOT NULL CHECK (size > 0),
  bytes      bytea NOT NULL,
  signer     text NOT NULL,                    -- the person, from the grant
  producer   text NOT NULL,                    -- the agent, from the grant
  grant_id   text NOT NULL,
  added_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, num),
  UNIQUE (tenant_id, id)
);

-- Same bytes, same document: re-uploading returns the existing one, no second row.
CREATE UNIQUE INDEX IF NOT EXISTS document_sha_idx ON document (tenant_id, sha256);

-- Documents are evidence: if they could be edited, claims resting on them would mean nothing.
CREATE OR REPLACE TRIGGER document_append_only BEFORE UPDATE OR DELETE ON document
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER document_no_truncate BEFORE TRUNCATE ON document
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();

ALTER TABLE document ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON document;
CREATE POLICY tenant_isolation ON document
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Documents are numbered per store, like T and A.
ALTER TABLE tenant_counter DROP CONSTRAINT IF EXISTS tenant_counter_kind_check;
-- NOT VALID: this file re-runs on every startup, when the counter already has kinds
-- added later (TH, F). Checking existing rows would make the re-run fail; the full list
-- is in 0020.
ALTER TABLE tenant_counter ADD CONSTRAINT tenant_counter_kind_check CHECK (kind IN ('T', 'A', 'D')) NOT VALID;

-- A fact points to evidence through one of these two columns; the gate requires at least one.
ALTER TABLE truth_ledger
  ADD COLUMN IF NOT EXISTS source_url text,
  ADD COLUMN IF NOT EXISTS source_doc text;

GRANT SELECT, INSERT ON document TO tk_api;
