-- Document and the T–D relation, stage 1 (user approved 2/10/2026).
--
-- 1. tk is a knowledge store, not a file store. A document is readable text plus a
--    reference to the original, which lives somewhere else. The bytes column remains
--    only for documents loaded before this date; new documents have no bytes.
-- 2. T–D is many-to-many through the evidence table. Rows are never edited;
--    withdrawing is an event in evidence_history.
-- 3. Flags of the T/A store (B6): a separate table, not the old ledger's `flags`,
--    because flags here point at T-…/D-…, not at the old ledger's claim_id.
--
-- The file re-runs on every startup: every statement is conditional, and the two copies
-- of old data only touch rows not yet copied.

-- ---------- document ----------

ALTER TABLE document
  ADD COLUMN IF NOT EXISTS text           text,
  ADD COLUMN IF NOT EXISTS text_producer  text,
  ADD COLUMN IF NOT EXISTS method         text,
  ADD COLUMN IF NOT EXISTS locator        text,
  ADD COLUMN IF NOT EXISTS source_uri     text,
  ADD COLUMN IF NOT EXISTS content_sha256 text,
  ADD COLUMN IF NOT EXISTS custodian      text,
  ADD COLUMN IF NOT EXISTS captured_at    timestamptz;

-- sha256, size, media_type now describe the original, and are optional. The same sha256
-- is still one document (document_sha_idx, 0013): NULLs do not collide in a unique index.
ALTER TABLE document ALTER COLUMN bytes      DROP NOT NULL;
ALTER TABLE document ALTER COLUMN sha256     DROP NOT NULL;
ALTER TABLE document ALTER COLUMN media_type DROP NOT NULL;
ALTER TABLE document ALTER COLUMN size       DROP NOT NULL;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'document_text_producer_check') THEN
    ALTER TABLE document ADD CONSTRAINT document_text_producer_check
      CHECK (text_producer IN ('owner', 'claude', 'extract'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'document_method_check') THEN
    ALTER TABLE document ADD CONSTRAINT document_method_check
      CHECK (method IN ('opened_source', 'rehashed', 'read_text'));
  END IF;
END $$;

-- Text documents loaded before this date: their bytes are the text ("when the schema
-- changes, nothing needs rewriting"). Copied once; the author of the text is the owner,
-- and method is read_text because nobody declared opening the original. Non-text
-- documents (PDF, images) keep text NULL and read as legacy. Rows whose bytes are not
-- valid UTF-8 are skipped instead of breaking the whole startup.
DO $$
DECLARE d record;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM document
                 WHERE text IS NULL AND bytes IS NOT NULL AND media_type LIKE 'text/%') THEN
    RETURN;
  END IF;
  ALTER TABLE document DISABLE TRIGGER USER;
  FOR d IN SELECT tenant_id, num FROM document
           WHERE text IS NULL AND bytes IS NOT NULL AND media_type LIKE 'text/%' LOOP
    BEGIN
      UPDATE document
         SET text = convert_from(bytes, 'UTF8'), text_producer = 'owner', method = 'read_text'
       WHERE tenant_id = d.tenant_id AND num = d.num;
    EXCEPTION WHEN character_not_in_repertoire OR untranslatable_character THEN
      NULL;
    END;
  END LOOP;
  ALTER TABLE document ENABLE TRIGGER USER;
END $$;

CREATE INDEX IF NOT EXISTS document_text_trgm ON document USING gin (lower(text) gin_trgm_ops);
CREATE INDEX IF NOT EXISTS document_source_uri_idx ON document (tenant_id, source_uri);

-- ---------- evidence ----------

-- No foreign key to truth_ledger(id) or document(id): 0017 drops and rebuilds the id
-- column when converting an old store, and a foreign key would block that. The machine
-- checks that both ends exist at write time.
CREATE TABLE IF NOT EXISTS evidence (
  id        bigserial PRIMARY KEY,
  tenant_id text NOT NULL DEFAULT 'default',
  truth     text NOT NULL,
  document  text NOT NULL,
  signer    text NOT NULL,                     -- the person who attached it, from the grant; same as the document's signer
  producer  text NOT NULL,
  grant_id  text NOT NULL,
  kind      text NOT NULL CHECK (kind IN ('attest', 'dispute')),
  "where"   text,                              -- where to look in the document: "page 2", "row Q3"
  note      text,
  added_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT evidence_tenant_truth_document_key UNIQUE (tenant_id, truth, document)
);
CREATE INDEX IF NOT EXISTS evidence_document_idx ON evidence (tenant_id, document);

CREATE TABLE IF NOT EXISTS evidence_history (
  id        bigserial PRIMARY KEY,
  tenant_id text NOT NULL DEFAULT 'default',
  truth     text NOT NULL,
  document  text NOT NULL,
  event     text NOT NULL CHECK (event IN ('attached', 'withdrawn')),
  actor     text NOT NULL,
  note      text,
  at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS evidence_history_pair_idx ON evidence_history (tenant_id, truth, document, id);

-- Every T that already had a source_doc gets it as its first attest row: the signer
-- is the person who signed the T, where is the T's source line. Copied once; existing
-- rows are skipped.
WITH added AS (
  INSERT INTO evidence (tenant_id, truth, document, signer, producer, grant_id, kind, "where", added_at)
  SELECT tenant_id, id, source_doc, signer, producer, grant_id, 'attest', source, created_at
  FROM truth_ledger WHERE source_doc IS NOT NULL
  ON CONFLICT (tenant_id, truth, document) DO NOTHING
  RETURNING tenant_id, truth, document, signer, added_at
)
INSERT INTO evidence_history (tenant_id, truth, document, event, actor, note, at)
SELECT tenant_id, truth, document, 'attached', signer, 'from source_doc', added_at FROM added;

-- ---------- flags of the T/A store ----------

CREATE TABLE IF NOT EXISTS tka_flag (
  tenant_id  text NOT NULL DEFAULT 'default',
  num        bigint NOT NULL,
  id         text GENERATED ALWAYS AS ('F-' || num::text) STORED,
  kind       text NOT NULL CHECK (kind IN ('evidence_withdrawn', 'page_changed', 'read_differently')),
  subject    text NOT NULL,                    -- the T-… or D-… the flag is raised on
  detail     text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, num),
  CONSTRAINT tka_flag_tenant_id_key UNIQUE (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS tka_flag_subject_idx ON tka_flag (tenant_id, subject);

CREATE TABLE IF NOT EXISTS tka_flag_history (
  id        bigserial PRIMARY KEY,
  tenant_id text NOT NULL DEFAULT 'default',
  flag      text NOT NULL,
  event     text NOT NULL CHECK (event IN ('opened', 'closed')),
  actor     text NOT NULL,
  note      text,
  at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS tka_flag_history_flag_idx ON tka_flag_history (tenant_id, flag, id);

-- ---------- append-only, row-level security, grants ----------

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['evidence', 'evidence_history', 'tka_flag', 'tka_flag_history'] LOOP
    EXECUTE format('CREATE OR REPLACE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION tk_append_only()', t || '_append_only', t);
    EXECUTE format('CREATE OR REPLACE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only()', t || '_no_truncate', t);
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    EXECUTE format($f$CREATE POLICY tenant_isolation ON %I
      USING (tenant_id = current_setting('app.tenant_id', true))
      WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$f$, t);
  END LOOP;
END $$;

-- The counter gains kind F (flag). NOT VALID like 0013 and 0019: this file re-runs on
-- every startup, when the counter already has kinds added later (C, 0039). The full
-- list, checked against existing rows too, is in 0039.
ALTER TABLE tenant_counter DROP CONSTRAINT IF EXISTS tenant_counter_kind_check;
ALTER TABLE tenant_counter ADD CONSTRAINT tenant_counter_kind_check CHECK (kind IN ('T', 'A', 'D', 'TH', 'F')) NOT VALID;

GRANT SELECT, INSERT ON evidence, evidence_history, tka_flag, tka_flag_history TO tk_api;
GRANT USAGE ON evidence_id_seq, evidence_history_id_seq, tka_flag_history_id_seq TO tk_api;
