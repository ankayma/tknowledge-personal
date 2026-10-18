-- Thread: a matter the user remembers, called by keyword, pointing at things in the
-- store (user decided 1/10/2026).
--
-- A thread is not a claim: no T/A label, no gate. Four tables, all add-only:
--
--   thread          id TH-<n>: the row's own number in the store, unpadded, like T-<n>
--   thread_keyword  each time a (normalized) keyword is attached: where search looks
--   thread_link     each time a thread is linked to something in the store: reverse lookup
--   thread_history  opened · tagged · untagged · linked · unlinked · closed
--
-- Which keywords and links are in effect, and whether a thread is open or closed, are
-- all read from thread_history: the newest event for the same target decides. Removing
-- something adds an event row; nothing is deleted or edited.
--
-- The target kind is recognized by id prefix (T-, A-, D-, P., person:, conv_, TH-)
-- in code (internal/tka/thread.go). A link_kind config table was considered, but each
-- kind needs a "does this id exist" check, which is code, not data, so the list lives
-- in code: adding a new concept means adding one line there.

CREATE TABLE IF NOT EXISTS thread (
  tenant_id  text NOT NULL DEFAULT 'default',
  num        bigint NOT NULL,
  id         text GENERATED ALWAYS AS ('TH-' || num::text) STORED,
  title      text NOT NULL CHECK (btrim(title) <> ''),
  signer     text NOT NULL,
  producer   text NOT NULL,
  grant_id   text NOT NULL,
  chat_id    text,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, num),
  CONSTRAINT thread_tenant_id_key UNIQUE (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS thread_keyword (
  id        bigserial PRIMARY KEY,
  tenant_id text NOT NULL DEFAULT 'default',
  thread_id text NOT NULL,
  keyword   text NOT NULL CHECK (keyword <> ''),
  actor     text NOT NULL,
  at        timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, thread_id) REFERENCES thread (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS thread_link (
  id        bigserial PRIMARY KEY,
  tenant_id text NOT NULL DEFAULT 'default',
  thread_id text NOT NULL,
  target_id text NOT NULL,
  kind      text NOT NULL CHECK (kind IN ('truth', 'assumption', 'document', 'principle', 'person', 'conversation', 'thread')),
  actor     text NOT NULL,
  note      text,
  at        timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, thread_id) REFERENCES thread (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS thread_history (
  id        bigserial PRIMARY KEY,
  tenant_id text NOT NULL DEFAULT 'default',
  thread_id text NOT NULL,
  event     text NOT NULL CHECK (event IN ('opened', 'tagged', 'untagged', 'linked', 'unlinked', 'closed')),
  target    text,
  actor     text NOT NULL,
  note      text,
  at        timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, thread_id) REFERENCES thread (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS thread_keyword_tenant_idx ON thread_keyword (tenant_id, keyword);
CREATE INDEX IF NOT EXISTS thread_keyword_trgm ON thread_keyword USING gin (keyword gin_trgm_ops);
CREATE INDEX IF NOT EXISTS thread_link_thread_idx ON thread_link (tenant_id, thread_id);
CREATE INDEX IF NOT EXISTS thread_link_target_idx ON thread_link (tenant_id, target_id);
CREATE INDEX IF NOT EXISTS thread_history_thread_idx ON thread_history (tenant_id, thread_id, id);
CREATE INDEX IF NOT EXISTS thread_history_target_idx ON thread_history (tenant_id, target, id);

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['thread', 'thread_keyword', 'thread_link', 'thread_history'] LOOP
    EXECUTE format('CREATE OR REPLACE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION tk_append_only()', t || '_append_only', t);
    EXECUTE format('CREATE OR REPLACE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only()', t || '_no_truncate', t);
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    EXECUTE format($f$CREATE POLICY tenant_isolation ON %I
      USING (tenant_id = current_setting('app.tenant_id', true))
      WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$f$, t);
  END LOOP;
END $$;

-- TH is the fourth kind of the per-store counter (after T, A, D).
ALTER TABLE tenant_counter DROP CONSTRAINT IF EXISTS tenant_counter_kind_check;
-- NOT VALID: this file re-runs on every startup, when the counter already has kinds
-- added later (TH, F). Checking existing rows would make the re-run fail; the full list
-- is in 0020.
ALTER TABLE tenant_counter ADD CONSTRAINT tenant_counter_kind_check CHECK (kind IN ('T', 'A', 'D', 'TH')) NOT VALID;

GRANT SELECT, INSERT ON thread, thread_keyword, thread_link, thread_history TO tk_api;
GRANT USAGE ON thread_keyword_id_seq, thread_link_id_seq, thread_history_id_seq TO tk_api;
