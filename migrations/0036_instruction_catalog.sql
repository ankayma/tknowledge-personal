-- Instruction catalog (owner approved 14/10/2026).
--
-- The catalog is the `catalog` tenant: each entry is an instruction row of that
-- tenant, public id BP-<num>, written in English. A user's store that takes an entry
-- gets a row with catalog_ref = BP-<n>, the English text verbatim, and the agent's
-- translation in separate columns (translation, lang, translated_by). The table stays
-- add-only.

ALTER TABLE instruction ADD COLUMN IF NOT EXISTS catalog_ref   text;
ALTER TABLE instruction ADD COLUMN IF NOT EXISTS translation   text;
ALTER TABLE instruction ADD COLUMN IF NOT EXISTS lang          text;
ALTER TABLE instruction ADD COLUMN IF NOT EXISTS translated_by text;
-- Catalog tenant only: whether the entry is a default for new stores, the real
-- (anonymized) incident that explains why it exists, and a note for the reviewer
-- (e.g. too private).
ALTER TABLE instruction ADD COLUMN IF NOT EXISTS starter       boolean NOT NULL DEFAULT false;
ALTER TABLE instruction ADD COLUMN IF NOT EXISTS evidence      text;
ALTER TABLE instruction ADD COLUMN IF NOT EXISTS review_note   text;

-- 'catalog': a row the user took from the catalog (unlike 'default', the base rows
-- created with the store, and 'added', rows the user wrote).
ALTER TABLE instruction DROP CONSTRAINT IF EXISTS instruction_origin_check;
ALTER TABLE instruction ADD CONSTRAINT instruction_origin_check CHECK (origin IN ('default', 'added', 'catalog'));
ALTER TABLE instruction DROP CONSTRAINT IF EXISTS instruction_translation_check;
ALTER TABLE instruction ADD CONSTRAINT instruction_translation_check CHECK (
  translation IS NULL OR (lang IS NOT NULL AND btrim(lang) <> '' AND translated_by IS NOT NULL AND btrim(translated_by) <> ''));
ALTER TABLE instruction DROP CONSTRAINT IF EXISTS instruction_catalog_ref_check;
ALTER TABLE instruction ADD CONSTRAINT instruction_catalog_ref_check CHECK (
  catalog_ref IS NULL OR (tenant_id <> 'catalog' AND catalog_ref ~ '^BP-[1-9][0-9]*$'));
ALTER TABLE instruction DROP CONSTRAINT IF EXISTS instruction_catalog_only_check;
ALTER TABLE instruction ADD CONSTRAINT instruction_catalog_only_check CHECK (
  tenant_id = 'catalog' OR (NOT starter AND evidence IS NULL AND review_note IS NULL));

-- Reviewing a catalog entry: approve or reject, add-only. An entry with no row here is
-- "proposed": no user sees it yet. On re-review the newest row wins.
CREATE TABLE IF NOT EXISTS catalog_review (
  id             bigserial PRIMARY KEY,
  tenant_id      text NOT NULL DEFAULT 'catalog' CHECK (tenant_id = 'catalog'),
  instruction_id text NOT NULL,
  decision       text NOT NULL CHECK (decision IN ('approve', 'reject')),
  note           text,
  signer         text NOT NULL,
  producer       text NOT NULL,
  grant_id       text NOT NULL,
  at             timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, instruction_id) REFERENCES instruction (tenant_id, id)
);
CREATE OR REPLACE TRIGGER catalog_review_append_only BEFORE UPDATE OR DELETE ON catalog_review
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER catalog_review_no_truncate BEFORE TRUNCATE ON catalog_review
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();
ALTER TABLE catalog_review ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON catalog_review;
CREATE POLICY tenant_isolation ON catalog_review
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
GRANT SELECT, INSERT ON catalog_review TO tk_api;
GRANT USAGE ON SEQUENCE catalog_review_id_seq TO tk_api;

INSERT INTO tenant (id, name, tier) VALUES ('catalog', 'T-knowledge instruction catalog', 'catalog') ON CONFLICT (id) DO NOTHING;
