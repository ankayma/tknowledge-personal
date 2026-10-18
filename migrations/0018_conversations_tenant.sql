-- Conversation is the new name for the old thread, and people/conversations/events
-- belong to a store (user decided 1/10/2026).
--
-- 1. Rename. Stores built before this date have a `threads` table and an
--    events.thread_id column; rename them to `conversations` and
--    events.conversation_id. Only renames when the old name exists and the new one
--    does not, so a re-run does nothing. 0007 does not rebuild `threads` once
--    `conversations` exists (see the note there).
--    The kind value 'post_thread' stays: it is the comment chain under a post, not
--    a meta-knowledge thread, and changing it would change data already written.
--
-- 2. tenant_id + RLS, as 0011 did for the T/A store: strangers' trial stores used
--    to share these three tables. Unique keys (person slug, post url, kind + person)
--    are now scoped to one store.
--
-- Adding a column with a default rewrites no rows, so the append-only trigger is
-- not touched.

DO $$
BEGIN
  IF to_regclass('threads') IS NOT NULL AND to_regclass('conversations') IS NULL THEN
    ALTER TABLE threads RENAME TO conversations;
  END IF;
  IF to_regclass('threads_id_seq') IS NOT NULL AND to_regclass('conversations_id_seq') IS NULL THEN
    ALTER SEQUENCE threads_id_seq RENAME TO conversations_id_seq;
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_name = 'conversations' AND column_name = 'owner_of_thread') THEN
    ALTER TABLE conversations RENAME COLUMN owner_of_thread TO owner_of_conversation;
  END IF;
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_name = 'events' AND column_name = 'thread_id') THEN
    ALTER TABLE events RENAME COLUMN thread_id TO conversation_id;
  END IF;
END $$;

ALTER TABLE persons       ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT 'default';
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT 'default';
ALTER TABLE events        ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT 'default';

-- People: slug is unique within a store. The old foreign key pointing at the global
-- slug must be dropped before dropping the unique key it depends on.
ALTER TABLE conversations DROP CONSTRAINT IF EXISTS threads_person_slug_fkey;
ALTER TABLE conversations DROP CONSTRAINT IF EXISTS conversations_person_slug_fkey;
ALTER TABLE persons DROP CONSTRAINT IF EXISTS persons_slug_key;
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'persons_tenant_slug_key') THEN
    ALTER TABLE persons ADD CONSTRAINT persons_tenant_slug_key UNIQUE (tenant_id, slug);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'conversations_person_fkey') THEN
    ALTER TABLE conversations ADD CONSTRAINT conversations_person_fkey
      FOREIGN KEY (tenant_id, person_slug) REFERENCES persons (tenant_id, slug);
  END IF;
END $$;

DROP INDEX IF EXISTS threads_post_url_uq;
DROP INDEX IF EXISTS threads_person_kind_uq;
DROP INDEX IF EXISTS events_thread_at_idx;
DROP INDEX IF EXISTS events_actor_at_idx;
CREATE UNIQUE INDEX IF NOT EXISTS conversations_post_url_uq ON conversations (tenant_id, url) WHERE kind = 'post_thread';
CREATE UNIQUE INDEX IF NOT EXISTS conversations_person_kind_uq ON conversations (tenant_id, kind, person_slug) WHERE kind <> 'post_thread';
CREATE INDEX IF NOT EXISTS events_conversation_at_idx ON events (tenant_id, conversation_id, at, id);
CREATE INDEX IF NOT EXISTS events_tenant_actor_at_idx ON events (tenant_id, actor, at DESC, id DESC);
CREATE INDEX IF NOT EXISTS persons_slug_trgm ON persons USING gin (slug gin_trgm_ops);

DROP TRIGGER IF EXISTS threads_append_only ON conversations;
DROP TRIGGER IF EXISTS threads_no_truncate ON conversations;
CREATE OR REPLACE TRIGGER conversations_append_only BEFORE UPDATE OR DELETE ON conversations FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER conversations_no_truncate BEFORE TRUNCATE ON conversations FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER events_append_only BEFORE UPDATE OR DELETE ON events FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER events_no_truncate BEFORE TRUNCATE ON events FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();

DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['persons', 'conversations', 'events'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    EXECUTE format($f$CREATE POLICY tenant_isolation ON %I
      USING (tenant_id = current_setting('app.tenant_id', true))
      WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$f$, t);
  END LOOP;
END $$;

GRANT SELECT, INSERT ON persons, conversations, events TO tk_api;
GRANT USAGE ON persons_id_seq, conversations_id_seq, events_id_seq TO tk_api;
