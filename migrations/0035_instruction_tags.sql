-- 5W1H tags for instructions (owner 13/10/2026, document D-116 in the owner's store):
-- help(situation) returns the rows that fit the situation instead of the whole list.
--
-- Tags are server-side metadata next to the instruction row; the instruction text is
-- never edited. Add-only: retagging adds a new row, and the newest row per instruction
-- is its current tag. Default rows whose text is unchanged use the tags of the default
-- list in code and need no row here.
--
-- who/what/when/where come from a closed vocabulary (tka.TagVocabulary, checked in Go);
-- when may carry a condition "before-write:work-status". why/how are free text.

CREATE TABLE IF NOT EXISTS instruction_tag (
  id             bigserial PRIMARY KEY,
  tenant_id      text NOT NULL DEFAULT 'default',
  instruction_id text NOT NULL,
  tag_who        text[] NOT NULL DEFAULT '{}',
  tag_what       text[] NOT NULL DEFAULT '{}',
  tag_when       text[] NOT NULL DEFAULT '{}',
  tag_where      text[] NOT NULL DEFAULT '{}',
  tag_why        text,
  tag_how        text,
  signer         text NOT NULL,
  producer       text NOT NULL,
  grant_id       text NOT NULL,
  at             timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, instruction_id) REFERENCES instruction (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS instruction_tag_line_idx ON instruction_tag (tenant_id, instruction_id, id);

CREATE OR REPLACE TRIGGER instruction_tag_append_only BEFORE UPDATE OR DELETE ON instruction_tag
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER instruction_tag_no_truncate BEFORE TRUNCATE ON instruction_tag
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();
ALTER TABLE instruction_tag ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON instruction_tag;
CREATE POLICY tenant_isolation ON instruction_tag
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
GRANT SELECT, INSERT ON instruction_tag TO tk_api;
GRANT USAGE ON SEQUENCE instruction_tag_id_seq TO tk_api;
