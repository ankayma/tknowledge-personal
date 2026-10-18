-- Two changes the owner approved 3/10/2026.
--
-- 1. Summary on a thread: a readable paragraph saying where the matter stands. It is
--    A, never T: it has an author and a signer, every sentence cites ids in the
--    thread, and it is updated by a new version (the old one reads as superseded),
--    never edited in place. "stale" is computed at read time, not stored. S-<n> is
--    numbered per store, issued under the store's thread lock (the lock every thread
--    write takes), so no new kind is needed in tenant_counter, whose kind constraint
--    is rebuilt on every startup in 0013/0019/0020.
--
-- 2. A word cap per claim (100 for trial stores); max_claim stays as the backup cap
--    (600 graphemes). Open trial stores are set once; paid stores and closed stores
--    are not touched.
--
-- Re-runs on every startup: every statement is conditional.

CREATE TABLE IF NOT EXISTS thread_summary (
  tenant_id  text NOT NULL DEFAULT 'default',
  num        bigint NOT NULL,
  id         text GENERATED ALWAYS AS ('S-' || num::text) STORED,
  thread_id  text NOT NULL,
  text       text NOT NULL CHECK (btrim(text) <> ''),
  author     text NOT NULL CHECK (author IN ('owner', 'claude')),
  signer     text NOT NULL,
  producer   text NOT NULL,
  grant_id   text NOT NULL,
  chat_id    text,
  as_of      text,                              -- newest thread entry the summary has accounted for
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, num),
  CONSTRAINT thread_summary_tenant_id_key UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, thread_id) REFERENCES thread (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS thread_summary_thread_idx ON thread_summary (tenant_id, thread_id, num DESC);
-- No text index on this table: lookup finds summaries through summary_search (0033),
-- which holds only current versions.

CREATE OR REPLACE TRIGGER thread_summary_append_only BEFORE UPDATE OR DELETE ON thread_summary
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER thread_summary_no_truncate BEFORE TRUNCATE ON thread_summary
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();
ALTER TABLE thread_summary ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON thread_summary;
CREATE POLICY tenant_isolation ON thread_summary
  USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
GRANT SELECT, INSERT ON thread_summary TO tk_api;

-- Writing a summary leaves a row in the thread history. 0019 creates the table with the
-- CHECK inside CREATE TABLE IF NOT EXISTS, so re-running 0019 does not rebuild the
-- CHECK; relaxing it here once is enough.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'thread_history'::regclass AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%summarized%') THEN
    ALTER TABLE thread_history DROP CONSTRAINT IF EXISTS thread_history_event_check;
    ALTER TABLE thread_history ADD CONSTRAINT thread_history_event_check
      CHECK (event IN ('opened', 'tagged', 'untagged', 'linked', 'unlinked', 'closed', 'summarized'));
  END IF;
END $$;

ALTER TABLE tenant ADD COLUMN IF NOT EXISTS max_words integer;  -- NULL = no word cap

UPDATE tenant
   SET max_words = 100, max_claim = 600
 WHERE tier = 'trial' AND id LIKE 'demo-%' AND max_words IS NULL
   AND (expires_at IS NULL OR expires_at > now());
