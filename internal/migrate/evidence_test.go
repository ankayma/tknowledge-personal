package migrate_test

import (
	"context"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
)

// A store written before 0020: documents kept as files, a fact citing one with
// source_doc, and counters that already run past T/A/D. Migrating it — twice,
// as every boot does — gives the text documents their text, gives the fact its
// first evidence row exactly once, leaves a non-text document legacy, and does
// not trip on the counter rows that 0013 and 0019 never listed.
func TestEvidence_MigratesALegacyStoreTwice(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := testapi.StartPostgres(t)
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`
		INSERT INTO document (tenant_id, num, sha256, media_type, filename, size, bytes, signer, producer, grant_id)
		VALUES ('demo-x', 2, repeat('a', 64), 'text/plain', 'bybit.txt', 22, convert_to('Applied to Bybit 1/10', 'UTF8'), 'bao', 'claude', 'g'),
		       ('demo-x', 3, repeat('b', 64), 'image/png', 'shot.png', 4, '\x89504e47', 'bao', 'claude', 'g');
		INSERT INTO truth_ledger (tenant_id, num, claim, claim_key, basis, source, source_doc, verified_at, signer, producer, grant_id)
		VALUES ('demo-x', 2, 'Bao applied to Bybit on 2026-10-01', 'bao applied to bybit on 2026-10-01', 'fact',
		        'LinkedIn > Applied, row 1', 'D-2', '2026-10-01', 'bao', 'claude', 'g');
		INSERT INTO tenant_counter (tenant_id, kind, next) VALUES ('demo-x', 'TH', 3), ('demo-x', 'F', 2);`)

	for run := 1; run <= 2; run++ {
		if err := migrate.Apply(ctx, pool); err != nil {
			t.Fatalf("apply (run %d): %v", run, err)
		}
	}

	var text, producer, method *string
	if err := pool.QueryRow(ctx, `SELECT text, text_producer, method FROM document WHERE tenant_id = 'demo-x' AND num = 2`).
		Scan(&text, &producer, &method); err != nil {
		t.Fatal(err)
	}
	if text == nil || *text != "Applied to Bybit 1/10" || *producer != "owner" || *method != "read_text" {
		t.Fatalf("a legacy text document reads as its own text: %v %v %v", text, producer, method)
	}
	var pngText *string
	if err := pool.QueryRow(ctx, `SELECT text FROM document WHERE tenant_id = 'demo-x' AND num = 3`).Scan(&pngText); err != nil {
		t.Fatal(err)
	}
	if pngText != nil {
		t.Fatalf("a legacy image has no text to copy: %q", *pngText)
	}

	var rows, events int
	var where, signer string
	if err := pool.QueryRow(ctx, `SELECT count(*), max("where"), max(signer) FROM evidence
		WHERE tenant_id = 'demo-x' AND truth = 'T-2' AND document = 'D-2' AND kind = 'attest'`).Scan(&rows, &where, &signer); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM evidence_history
		WHERE tenant_id = 'demo-x' AND truth = 'T-2' AND event = 'attached'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || events != 1 || where != "LinkedIn > Applied, row 1" || signer != "bao" {
		t.Fatalf("one evidence row from source_doc, once: rows %d events %d where %q signer %q", rows, events, where, signer)
	}

	// The tables are append-only like the rest of the store.
	if _, err := pool.Exec(ctx, `UPDATE evidence SET "where" = 'x'`); err == nil {
		t.Fatal("evidence must refuse UPDATE")
	}
	if _, err := pool.Exec(ctx, `UPDATE document SET text = 'x' WHERE tenant_id = 'demo-x'`); err == nil {
		t.Fatal("the document trigger must be back on")
	}
	// And the counter check is whole again: every kind in use is listed.
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_counter (tenant_id, kind) VALUES ('demo-x', 'Z')`); err == nil {
		t.Fatal("an unknown counter kind must be refused")
	}
}
