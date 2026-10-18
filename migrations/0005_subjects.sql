-- Claims by subject.
-- No column rewrites old rows: ADD COLUMN with a DEFAULT on PG ≥ 11 does not rewrite
-- the table, so it produces no UPDATE on the change feed (CJCM sees no ledger_update).

-- Two moves close an A-b question (close: resolved | dropped). Invariant:
-- mark keeps A; only on an A-b that is still open.
ALTER TYPE move_t ADD VALUE IF NOT EXISTS 'resolve';
ALTER TYPE move_t ADD VALUE IF NOT EXISTS 'drop';

ALTER TABLE ledger
  ADD COLUMN IF NOT EXISTS subject text,                          -- <namespace>:<entity>.<attribute>, normalized
  ADD COLUMN IF NOT EXISTS owner   text,                          -- the person behind the content
  ADD COLUMN IF NOT EXISTS chat_id text,
  ADD COLUMN IF NOT EXISTS ab      boolean NOT NULL DEFAULT false, -- A-b label: waiting for the owner to clarify
  ADD COLUMN IF NOT EXISTS scope   text;                          -- user:<id> | team:<id> | org

CREATE INDEX IF NOT EXISTS ledger_subject_id_idx ON ledger (subject, id DESC) WHERE subject IS NOT NULL;

-- Closed namespace list. Adding a namespace = org.namespaces.add (phase D).
CREATE TABLE IF NOT EXISTS namespaces (
  name        text PRIMARY KEY,
  description text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);
INSERT INTO namespaces (name, description) VALUES
  ('tracker', 'dòng ứng tuyển'),
  ('person',  'người'),
  ('thread',  'thread tương tác'),
  ('post',    'bài của mình'),
  ('rule',    'quyết định về rule'),
  ('tk',      'T-Knowledge / MVP'),
  ('grant',   'grant / submission')
ON CONFLICT (name) DO NOTHING;

GRANT SELECT ON namespaces TO tk_api;
