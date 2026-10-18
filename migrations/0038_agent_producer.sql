-- "agent": the AI writing through the grant, whichever it is (owner 14/10/2026).
-- Which AI it was is the row's producer, read from the grant. "claude" stays.
-- Re-runs on every startup: only changes constraints that do not allow 'agent' yet.

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'document_text_producer_check'
                 AND pg_get_constraintdef(oid) LIKE '%agent%') THEN
    ALTER TABLE document DROP CONSTRAINT IF EXISTS document_text_producer_check;
    ALTER TABLE document ADD CONSTRAINT document_text_producer_check
      CHECK (text_producer IN ('owner', 'agent', 'claude', 'extract'));
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'thread_summary_author_check'
                 AND pg_get_constraintdef(oid) LIKE '%agent%') THEN
    ALTER TABLE thread_summary DROP CONSTRAINT IF EXISTS thread_summary_author_check;
    ALTER TABLE thread_summary ADD CONSTRAINT thread_summary_author_check
      CHECK (author IN ('owner', 'agent', 'claude'));
  END IF;
END $$;
