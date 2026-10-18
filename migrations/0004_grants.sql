-- Grant: the caller's identity.
-- Every row written through tk-api carries the grant_id of the grant that signed it. The
-- column is nullable because rows inserted outside the API (CJ-10, demo step 7) have no
-- grant, and they must still be insertable so CJCM can catch them.
ALTER TABLE ledger ADD COLUMN IF NOT EXISTS grant_id text;

-- Revoking a grant: insert-only, rows are never deleted (a revocation is an event).
CREATE TABLE IF NOT EXISTS revoked_grants (
  grant_id   text PRIMARY KEY,
  reason     text,
  revoked_by text NOT NULL,          -- user_id of the tk:admin grant that revoked it
  ts         timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT ON revoked_grants TO tk_api;
