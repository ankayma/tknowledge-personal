-- Case in the Personal store (owner 17/10/2026, D-239 in the owner's store): a lesson
-- is saved as a document; the agent then suggests also creating a Case, which is saved
-- if the user agrees; otherwise the lesson stays only in documents. A Case is what
-- gets shared to Community, so its fields match Community's Case, plus from_doc, the
-- document it was copied from.
--
-- A Case is never evidence for a T: admit only accepts D-…. from_doc has no foreign
-- key to document (document's id column is rebuilt in 0017); AddCase checks that the
-- document exists in the store before writing.
-- Add-only. Kind and tags are case_tag rows, the newest row is current (filtering is
-- the main use; kinds are open, unlimited: owner 17/10). Withdrawing is a case_retired row.

CREATE TABLE IF NOT EXISTS case_entry (
  tenant_id   text NOT NULL DEFAULT 'default',
  num         bigint NOT NULL,
  id          text GENERATED ALWAYS AS ('C-' || num::text) STORED,
  title       text NOT NULL CHECK (btrim(title) <> ''),
  text        text NOT NULL CHECK (btrim(text) <> ''),
  agent       text,
  cost_usd    numeric CHECK (cost_usd IS NULL OR cost_usd >= 0),
  cost_hours  numeric CHECK (cost_hours IS NULL OR cost_hours >= 0),
  cost_note   text,
  who         text,
  what        text,
  happened_on date,
  "where"     text,
  why         text,
  how         text,
  from_doc    text NOT NULL,
  signer      text NOT NULL,
  producer    text NOT NULL,
  grant_id    text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, num),
  UNIQUE (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS case_tag (
  id           bigserial PRIMARY KEY,
  tenant_id    text NOT NULL DEFAULT 'default',
  case_id      text NOT NULL,
  kind         text,
  keywords     text[] NOT NULL DEFAULT '{}',
  topics       text[] NOT NULL DEFAULT '{}',
  case_studies text[] NOT NULL DEFAULT '{}',
  signer       text NOT NULL,
  producer     text NOT NULL,
  grant_id     text NOT NULL,
  at           timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, case_id) REFERENCES case_entry (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS case_tag_line_idx ON case_tag (tenant_id, case_id, id);

CREATE TABLE IF NOT EXISTS case_retired (
  tenant_id text NOT NULL DEFAULT 'default',
  case_id   text NOT NULL,
  reason    text NOT NULL CHECK (btrim(reason) <> ''),
  signer    text NOT NULL,
  producer  text NOT NULL,
  grant_id  text NOT NULL,
  at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, case_id),
  FOREIGN KEY (tenant_id, case_id) REFERENCES case_entry (tenant_id, id)
);

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['case_entry', 'case_tag', 'case_retired'] LOOP
    EXECUTE format('CREATE OR REPLACE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION tk_append_only()', t || '_append_only', t);
    EXECUTE format('CREATE OR REPLACE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only()', t || '_no_truncate', t);
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    EXECUTE format($f$CREATE POLICY tenant_isolation ON %I
      USING (tenant_id = current_setting('app.tenant_id', true))
      WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$f$, t);
  END LOOP;
END $$;
GRANT SELECT, INSERT ON case_entry, case_tag, case_retired TO tk_api;
GRANT USAGE ON SEQUENCE case_tag_id_seq TO tk_api;

-- The counter gains kind C. The full list, checked against existing rows too; 0013,
-- 0019, 0020 set narrower lists with NOT VALID so re-runs do not trip on existing C rows.
ALTER TABLE tenant_counter DROP CONSTRAINT IF EXISTS tenant_counter_kind_check;
ALTER TABLE tenant_counter ADD CONSTRAINT tenant_counter_kind_check CHECK (kind IN ('T', 'A', 'D', 'TH', 'F', 'C'));
