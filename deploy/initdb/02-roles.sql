-- Bootstrap login roles shared for dev (passwords match the DSNs in docker-compose.yml).
-- GRANTs on specific tables live in each repo's migrations (the tables do not exist yet when initdb runs).
-- Checked by internal/deploytest/initdb_test.go (C-06, Postgres side): no role can
-- connect to another service's database.
CREATE ROLE tk_api     LOGIN PASSWORD 'dev';
CREATE ROLE tk_checker LOGIN PASSWORD 'dev';
CREATE ROLE tk_cdc     LOGIN REPLICATION PASSWORD 'dev';

CREATE ROLE cc_api     LOGIN PASSWORD 'dev';
CREATE ROLE cc_exec    LOGIN PASSWORD 'dev';
CREATE ROLE cc_outbox  LOGIN PASSWORD 'dev';

CREATE ROLE audit      LOGIN PASSWORD 'dev';

-- By default Postgres gives PUBLIC CONNECT on every database: revoke it and grant the right roles.
REVOKE CONNECT ON DATABASE tk, cc, audit FROM PUBLIC;
GRANT CONNECT ON DATABASE tk    TO tk_api, tk_checker, tk_cdc;
GRANT CONNECT ON DATABASE cc    TO cc_api, cc_exec, cc_outbox;
GRANT CONNECT ON DATABASE audit TO audit;

-- CJCM creates its own tables in `audit` (pgstore); make it owner so it has CREATE on schema public (PG15+).
ALTER DATABASE audit OWNER TO audit;
