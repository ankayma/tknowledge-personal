package migrate_test

import (
	"context"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
)

// 0032 gives the open demo-… trial stores the word cap once — 100 words, 600
// characters as the backstop — and leaves paid and closed stores alone. It
// also lets thread history record a summary. Applying it again changes nothing.
func TestSummaryWords_BackfillOnce(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := testapi.StartPostgres(t)
	if _, err := pool.Exec(ctx, `
		INSERT INTO tenant (id, name, tier, expires_at, max_claim) VALUES
		  ('demo-open', 'trial', 'trial', NULL,                        300),
		  ('demo-shut', 'trial', 'trial', now() - interval '1 hour',   300),
		  ('demo-paid', 'owner', 'paid',  NULL,                        300);
		UPDATE tenant SET max_words = NULL WHERE id LIKE 'demo-%'`); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if err := migrate.Apply(ctx, pool); err != nil {
			t.Fatalf("apply (run %d): %v", run, err)
		}
	}
	read := func(id string) (words *int, claim int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT max_words, max_claim FROM tenant WHERE id = $1`, id).Scan(&words, &claim); err != nil {
			t.Fatal(err)
		}
		return words, claim
	}
	if w, c := read("demo-open"); w == nil || *w != 100 || c != 600 {
		t.Fatalf("open trial: words %v, claim %d", w, c)
	}
	if w, c := read("demo-shut"); w != nil || c != 300 {
		t.Fatalf("a closed trial stays as it was: words %v, claim %d", w, c)
	}
	if w, c := read("demo-paid"); w != nil || c != 300 {
		t.Fatalf("a paid store keeps its terms: words %v, claim %d", w, c)
	}
	// Thread history takes the summarized event, on a store migrated twice.
	if _, err := pool.Exec(ctx, `
		INSERT INTO thread (tenant_id, num, title, signer, producer, grant_id) VALUES ('default', 1, 'm', 'bao', 'c', 'g');
		INSERT INTO thread_history (tenant_id, thread_id, event, target, actor) VALUES ('default', 'TH-1', 'summarized', 'S-1', 'bao')`); err != nil {
		t.Fatalf("summarized event: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO thread_history (tenant_id, thread_id, event, actor) VALUES ('default', 'TH-1', 'renamed', 'bao')`); err == nil {
		t.Fatal("an unknown event must still be refused")
	}
}
