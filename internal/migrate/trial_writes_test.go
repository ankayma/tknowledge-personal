package migrate_test

import (
	"context"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
)

// A database as it stood before 0034 (owner 13/10/2026): open demo-… trial
// stores carry the store-wide cap of 1000 writes from 0031, every address that
// opened one is in trial_owner, and there are no seats. Applying the
// migrations makes each such address a member on a free seat with one history
// row, takes the store-wide cap off the trial stores (writes are counted by
// seat now), and leaves closed try-… stores and a paid store's own cap alone.
// 0031 no longer re-caps anything once seats exist, and a second run changes
// nothing.
func TestSeatWrites_MovesTrialStoresToSeats(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := testapi.StartPostgres(t)
	exec := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`DROP TABLE member, member_seat_history, member_writes`)
	exec(`
		INSERT INTO tenant (id, name, tier, expires_at, max_entries, max_claim, max_writes) VALUES
		  ('demo-aaa', 'trial demo-aaa', 'trial', NULL,                       NULL, 600, 1000),
		  ('demo-bbb', 'trial demo-bbb', 'trial', NULL,                       NULL, 600, 1000),
		  ('try-ccc',  'trial try-ccc',  'trial', now() - interval '1 day',   60,   300, NULL),
		  ('acme',     'Acme',           'paid',  NULL,                       NULL, NULL, 500),
		  ('demo-new', 'trial demo-new', 'trial', NULL,                       NULL, 600, NULL);
		INSERT INTO tenant_writes (tenant_id, n) VALUES ('demo-aaa', 640), ('demo-bbb', 3);
		INSERT INTO trial_owner (email, tenant_id) VALUES
		  ('heavy@example.test', 'demo-aaa'), ('light@example.test', 'demo-bbb'), ('old@example.test', 'try-ccc')`)

	for run := 1; run <= 2; run++ {
		if err := migrate.Apply(ctx, pool); err != nil {
			t.Fatalf("apply %d: %v", run, err)
		}
	}

	capOf := func(id string) *int {
		t.Helper()
		var n *int
		if err := pool.QueryRow(ctx, `SELECT max_writes FROM tenant WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, id := range []string{"demo-aaa", "demo-bbb", "demo-new"} {
		if n := capOf(id); n != nil {
			t.Fatalf("%s: a trial store has no store-wide cap once seats count: %d", id, *n)
		}
	}
	if n := capOf("acme"); n == nil || *n != 500 {
		t.Fatalf("a paid store keeps its own cap: %v", n)
	}
	if n := capOf("try-ccc"); n != nil {
		t.Fatalf("a closed try- store is left alone: %v", n)
	}
	var closed bool
	if err := pool.QueryRow(ctx, `SELECT expires_at < now() FROM tenant WHERE id = 'try-ccc'`).Scan(&closed); err != nil || !closed {
		t.Fatalf("a closed store stays closed: %v %v", closed, err)
	}

	type seat struct{ seat, by string }
	members := map[string]seat{}
	rows, err := pool.Query(ctx, `SELECT m.tenant_id || '/' || m.user_id, m.seat, h.by
		FROM member m JOIN member_seat_history h USING (tenant_id, user_id)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		var s seat
		if err := rows.Scan(&k, &s.seat, &s.by); err != nil {
			t.Fatal(err)
		}
		if _, dup := members[k]; dup {
			t.Fatalf("%s: a second run must not add a second history row", k)
		}
		members[k] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]seat{
		"demo-aaa/heavy@example.test": {"free", "migration-0034"},
		"demo-bbb/light@example.test": {"free", "migration-0034"},
		"try-ccc/old@example.test":    {"free", "migration-0034"},
	}
	if len(members) != len(want) {
		t.Fatalf("members: %v", members)
	}
	for k, w := range want {
		if members[k] != w {
			t.Fatalf("%s: %+v, want %+v", k, members[k], w)
		}
	}

	// The month starts at zero: the old total is kept as history, never read as
	// this month's count.
	var monthly, total int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM member_writes),
		(SELECT n FROM tenant_writes WHERE tenant_id = 'demo-aaa')`).Scan(&monthly, &total); err != nil {
		t.Fatal(err)
	}
	if monthly != 0 || total != 640 {
		t.Fatalf("month counts %d (want 0), old total %d (want 640 kept)", monthly, total)
	}

	// A trial store capped again by hand (or by an old binary) is not re-capped
	// by 0031 on the next boot, and 0034 takes the cap off again.
	exec(`UPDATE tenant SET max_writes = NULL WHERE id = 'demo-new'`)
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if n := capOf("demo-new"); n != nil {
		t.Fatalf("0031 must not re-cap once seats exist: %d", *n)
	}

	// The seat history is append-only, like the ledgers.
	if _, err := pool.Exec(ctx, `UPDATE member_seat_history SET seat_to = 'unlimited'`); err == nil {
		t.Fatal("seat history must refuse UPDATE")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM member_seat_history`); err == nil {
		t.Fatal("seat history must refuse DELETE")
	}
}
