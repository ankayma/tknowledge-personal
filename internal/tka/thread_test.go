package tka_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

func fact(t *testing.T, r *tka.Repo, a tka.Actor, url string) tka.BasisInput {
	b := onDoc(t, r, a, url, "the email at "+url)
	b.Where = "the email, first paragraph"
	return b
}

func ids(lines []tka.ThreadLine) string {
	var out []string
	for _, l := range lines {
		out = append(out, l.ID)
	}
	return strings.Join(out, " ")
}

// The flow the user approved on 1/10/2026 (spec §6): a thread named by the
// words the person uses, filed into as claims, assumptions and documents are
// written, and read back as where the matter stands today.
func TestThread_OpenFileAndRead(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)

	v, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "  Senior SA   interview — Northwind ",
		Keywords: []string{"A42", "Northwind", "Northwind A42", "mira", "A-42"}})
	if err != nil || v.ID != "TH-1" || v.Title != "Senior SA interview — Northwind" || v.Status != "open" {
		t.Fatalf("open: %+v %v", v, err)
	}
	if strings.Join(v.Keywords, ",") != "a42,northwind,northwind-a42,mira" {
		t.Fatalf("keywords are normalised and deduplicated: %v", v.Keywords)
	}

	// Filed while writing, by keyword or by id.
	invite, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Northwind invited Lan Nguyen to a third-round interview on 2026-09-15",
		BasisInput: fact(t, r, a, "https://example.com/invite"), Thread: "a42"})
	if err != nil || invite.Entry == nil || invite.Thread == nil || invite.Thread.Thread != "TH-1" || !invite.Thread.New {
		t.Fatalf("admit into the thread: %+v %v", invite, err)
	}
	waiting, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Northwind sends Lan Nguyen the round-3 result before 2026-09-30",
		Type: "A-p", Source: "owner", If: "an email with the result", Not: "2026-09-30 passes with no email", Thread: "TH-1"})
	if err != nil || waiting.Entry == nil || waiting.Thread == nil || waiting.Thread.Target != waiting.Entry.ID {
		t.Fatalf("assume into the thread: %+v %v", waiting, err)
	}
	invitation := tka.DocumentInput{Filename: "invite.txt", Text: "Round 3 on 15 September",
		TextProducer: "owner", Method: "opened_source", SHA256: strings.Repeat("ab", 32), Thread: "northwind"}
	doc, err := r.AddDocument(ctx, a, invitation)
	if err != nil || doc.Thread == nil || doc.Thread.Thread != "TH-1" {
		t.Fatalf("document into the thread: %+v %v", doc, err)
	}
	// The same original again is the same document; asking to file it again is
	// not an error and writes no second link.
	invitation.Thread = "TH-1"
	again, err := r.AddDocument(ctx, a, invitation)
	if err != nil || again.ID != doc.ID || again.Thread == nil || again.Thread.New {
		t.Fatalf("same document, same link: %+v %v", again, err)
	}
	// A duplicate claim is still filed when asked.
	dup, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Northwind invited Lan Nguyen to a third-round interview on 2026-09-15",
		BasisInput: fact(t, r, a, "https://example.com/invite"), Thread: "mira"})
	if err != nil || dup.Entry == nil || dup.Entry.ID != invite.Entry.ID || dup.Thread == nil || dup.Thread.New {
		t.Fatalf("duplicate admit: %+v %v", dup, err)
	}

	if _, err := r.AddPrinciple(ctx, a, "P.3", "Honest gaps over false promises.", ""); err != nil {
		t.Fatal(err)
	}
	if res, err := r.Link(ctx, a, tka.LinkInput{Thread: "a42", IDs: []string{"p.3"}, Note: "how to write to Mira"}); err != nil ||
		len(res.Linked) != 1 || res.Linked[0].ID != "P.3" || res.Linked[0].Kind != "principle" {
		t.Fatalf("link a principle: %+v %v", res, err)
	}

	view, err := r.ReadThread(ctx, "", "northwind-a42", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Chain) != 1 || view.Chain[0].ID != invite.Entry.ID || len(view.Open) != 1 || view.Open[0].ID != waiting.Entry.ID ||
		len(view.Documents) != 1 || view.Documents[0].ID != doc.ID || len(view.Principles) != 1 {
		t.Fatalf("view: %+v", view)
	}
	for _, want := range []string{"As of ", invite.Entry.ID, waiting.Entry.ID} {
		if !strings.Contains(view.Counts, want) {
			t.Fatalf("counts %q lacks %q", view.Counts, want)
		}
	}
	if view.Summary != nil {
		t.Fatalf("no one wrote a summary, so there is none: %+v", view.Summary)
	}
}

// What a thread may point at is checked, and so is which thread is meant.
func TestThread_Gates(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	one, _ := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "A42 interview", Keywords: []string{"a42", "mira"}})
	two, _ := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Capital.com #068", Keywords: []string{"capital", "mira"}})

	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"empty title", func() error { _, e := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "  "}); return e }(), "EMPTY_TITLE"},
		{"empty keyword", func() error {
			_, e := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "x", Keywords: []string{"!!!"}})
			return e
		}(), "EMPTY_KEYWORD"},
		{"no keyword to tag", func() error { _, e := r.TagThread(ctx, a, tka.ThreadTagInput{Thread: one.ID}); return e }(), "EMPTY_KEYWORD"},
		{"unknown id", func() error { _, e := r.ReadThread(ctx, "", "TH-99", 1, false); return e }(), "THREAD_UNKNOWN"},
		{"unknown keyword", func() error { _, e := r.ReadThread(ctx, "", "nobody-calls-it-this", 1, false); return e }(), "THREAD_UNKNOWN"},
		{"ambiguous keyword", func() error { _, e := r.ReadThread(ctx, "", "Mira", 1, false); return e }(), "THREAD_AMBIGUOUS"},
		{"nothing to link", func() error { _, e := r.Link(ctx, a, tka.LinkInput{Thread: one.ID}); return e }(), "EMPTY_LINK"},
		{"truth not here", func() error { _, e := r.Link(ctx, a, tka.LinkInput{Thread: one.ID, IDs: []string{"T-99"}}); return e }(), "LINK_TARGET_UNKNOWN"},
		{"person not here", func() error {
			_, e := r.Link(ctx, a, tka.LinkInput{Thread: one.ID, IDs: []string{"person:nobody"}})
			return e
		}(), "LINK_TARGET_UNKNOWN"},
		{"conversation not here", func() error {
			_, e := r.Link(ctx, a, tka.LinkInput{Thread: one.ID, IDs: []string{"conv_5"}})
			return e
		}(), "LINK_TARGET_UNKNOWN"},
		{"not an id", func() error { _, e := r.Link(ctx, a, tka.LinkInput{Thread: one.ID, IDs: []string{"17"}}); return e }(), "LINK_TARGET_UNKNOWN"},
		{"unlink without reason", func() error {
			_, e := r.Unlink(ctx, a, tka.UnlinkInput{Thread: one.ID, ID: two.ID})
			return e
		}(), "EMPTY_REASON"},
		{"unlink what is not linked", func() error {
			_, e := r.Unlink(ctx, a, tka.UnlinkInput{Thread: one.ID, ID: two.ID, Reason: "x"})
			return e
		}(), "NOT_LINKED"},
	} {
		if got := code(t, c.err); got != c.want {
			t.Errorf("%s: want %s, got %s (%v)", c.name, c.want, got, c.err)
		}
	}

	// THREAD_AMBIGUOUS names the candidates; the store never picks.
	_, err := r.ReadThread(ctx, "", "mira", 1, false)
	var e *tka.Error
	if !asTka(err, &e) || ids(e.Candidates) != one.ID+" "+two.ID || e.Next == "" {
		t.Fatalf("candidates: %+v", e)
	}
	// A write naming an ambiguous thread writes nothing at all.
	before := countTruths(t, r)
	_, err = r.Admit(ctx, a, tka.AdmitInput{Claim: "Northwind invited Lan Nguyen to a third-round interview on 2026-09-15",
		BasisInput: fact(t, r, a, "https://example.com/invite"), Thread: "mira"})
	if code(t, err) != "THREAD_AMBIGUOUS" || countTruths(t, r) != before {
		t.Fatalf("ambiguous thread on admit: %v, truths %d → %d", err, before, countTruths(t, r))
	}
	_, err = r.Assume(ctx, a, tka.AssumeInput{Claim: "Northwind replies to Lan Nguyen before 2026-10-30", Type: "A-p", Source: "owner",
		If: "a reply", Not: "no reply by then", Thread: "TH-77"})
	if code(t, err) != "THREAD_UNKNOWN" {
		t.Fatalf("unknown thread on assume: %v", err)
	}
	if open, _ := r.OpenAssumptions(ctx, "", ""); len(open) != 0 {
		t.Fatalf("refused assume wrote %+v", open)
	}

	// Linking twice: the second time says so.
	if _, err := r.Link(ctx, a, tka.LinkInput{Thread: one.ID, IDs: []string{two.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Link(ctx, a, tka.LinkInput{Thread: one.ID, IDs: []string{"th-0002"}}); code(t, err) != "ALREADY_LINKED" {
		t.Fatalf("second link: %v", err)
	}

	// Close once; the second close says so; a closed thread still reads.
	if v, err := r.CloseThread(ctx, a, tka.ThreadCloseInput{Thread: two.ID, Reason: "no reply in 30 days"}); err != nil || v.Status != "closed" {
		t.Fatalf("close: %+v %v", v, err)
	}
	if _, err := r.CloseThread(ctx, a, tka.ThreadCloseInput{Thread: two.ID, Reason: "again"}); code(t, err) != "ALREADY_CLOSED" {
		t.Fatalf("close twice: %v", err)
	}
	if _, err := r.CloseThread(ctx, a, tka.ThreadCloseInput{Thread: one.ID}); code(t, err) != "EMPTY_REASON" {
		t.Fatalf("close without reason: %v", err)
	}
	if v, err := r.ReadThread(ctx, "", "capital", 1, false); err != nil || v.Status != "closed" || v.ClosedBy == nil {
		t.Fatalf("closed thread reads: %+v %v", v, err)
	}
}

// Threads form a graph: one may sit inside several, never inside itself.
func TestThread_GraphNotTreeNoCycle(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	open := func(title string) string {
		t.Helper()
		v, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return v.ID
	}
	dubai, a42, round3, aiRoles := open("Job search Dubai 2026"), open("A42 interview"), open("A42 round 3"), open("AI architect roles")
	link := func(from, to string) error {
		_, err := r.Link(ctx, a, tka.LinkInput{Thread: from, IDs: []string{to}})
		return err
	}
	for _, l := range [][2]string{{dubai, a42}, {a42, round3}, {aiRoles, a42}} {
		if err := link(l[0], l[1]); err != nil {
			t.Fatalf("%s → %s: %v", l[0], l[1], err)
		}
	}
	if err := link(round3, dubai); code(t, err) != "THREAD_CYCLE" {
		t.Fatalf("round 3 → Dubai closes a loop: %v", err)
	}
	if err := link(a42, a42); code(t, err) != "THREAD_CYCLE" {
		t.Fatalf("a thread inside itself: %v", err)
	}

	v, err := r.ReadThread(ctx, "", a42, 1, false)
	if err != nil || ids(v.Parents) != dubai+" "+aiRoles || ids(v.Threads) != round3 {
		t.Fatalf("two parents, one child: parents %s, threads %s, %v", ids(v.Parents), ids(v.Threads), err)
	}
	top, err := r.ReadThread(ctx, "", dubai, 2, false)
	if err != nil || len(top.Threads) != 1 || ids(top.Threads[0].Threads) != round3 {
		t.Fatalf("depth 2 shows the grandchild: %+v %v", top.Threads, err)
	}
	if one, _ := r.ReadThread(ctx, "", dubai, 1, false); len(one.Threads) != 1 || len(one.Threads[0].Threads) != 0 {
		t.Fatalf("depth 1 shows children only: %+v", one.Threads)
	}

	// Unlinking reopens the way: the loop it would have closed no longer exists.
	if _, err := r.Unlink(ctx, a, tka.UnlinkInput{Thread: dubai, ID: a42, Reason: "filed under the wrong search"}); err != nil {
		t.Fatal(err)
	}
	if err := link(round3, dubai); err != nil {
		t.Fatalf("after unlink: %v", err)
	}
}

// Keywords come and go by events; lookup follows what holds now.
func TestThread_TagAndLookup(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	a42, _ := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Senior SA interview — Northwind", Keywords: []string{"a42", "northwind"}})
	r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Dubai rent", Keywords: []string{"apartment"}})

	res, err := r.TagThread(ctx, a, tka.ThreadTagInput{Thread: "a42", Add: []string{"Mira", "a42"}, Remove: []string{"northwind", "never-there"}, Reason: "the person calls it A42"})
	if err != nil || strings.Join(res.Added, ",") != "mira" || strings.Join(res.Removed, ",") != "northwind" ||
		strings.Join(res.Unchanged, ",") != "a42,never-there" || strings.Join(res.Thread.Keywords, ",") != "a42,mira" {
		t.Fatalf("tag: %+v %v", res, err)
	}

	find := func(q string) string {
		t.Helper()
		got, err := r.LookupThreads(ctx, "", q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range got {
			out = append(out, m.ID+"/"+m.Matched)
		}
		return strings.Join(out, " ")
	}
	if got := find("A 42"); got != a42.ID+"/keyword:a42" {
		t.Fatalf("exact keyword: %s", got)
	}
	if got := find("miraa"); got != a42.ID+"/keyword:mira" {
		t.Fatalf("a keyword typed half-right: %s", got)
	}
	if got := find("northwind"); got != a42.ID+"/title" {
		t.Fatalf("a removed keyword no longer names it, the title still does: %s", got)
	}
	if got := find("zzz"); got != "" {
		t.Fatalf("nothing close: %s", got)
	}
}

// refute closes an open A-p whose not: happened — and nothing else.
func TestRefute(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	p, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Northwind sends Lan Nguyen the round-3 result before 2026-09-30",
		Type: "A-p", Source: "owner", If: "an email with the result", Not: "2026-09-30 passes with no email"})
	if err != nil {
		t.Fatal(err)
	}
	reading, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "The A42 recruiter reads Lan Nguyen's profile as a solution architect profile",
		Type: "A-r", Source: "ai"})
	if err != nil {
		t.Fatal(err)
	}
	offer, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Northwind sent Lan Nguyen an offer on 2026-10-01", BasisInput: fact(t, r, a, "https://example.com/offer")})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		in   tka.RefuteInput
		want string
	}{
		{"no observation", tka.RefuteInput{ID: p.Entry.ID}, "EMPTY_OBSERVATION"},
		{"a reading aid", tka.RefuteInput{ID: reading.Entry.ID, Observed: "x"}, "NOT_REFUTABLE"},
		{"a claim in the ledger", tka.RefuteInput{ID: offer.Entry.ID, Observed: "x"}, "NOT_REFUTABLE"},
		{"no such assumption", tka.RefuteInput{ID: "A-99", Observed: "x"}, "NOT_FOUND"},
		{"evidence not in this store", tka.RefuteInput{ID: p.Entry.ID, Observed: "x", Evidence: "T-99"}, "NOT_FOUND"},
	} {
		if _, err := r.Refute(ctx, a, c.in); code(t, err) != c.want {
			t.Errorf("%s: want %s, got %v", c.name, c.want, err)
		}
	}

	got, err := r.Refute(ctx, a, tka.RefuteInput{ID: strings.Replace(p.Entry.ID, "A-", "a-00", 1),
		Observed: "the result came on 2026-10-01, after the date", Evidence: offer.Entry.ID})
	if err != nil || got.Status != "refuted" {
		t.Fatalf("refute: %+v %v", got, err)
	}
	last := got.History[len(got.History)-1]
	if last.Event != "refuted" || last.Note == nil || !strings.Contains(*last.Note, "evidence: "+offer.Entry.ID) {
		t.Fatalf("history: %+v", last)
	}
	if _, err := r.Refute(ctx, a, tka.RefuteInput{ID: p.Entry.ID, Observed: "again"}); code(t, err) != "NOT_REFUTABLE" {
		t.Fatalf("refute twice: %v", err)
	}
	if _, err := r.Promote(ctx, a, tka.PromoteInput{AssumptionID: p.Entry.ID, BasisInput: fact(t, r, a, "https://example.com/x")}); code(t, err) != "NOT_OPEN" {
		t.Fatalf("a refuted assumption is not promoted: %v", err)
	}
}

// An assumption promoted into a claim brings that claim into the thread's
// chain: it is the same sentence, now standing.
func TestThread_PromotedAssumptionJoinsTheChain(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	th, _ := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "A42 interview", Keywords: []string{"a42"}})
	p, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Mira Patel invites Lan Nguyen to round 2 before 2026-09-26",
		Type: "A-p", Source: "owner", If: "an invite email", Not: "2026-09-26 passes with nothing", Thread: "a42"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Promote(ctx, a, tka.PromoteInput{AssumptionID: p.Entry.ID, BasisInput: fact(t, r, a, "https://example.com/round2")})
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.ReadThread(ctx, "", th.ID, 1, false)
	if err != nil || len(v.Chain) != 1 || v.Chain[0].ID != res.Entry.ID || v.Chain[0].Via == nil || *v.Chain[0].Via != p.Entry.ID ||
		len(v.Open) != 0 || len(v.Settled) != 1 {
		t.Fatalf("promoted claim in the chain: %+v %v", v, err)
	}
}

// One store's threads are invisible from another, and cannot point into it.
func TestThread_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := testapi.StartPostgres(t)
	r := tka.New(pool)
	acme := tka.Actor{UserID: "bao", ActorID: "claude", GrantID: "g1", TenantID: "acme"}
	globex := tka.Actor{UserID: "lan", ActorID: "claude", GrantID: "g2", TenantID: "globex"}
	for _, id := range []string{"acme", "globex"} {
		if _, err := r.CreateTenant(ctx, tka.Tenant{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	mine, err := r.OpenThread(ctx, acme, tka.ThreadOpenInput{Title: "A42 interview", Keywords: []string{"a42"}})
	if err != nil || mine.ID != "TH-1" {
		t.Fatalf("acme: %+v %v", mine, err)
	}
	theirs, err := r.OpenThread(ctx, globex, tka.ThreadOpenInput{Title: "Their matter", Keywords: []string{"a42"}})
	if err != nil || theirs.ID != "TH-1" {
		t.Fatalf("globex numbers its own: %+v %v", theirs, err)
	}
	claim, err := r.Admit(ctx, globex, tka.AdmitInput{Claim: "Globex opened a Dubai office on 2026-02-11", BasisInput: fact(t, r, globex, "https://example.com/globex")})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := r.ReadThread(ctx, "acme", "a42", 1, false); err != nil || v.Title != "A42 interview" {
		t.Fatalf("acme's a42 is acme's: %+v %v", v, err)
	}
	if _, err := r.Link(ctx, acme, tka.LinkInput{Thread: "TH-1", IDs: []string{claim.Entry.ID}}); code(t, err) != "LINK_TARGET_UNKNOWN" {
		t.Fatalf("acme cannot point at globex's claim: %v", err)
	}
	if got, _ := r.LookupThreads(ctx, "acme", "their matter"); len(got) != 0 {
		t.Fatalf("lookup stays in its store: %+v", got)
	}
}

// Thread tables are append-only, like the ledger.
func TestThread_TablesAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := testapi.StartPostgres(t)
	r := tka.New(pool)
	a := tka.Actor{UserID: "bao", ActorID: "claude", GrantID: "g"}
	if _, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "A42", Keywords: []string{"a42"}}); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE thread SET title = 'x'`, `DELETE FROM thread_keyword`, `DELETE FROM thread_history`,
		`UPDATE thread_history SET event = 'closed'`, `TRUNCATE thread_link`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("%s: %v", sql, err)
		}
	}
}

func asTka(err error, target **tka.Error) bool {
	e, ok := err.(*tka.Error)
	*target = e
	return ok
}

func countTruths(t *testing.T, r *tka.Repo) int {
	t.Helper()
	list, err := r.Truths(context.Background(), "", false, 200)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}
