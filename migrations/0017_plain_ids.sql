-- An id is the row's own number, unpadded (user decided 1/10/2026): T-1, T-2 … T-10000;
-- T-100 is row num = 100 of truth_ledger in that store. Same for A- and D-.
--
-- The old 'T-' || lpad(num, 4, '0') broke at row 10000: lpad truncates strings longer
-- than 4, so num 10000 became "T-1000" and collided on unique with the real T-1000;
-- every later write failed.
--
-- 0010/0013 already create the column in the new form for stores built from scratch.
-- This file converts existing stores, and only runs while the id column is still in
-- lpad form, so a re-run on startup does nothing. Stored references ("T-0007" in
-- premises, history, relation…) are converted too, so every id in the store has
-- exactly one spelling.

DO $$
DECLARE
  old_shape boolean;
BEGIN
  SELECT pg_get_expr(d.adbin, d.adrelid) LIKE '%lpad%' INTO old_shape
  FROM pg_attrdef d
  JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
  WHERE d.adrelid = 'truth_ledger'::regclass AND a.attname = 'id';
  IF NOT coalesce(old_shape, false) THEN
    RETURN;
  END IF;

  -- "T-0007" → "T-7", only where it stands as a whole word.
  CREATE FUNCTION pg_temp.plain(s text) RETURNS text LANGUAGE sql IMMUTABLE AS
    $f$ SELECT regexp_replace(s, '\m([TAD])-0+([1-9][0-9]*)\M', '\1-\2', 'g') $f$;
  CREATE FUNCTION pg_temp.plain(a text[]) RETURNS text[] LANGUAGE sql IMMUTABLE AS
    $f$ SELECT coalesce(array_agg(pg_temp.plain(x) ORDER BY i), '{}')
        FROM unnest(a) WITH ORDINALITY AS u(x, i) $f$;

  -- These tables block UPDATE with an append-only trigger; this changes how ids are
  -- written, not the content, so the triggers are disabled for the change and re-enabled
  -- right after.
  ALTER TABLE truth_ledger   DISABLE TRIGGER USER;
  ALTER TABLE entry_history  DISABLE TRIGGER USER;
  ALTER TABLE entry_relation DISABLE TRIGGER USER;

  UPDATE truth_ledger SET
    premises      = pg_temp.plain(premises),
    supports      = pg_temp.plain(supports),
    promoted_from = pg_temp.plain(promoted_from),
    supersedes    = pg_temp.plain(supersedes),
    source_entry  = pg_temp.plain(source_entry),
    source_doc    = pg_temp.plain(source_doc);
  UPDATE assumption     SET supports = pg_temp.plain(supports);
  UPDATE entry_history  SET entry_id = pg_temp.plain(entry_id), note = pg_temp.plain(note);
  UPDATE entry_relation SET from_id = pg_temp.plain(from_id), to_id = pg_temp.plain(to_id),
                            note = pg_temp.plain(note);

  -- PG16 has no ALTER COLUMN … SET EXPRESSION: drop the generated column and add it back.
  -- Dropping the column drops the unique index/constraint on (tenant_id, id) with it;
  -- rebuild them under the old names.
  ALTER TABLE truth_ledger DROP COLUMN id;
  ALTER TABLE truth_ledger ADD COLUMN id text GENERATED ALWAYS AS ('T-' || num::text) STORED;
  CREATE UNIQUE INDEX truth_ledger_tenant_id_idx ON truth_ledger (tenant_id, id);

  ALTER TABLE assumption DROP COLUMN id;
  ALTER TABLE assumption ADD COLUMN id text GENERATED ALWAYS AS ('A-' || num::text) STORED;
  CREATE UNIQUE INDEX assumption_tenant_id_idx ON assumption (tenant_id, id);

  ALTER TABLE document DROP COLUMN id;
  ALTER TABLE document ADD COLUMN id text GENERATED ALWAYS AS ('D-' || num::text) STORED;
  ALTER TABLE document ADD CONSTRAINT document_tenant_id_id_key UNIQUE (tenant_id, id);

  ALTER TABLE truth_ledger   ENABLE TRIGGER USER;
  ALTER TABLE entry_history  ENABLE TRIGGER USER;
  ALTER TABLE entry_relation ENABLE TRIGGER USER;
END $$;
