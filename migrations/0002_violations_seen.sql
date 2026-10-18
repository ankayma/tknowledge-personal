-- violations_seen: the checker writes here from the CJCM aud:alerts stream.
-- is_traceable reads this table to know which rows are in V(w). It holds no data until
-- the checker (Phase 1 step 5) runs: an empty table means "no violation seen yet",
-- not "no violation exists".
CREATE TABLE IF NOT EXISTS violations_seen (
  row_id bigint PRIMARY KEY,
  kind   text   NOT NULL,
  ts     timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT ON violations_seen TO tk_api;
GRANT SELECT, INSERT ON violations_seen TO tk_checker;
