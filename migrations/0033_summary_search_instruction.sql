-- Two changes from the owner, 5/10/2026.
--
-- 1. lookup also searches summaries, but only current versions. thread_summary is
--    add-only, so "old versions leave the index" needs a separate table: one row per
--    thread, pointing at its current summary. A trigger replaces that row in the same
--    write as the summary, so the index never drifts from the table. The old trigram
--    index on thread_summary (0032) also covered superseded versions; dropped.
--
-- 2. Instruction: guidance on how to use the store, separate from principles and from
--    the ledger. Add and retire only; an I-<n> id is enough, no versions; scope is the
--    store; duplicate wording is detected as for claims, duplicate meaning is not
--    (owner decided 5/10/2026).
--
-- Re-runs on every startup: every statement is conditional.

DROP INDEX IF EXISTS thread_summary_text_trgm;

CREATE TABLE IF NOT EXISTS summary_search (
  tenant_id  text NOT NULL DEFAULT 'default',
  thread_id  text NOT NULL,
  summary_id text NOT NULL,
  text       text NOT NULL,
  PRIMARY KEY (tenant_id, thread_id),
  FOREIGN KEY (tenant_id, thread_id) REFERENCES thread (tenant_id, id),
  FOREIGN KEY (tenant_id, summary_id) REFERENCES thread_summary (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS summary_search_text_trgm ON summary_search USING gin (lower(text) gin_trgm_ops);
ALTER TABLE summary_search ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON summary_search;
CREATE POLICY tenant_isolation ON summary_search
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
GRANT SELECT, INSERT, UPDATE ON summary_search TO tk_api;

CREATE OR REPLACE FUNCTION tk_summary_search() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO summary_search (tenant_id, thread_id, summary_id, text)
  VALUES (NEW.tenant_id, NEW.thread_id, NEW.id, NEW.text)
  ON CONFLICT (tenant_id, thread_id) DO UPDATE SET summary_id = EXCLUDED.summary_id, text = EXCLUDED.text;
  RETURN NEW;
END $$;
CREATE OR REPLACE TRIGGER thread_summary_search AFTER INSERT ON thread_summary
  FOR EACH ROW EXECUTE FUNCTION tk_summary_search();

-- Summaries written before this table: insert the newest one per thread. A re-run only
-- rewrites what is already there.
INSERT INTO summary_search (tenant_id, thread_id, summary_id, text)
SELECT DISTINCT ON (tenant_id, thread_id) tenant_id, thread_id, id, text
  FROM thread_summary ORDER BY tenant_id, thread_id, num DESC
ON CONFLICT (tenant_id, thread_id) DO UPDATE SET summary_id = EXCLUDED.summary_id, text = EXCLUDED.text
  WHERE summary_search.summary_id IS DISTINCT FROM EXCLUDED.summary_id;

-- Instructions come in three kinds: meta (M1, M2: how to read and how to extend the
-- list), directive (1, 2 …: one thing the agent does; instruction_add creates this kind
-- by default and numbers it next), definition (def:<term>: one definition sentence, one
-- why sentence, and rationale_ref pointing into the docs; the full reasoning lives in
-- the docs, not in the store). label is the row's name in the list (M1, 12, def:claim);
-- grp is the group of a default row.
CREATE TABLE IF NOT EXISTS instruction (
  tenant_id     text NOT NULL DEFAULT 'default',
  num           bigint NOT NULL,
  id            text GENERATED ALWAYS AS ('I-' || num::text) STORED,
  kind          text NOT NULL CHECK (kind IN ('meta', 'directive', 'definition')),
  label         text NOT NULL CHECK (btrim(label) <> ''),
  text          text NOT NULL CHECK (btrim(text) <> ''),
  origin        text NOT NULL CHECK (origin IN ('default', 'added')),
  title         text,
  grp           text,
  term          text,
  why           text,
  rationale_ref text,
  supersedes    text,
  signer     text NOT NULL,
  producer   text NOT NULL,
  grant_id   text NOT NULL,
  chat_id    text,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, num),
  CONSTRAINT instruction_tenant_id_key UNIQUE (tenant_id, id),
  CONSTRAINT instruction_definition_check CHECK (
    (kind = 'definition') = (term IS NOT NULL AND why IS NOT NULL))
);
CREATE OR REPLACE TRIGGER instruction_append_only BEFORE UPDATE OR DELETE ON instruction
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER instruction_no_truncate BEFORE TRUNCATE ON instruction
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();
ALTER TABLE instruction ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON instruction;
CREATE POLICY tenant_isolation ON instruction
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
GRANT SELECT, INSERT ON instruction TO tk_api;

-- Retire is a separate row, also add-only: an instruction is retired once.
CREATE TABLE IF NOT EXISTS instruction_retired (
  tenant_id      text NOT NULL DEFAULT 'default',
  instruction_id text NOT NULL,
  reason         text NOT NULL CHECK (btrim(reason) <> ''),
  superseded_by  text,
  signer         text NOT NULL,
  producer       text NOT NULL,
  grant_id       text NOT NULL,
  at             timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, instruction_id),
  FOREIGN KEY (tenant_id, instruction_id) REFERENCES instruction (tenant_id, id)
);
CREATE OR REPLACE TRIGGER instruction_retired_append_only BEFORE UPDATE OR DELETE ON instruction_retired
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER instruction_retired_no_truncate BEFORE TRUNCATE ON instruction_retired
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();
ALTER TABLE instruction_retired ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON instruction_retired;
CREATE POLICY tenant_isolation ON instruction_retired
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
GRANT SELECT, INSERT ON instruction_retired TO tk_api;
