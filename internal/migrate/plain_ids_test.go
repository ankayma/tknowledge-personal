package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
)

// A store written before 0017 carries padded ids (T-0007) in its id columns and
// in every place that names an entry. Applying the migrations turns all of them
// into the one spelling (T-7), keeps the append-only triggers on, and a second
// run changes nothing.
func TestPlainIDs_ConvertsAPaddedStore(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := testapi.StartPostgres(t)
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}

	// Put the store back in the shape 0010/0013 used to create.
	exec(`
		ALTER TABLE truth_ledger DROP COLUMN id;
		ALTER TABLE truth_ledger ADD COLUMN id text GENERATED ALWAYS AS ('T-' || lpad(num::text, 4, '0')) STORED;
		CREATE UNIQUE INDEX truth_ledger_tenant_id_idx ON truth_ledger (tenant_id, id);
		ALTER TABLE assumption DROP COLUMN id;
		ALTER TABLE assumption ADD COLUMN id text GENERATED ALWAYS AS ('A-' || lpad(num::text, 4, '0')) STORED;
		CREATE UNIQUE INDEX assumption_tenant_id_idx ON assumption (tenant_id, id);
		ALTER TABLE document DROP COLUMN id;
		ALTER TABLE document ADD COLUMN id text GENERATED ALWAYS AS ('D-' || lpad(num::text, 4, '0')) STORED;
		ALTER TABLE document ADD CONSTRAINT document_tenant_id_id_key UNIQUE (tenant_id, id);`)
	exec(`
		INSERT INTO document (num, sha256, media_type, size, bytes, signer, producer, grant_id)
		VALUES (3, repeat('a', 64), 'text/plain', 1, 'x', 'bao', 'c', 'g');
		INSERT INTO assumption (num, claim, claim_key, type, source_kind, if_obs, not_obs, supports, signer, producer, grant_id)
		VALUES (42, 'a', 'a', 'A-p', 'owner', 'i', 'n', '{T-0007}', 'bao', 'c', 'g');
		INSERT INTO truth_ledger (num, claim, claim_key, basis, source, source_doc, verified_at, supports, promoted_from, signer, producer, grant_id)
		VALUES (7, 'f', 'f', 'fact', 's', 'D-0003', '2026-09-20', '{"Part C §H.6.1"}', 'A-0042', 'bao', 'c', 'g');
		INSERT INTO truth_ledger (num, claim, claim_key, basis, verified_at, premises, derivation, supersedes, source_entry, signer, producer, grant_id)
		VALUES (8, 'd', 'd', 'derivation', '2026-09-21', '{P.3,T-0007}', 'P.3 ∧ T-0007 ⟹ d', 'T-0007', NULL, 'bao', 'c', 'g');
		INSERT INTO entry_history (entry_id, event, actor, note) VALUES ('T-0008', 'created', 'bao', 'promoted from A-0042 · see T-0007');
		INSERT INTO entry_relation (from_id, to_id, kind, actor) VALUES ('T-0008', 'T-0007', 'restates', 'bao');`)

	for run := 1; run <= 2; run++ {
		if err := migrate.Apply(ctx, pool); err != nil {
			t.Fatalf("apply (run %d): %v", run, err)
		}
	}

	var got string
	if err := pool.QueryRow(ctx, `
		SELECT concat_ws(' | ',
		  (SELECT string_agg(id || ':' || array_to_string(premises, ',') || ':' || coalesce(supersedes, '-') || ':' ||
		          coalesce(promoted_from, '-') || ':' || coalesce(source_doc, '-'), ' ' ORDER BY num) FROM truth_ledger),
		  (SELECT id || ':' || array_to_string(supports, ',') FROM assumption),
		  (SELECT id FROM document),
		  (SELECT entry_id || ':' || note FROM entry_history WHERE entry_id LIKE 'T-8'),
		  (SELECT from_id || '>' || to_id FROM entry_relation))`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := "T-7::-:A-42:D-3 T-8:P.3,T-7:T-7:-:- | A-42:T-7 | D-3 | T-8:promoted from A-42 · see T-7 | T-8>T-7"
	if got != want {
		t.Fatalf("after migration:\n got  %s\n want %s", got, want)
	}

	// The prose of a derivation is the signer's words and stays as written.
	var chain string
	_ = pool.QueryRow(ctx, `SELECT derivation FROM truth_ledger WHERE num = 8`).Scan(&chain)
	if chain != "P.3 ∧ T-0007 ⟹ d" {
		t.Fatalf("derivation text changed: %q", chain)
	}

	// New rows past 9999 no longer collide, and the triggers are back on.
	exec(`INSERT INTO truth_ledger (num, claim, claim_key, basis, source, verified_at, signer, producer, grant_id)
	      VALUES (10000, 'g', 'g', 'fact', 's', '2026-10-01', 'bao', 'c', 'g'),
	             (1000,  'h', 'h', 'fact', 's', '2026-10-01', 'bao', 'c', 'g')`)
	if _, err := pool.Exec(ctx, `UPDATE truth_ledger SET claim = 'x' WHERE num = 7`); err == nil ||
		!strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("append-only trigger must be back on: %v", err)
	}
}
