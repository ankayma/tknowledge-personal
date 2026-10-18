-- People, conversations, events.
-- All three are insert-only: participants, last_event_at, alias, company/role/degree/tier
-- and my_touches_since_their_last are computed at read time; no column is overwritten.
--
-- A "conversation" is an exchange with one person on one channel (or one post).
-- Before 1/10/2026 it was called "thread" (table threads, events.thread_id); that name
-- now belongs to meta-knowledge threads (0019). Old stores are renamed in 0018. This file
-- re-runs on every startup, so it only builds the new shape when the old `threads` table
-- does not exist; otherwise a re-run would build an empty table next to the renamed one.
-- Indexes, triggers, grants, tenant_id and RLS for conversations/events live in 0018,
-- which runs after the names are correct.

CREATE TABLE IF NOT EXISTS persons (
  id         bigserial PRIMARY KEY,
  slug       text NOT NULL UNIQUE CHECK (slug <> '' AND slug <> 'me'),
  name       text NOT NULL,
  user_id    text NOT NULL,
  actor_id   text NOT NULL,
  grant_id   text NOT NULL,
  scope      text,
  created_at timestamptz NOT NULL DEFAULT now()
);

DO $$
BEGIN
  IF to_regclass('threads') IS NULL THEN
    CREATE TABLE IF NOT EXISTS conversations (
      id                    bigserial PRIMARY KEY,
      kind                  text NOT NULL CHECK (kind IN ('post_thread', 'dm', 'email', 'connect')),
      url                   text,             -- post_thread: normalized url (key)
      person_slug           text,             -- dm/email/connect: the person (key together with kind)
      owner_of_conversation text,             -- 'me' or a person slug
      user_id               text NOT NULL,
      actor_id              text NOT NULL,
      grant_id              text NOT NULL,
      scope                 text,
      created_at            timestamptz NOT NULL DEFAULT now(),
      CHECK ((kind = 'post_thread' AND url IS NOT NULL AND person_slug IS NULL)
          OR (kind <> 'post_thread' AND person_slug IS NOT NULL AND url IS NULL))
    );
    CREATE TABLE IF NOT EXISTS events (
      id              bigserial PRIMARY KEY,
      conversation_id bigint NOT NULL REFERENCES conversations(id),
      actor           text NOT NULL,            -- 'me' or persons.slug
      type            text NOT NULL CHECK (type IN ('comment', 'reply', 'react', 'note', 'dm', 'email', 'accept', 'view')),
      body            text,
      verbatim        boolean NOT NULL DEFAULT false,
      at              timestamptz NOT NULL,
      in_reply_to     bigint REFERENCES events(id),
      chat_id         text,
      user_id         text NOT NULL,
      actor_id        text NOT NULL,
      grant_id        text NOT NULL,
      scope           text,
      created_at      timestamptz NOT NULL DEFAULT now(),
      CHECK ((type IN ('react', 'accept', 'view')) = (body IS NULL))
    );
  END IF;
END $$;

-- Append-only by schema, like docs (0006): reuses the tk_append_only() function.
CREATE OR REPLACE TRIGGER persons_append_only BEFORE UPDATE OR DELETE ON persons FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER persons_no_truncate BEFORE TRUNCATE ON persons FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();

GRANT SELECT, INSERT ON persons TO tk_api;
GRANT USAGE ON persons_id_seq TO tk_api;
