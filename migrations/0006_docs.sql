-- Versioned documents.
-- Same (normalized) title → a new version that supersedes the old one. No overwrites.

CREATE TABLE IF NOT EXISTS docs (
  id          bigserial PRIMARY KEY,
  title       text    NOT NULL,
  title_key   text    NOT NULL,            -- normalized title: key of the version chain
  kind        text    NOT NULL CHECK (kind IN ('plan','spec','analysis','assessment','rule','post')),
  version     integer NOT NULL CHECK (version >= 1),
  body        text    NOT NULL,
  body_sha256 text    NOT NULL,
  refs        text[]  NOT NULL DEFAULT '{}', -- subject or person slug
  supersedes  bigint  REFERENCES docs(id),
  user_id     text    NOT NULL,            -- the grant that wrote it
  actor_id    text    NOT NULL,
  grant_id    text    NOT NULL,
  scope       text,
  ts          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (title_key, version)
);
CREATE INDEX IF NOT EXISTS docs_title_key_version_idx ON docs (title_key, version DESC);

-- Append-only by schema, not just by permissions: unlike the ledger, no CJCM watches
-- this table from outside, so UPDATE/DELETE/TRUNCATE are blocked even for superuser
-- (disabling the trigger is a deliberate DDL operation, not an accident).
CREATE OR REPLACE FUNCTION tk_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on % is not allowed: append-only', TG_OP, TG_TABLE_NAME
    USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE OR REPLACE TRIGGER docs_append_only BEFORE UPDATE OR DELETE ON docs
  FOR EACH ROW EXECUTE FUNCTION tk_append_only();
CREATE OR REPLACE TRIGGER docs_no_truncate BEFORE TRUNCATE ON docs
  FOR EACH STATEMENT EXECUTE FUNCTION tk_append_only();

GRANT SELECT, INSERT ON docs TO tk_api;
GRANT USAGE ON docs_id_seq TO tk_api;
