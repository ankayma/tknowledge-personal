-- Tenant: one design for both deployment modes.
-- Pooled is many tenants in one DB; siloed is exactly this schema with one tenant
-- in its own DB. There is no second code path: moving a tenant from pooled to
-- siloed is just a connection string change.

-- Tenant catalog (control plane). Empty `dsn` = use the default DB; fill it in when
-- a tenant is moved to its own cluster.
CREATE TABLE IF NOT EXISTS tenant (
  id         text PRIMARY KEY,
  name       text NOT NULL,
  tier       text NOT NULL DEFAULT 'free',
  dsn        text,
  region     text,
  created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO tenant (id, name) VALUES ('default', 'default') ON CONFLICT (id) DO NOTHING;

-- Per-tenant id counter: T-0001 is the first claim of *your* store, not of
-- someone else's. One row is locked per write; in return, numbers have no gaps
-- and nobody can guess another tenant's traffic from the gaps.
CREATE TABLE IF NOT EXISTS tenant_counter (
  tenant_id text NOT NULL,
  kind      text NOT NULL CHECK (kind IN ('T', 'A')),
  next      bigint NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, kind)
);

ALTER TABLE principle      ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT 'default';
ALTER TABLE truth_ledger   ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT 'default';
ALTER TABLE assumption     ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT 'default';
ALTER TABLE entry_history  ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT 'default';
ALTER TABLE entry_relation ADD COLUMN IF NOT EXISTS tenant_id text NOT NULL DEFAULT 'default';

-- Keys become composite: two tenants using the same code or the same id is
-- normal, not a conflict.
ALTER TABLE principle    DROP CONSTRAINT IF EXISTS principle_pkey;
ALTER TABLE principle    ADD PRIMARY KEY (tenant_id, code);

ALTER TABLE assumption   DROP CONSTRAINT IF EXISTS assumption_pkey;
ALTER TABLE assumption   DROP CONSTRAINT IF EXISTS assumption_id_key;
ALTER TABLE assumption   ALTER COLUMN num DROP DEFAULT;
ALTER TABLE assumption   ADD PRIMARY KEY (tenant_id, num);
CREATE UNIQUE INDEX IF NOT EXISTS assumption_tenant_id_idx ON assumption (tenant_id, id);

ALTER TABLE truth_ledger DROP CONSTRAINT IF EXISTS truth_ledger_pkey;
ALTER TABLE truth_ledger DROP CONSTRAINT IF EXISTS truth_ledger_id_key;
ALTER TABLE truth_ledger ALTER COLUMN num DROP DEFAULT;
ALTER TABLE truth_ledger ADD PRIMARY KEY (tenant_id, num);
CREATE UNIQUE INDEX IF NOT EXISTS truth_ledger_tenant_id_idx ON truth_ledger (tenant_id, id);

DROP INDEX IF EXISTS entry_history_entry_idx;
CREATE INDEX IF NOT EXISTS entry_history_tenant_entry_idx ON entry_history (tenant_id, entry_id, id DESC);
CREATE INDEX IF NOT EXISTS entry_relation_tenant_idx ON entry_relation (tenant_id, from_id);
CREATE INDEX IF NOT EXISTS assumption_tenant_key_idx ON assumption (tenant_id, claim_key);
CREATE INDEX IF NOT EXISTS truth_tenant_key_idx ON truth_ledger (tenant_id, claim_key);

-- Isolation at the DB layer, not only in code: if a WHERE clause is forgotten,
-- Postgres still blocks it. `app.tenant_id` is set by tk-api for each transaction.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['principle', 'truth_ledger', 'assumption', 'entry_history', 'entry_relation'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    EXECUTE format($f$CREATE POLICY tenant_isolation ON %I
      USING (tenant_id = current_setting('app.tenant_id', true))
      WITH CHECK (tenant_id = current_setting('app.tenant_id', true))$f$, t);
  END LOOP;
END $$;

GRANT SELECT, INSERT ON tenant, tenant_counter TO tk_api;
GRANT UPDATE ON tenant_counter TO tk_api;
