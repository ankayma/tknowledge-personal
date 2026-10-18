package tka_test

import (
	"context"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// lookup finds a thread's summary by its words, but only the current one: a
// superseded summary leaves the index when the newer one is written, not just
// the answer (owner 5/10/2026). The shape is the real case — D-13 and S-7 on
// TH-4 for "quant-CTO modelrank", where the summary names the two words far
// apart and the old summary S-4 named them too.
func TestLookupSummaries_OnlyCurrentAndStale(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)

	th, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Bao looks for prediction-market roles after the Crowley post", Keywords: []string{"prediction-talent"}})
	if err != nil {
		t.Fatal(err)
	}
	basis := onDoc(t, r, a, "https://example.com/rivera",
		"Sam Rivera profile, read 2026-10-04. Chief Technology Officer, Primary Commodity Fund. modelrank.net ranks LLMs by trading PnL per inference dollar.")
	ct, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Sam Rivera runs modelrank.net, a public benchmark ranking language models by trading PnL.",
		BasisInput: basis, Thread: th.ID})
	if err != nil || ct.Entry == nil {
		t.Fatalf("admit: %+v %v", ct, err)
	}
	T := ct.Entry.ID
	if _, err := r.Link(ctx, a, tka.LinkInput{Thread: th.ID, IDs: []string{basis.SourceDoc}}); err != nil {
		t.Fatal(err)
	}
	write := func(text string) tka.Summary {
		t.Helper()
		s, err := r.Summarize(ctx, a, tka.SummarizeInput{Thread: th.ID, Text: text, Author: "claude"})
		if err != nil {
			t.Fatalf("summarize: %v", err)
		}
		return s
	}
	old := write("Sam Rivera là quant-CTO và chạy modelrank (" + T + ").")
	cur := write("Bảo đã rà danh sách member, đọc profile vào tài liệu (" + basis.SourceDoc + "). Sam Rivera là quant-CTO phân loại cơ chế PM và chạy modelrank.net (" + T + ").")

	got, err := r.LookupSummaries(ctx, "", "quant-CTO modelrank")
	if err != nil || len(got) != 1 || got[0].ID != cur.ID || got[0].Thread != th.ID || got[0].Stale || got[0].Status != "current" {
		t.Fatalf("only the current summary, fresh, of its thread (old %s): %+v %v", old.ID, got, err)
	}
	if docs, err := r.LookupDocuments(ctx, "", "quant-CTO modelrank"); err != nil || len(docs) != 1 || docs[0].ID != basis.SourceDoc {
		t.Fatalf("the document is found by the same words: %+v %v", docs, err)
	}
	// Another thread whose summary only comes close ("quan sát", "quan điểm"):
	// it stays out while a summary carries the words verbatim.
	th2, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Bao bets prediction markets can insure event risk", Keywords: []string{"pm-insurance"}})
	if err != nil {
		t.Fatal(err)
	}
	as, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Prediction-market prices can insure the risk of specific events as of 2026-10-03",
		Type: "A-p", Source: "owner", If: "a desk hedges an event on Kalshi", Not: "no desk does by 2026-12-31", Thread: th2.ID})
	if err != nil || as.Entry == nil {
		t.Fatalf("assume: %+v %v", as, err)
	}
	near, err := r.Summarize(ctx, a, tka.SummarizeInput{Thread: th2.ID, Author: "claude",
		Text: "Chưa có quan sát nào nâng hoặc bác giả định, theo quan điểm quant (" + as.Entry.ID + ")."})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.LookupSummaries(ctx, "", "quant-CTO"); len(got) != 1 || got[0].ID != cur.ID {
		t.Fatalf("the words verbatim win outright over %s: %+v", near.ID, got)
	}

	// The old one is out of the index, not hidden on the way out.
	var rows int
	var indexed string
	if err := r.Pool.QueryRow(ctx, `SELECT count(*), max(summary_id) FROM summary_search WHERE thread_id = $1`, th.ID).Scan(&rows, &indexed); err != nil || rows != 1 || indexed != cur.ID {
		t.Fatalf("summary_search holds one row, the current summary: %d %s %v", rows, indexed, err)
	}

	// A change to the thread after it shows as stale on the hit.
	if _, err := r.Demote(ctx, a, T, "the profile changed", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.LookupSummaries(ctx, "", "quant-CTO modelrank"); len(got) != 1 || !got[0].Stale {
		t.Fatalf("stale after a demote: %+v", got)
	}

	// A newer summary takes its place in the index.
	next := write("Sam Rivera, quant-CTO, không còn chạy modelrank theo profile mới (" + basis.SourceDoc + ").")
	if got, _ := r.LookupSummaries(ctx, "", "quant-CTO modelrank"); len(got) != 1 || got[0].ID != next.ID || got[0].Stale {
		t.Fatalf("the newer summary replaces %s: %+v", cur.ID, got)
	}
	if got, _ := r.LookupSummaries(ctx, "", "nothing like this anywhere"); len(got) != 0 {
		t.Fatalf("no match: %+v", got)
	}

	// thread(TH) shows the current summary alone; summaries="all" the chain,
	// oldest first, each marked, the current one with its staleness.
	v, err := r.ReadThread(ctx, "", th.ID, 1, false)
	if err != nil || v.Summary == nil || v.Summary.ID != next.ID || v.Summaries != nil {
		t.Fatalf("default view: %+v %+v %v", v.Summary, v.Summaries, err)
	}
	v, err = r.ReadThread(ctx, "", th.ID, 1, true)
	if err != nil || v.Summaries == nil || len(*v.Summaries) != 3 {
		t.Fatalf("all summaries: %+v %v", v.Summaries, err)
	}
	for i, want := range []struct{ id, status string }{{old.ID, "superseded"}, {cur.ID, "superseded"}, {next.ID, "current"}} {
		s := (*v.Summaries)[i]
		if s.ID != want.id || s.Status != want.status || s.CreatedAt.IsZero() || (s.Status == "superseded" && s.Stale) {
			t.Fatalf("summary %d: want %s %s, got %+v", i, want.id, want.status, s)
		}
	}
}

// Summaries written before summary_search existed are put in on the next boot:
// the newest of each thread, and nothing else.
func TestSummarySearch_BackfilledOnMigrate(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	th, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Acme renews its contract", Keywords: []string{"acme"}})
	if err != nil {
		t.Fatal(err)
	}
	ct, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Acme renewed its contract on 2026-10-01",
		BasisInput: fact(t, r, a, "https://example.com/acme"), Thread: th.ID})
	if err != nil || ct.Entry == nil {
		t.Fatalf("admit: %+v %v", ct, err)
	}
	for _, text := range []string{"Acme renewed early (" + ct.Entry.ID + ").", "Acme renewed for two years (" + ct.Entry.ID + ")."} {
		if _, err := r.Summarize(ctx, a, tka.SummarizeInput{Thread: th.ID, Text: text, Author: "owner"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Pool.Exec(ctx, `DELETE FROM summary_search`); err != nil {
		t.Fatal(err)
	}
	if err := migrateAgain(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := r.LookupSummaries(ctx, "", "acme renewed")
	if err != nil || len(got) != 1 || got[0].ID != "S-2" {
		t.Fatalf("backfill keeps the newest only: %+v %v", got, err)
	}
}
