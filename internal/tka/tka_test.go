package tka_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

func newRepo(t *testing.T) (*tka.Repo, tka.Actor) {
	t.Helper()
	pool, _, _ := testapi.StartPostgres(t)
	return tka.New(pool), tka.Actor{UserID: "bao", ActorID: "claude-desktop", GrantID: "gr_test"}
}

// code is the refusal code of a store error; anything else fails the test.
func code(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var e *tka.Error
	if !errors.As(err, &e) {
		t.Fatalf("not a tka.Error: %v", err)
	}
	return e.Code
}

// A principle is added, never rewritten; retiring one shows every T standing
// on it before the owner pays the price (D-7 R2.1(d), owner 19/9).
func TestPrinciple_AddOnlyAndRetireShowsWhatFalls(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()

	p, err := r.AddPrinciple(ctx, a, "p1", "Strong architecture absorbs weak execution.", "depending on people instead of structure is a violation")
	if err != nil || p.Code != "P.1" || p.Status != "active" || p.Note == nil {
		t.Fatalf("add: %+v %v", p, err)
	}
	if _, err := r.AddPrinciple(ctx, a, "P.1", "gì đó khác", ""); code(t, err) != "PRINCIPLE_EXISTS" {
		t.Fatalf("a principle is never rewritten: %v", err)
	}
	if _, err := r.AddPrinciple(ctx, a, "P.x", "x", ""); code(t, err) != "INVALID_CODE" {
		t.Fatalf("bad code: %v", err)
	}
	if _, err := r.AddPrinciple(ctx, a, "9", "Principle and Facts Above Assumption.", ""); err != nil {
		t.Fatal(err)
	}

	res, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Cause channel đứng ngoài đường ghi",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{"P.1", "P.9"},
			Derivation: "P.9 ⟹ mọi thứ ngoài nguyên tắc và fact có nguồn đều phải kiểm; " +
				"P.1 ⟹ việc kiểm không được dựa vào người mà phải nằm trong cấu trúc " +
				"⟹ kênh kiểm phải đứng ngoài đường ghi"},
	})
	if err != nil || res.Entry == nil {
		t.Fatalf("admit on P.1: %+v %v", res, err)
	}

	retired, affected, err := r.RetirePrinciple(ctx, a, "P.1", "đổi cách phát biểu")
	if err != nil || retired.Status != "retired" || len(affected) != 1 || affected[0] != res.Entry.ID {
		t.Fatalf("retire: %+v %v %v", retired, affected, err)
	}
	active, err := r.Principles(ctx, "", false)
	if err != nil || len(active) != 1 || active[0].Code != "P.9" {
		t.Fatalf("active list: %+v %v", active, err)
	}
	all, err := r.Principles(ctx, "", true)
	if err != nil || len(all) != 2 || all[0].Code != "P.1" {
		t.Fatalf("including retired, in numeric order: %+v %v", all, err)
	}
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "gì đó mới",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{"P.1", "P.9"}, Derivation: "P.1 + P.9 ⟹ x"}}); code(t, err) != "PRINCIPLE_RETIRED" {
		t.Fatalf("a claim cannot rest on a retired principle: %v", err)
	}
}

// Four kinds of A, each with the resolve path it must declare; the store
// refuses to write blind next to a claim that is already there.
func TestAssume_TailsDuplicateSimilarAndUpdate(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()

	if _, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Mira gửi invite trước ngày 20/9/2026", Type: "A-p", Source: "owner"}); code(t, err) != "MISSING_TAIL" {
		t.Fatalf("A-p without if/not: %v", err)
	}
	if _, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "x", Type: "A-r", Source: "ai", Via: "amendment"}); code(t, err) != "INVALID_TAIL" {
		t.Fatalf("A-r has no tail: %v", err)
	}
	if _, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "x", Type: "A-p", Source: "nghe nói", If: "a", Not: "b"}); code(t, err) != "INVALID_SOURCE" {
		t.Fatalf("source enum: %v", err)
	}

	first, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "Mira gửi invite trước ngày 26/9/2026", Type: "A-p", Source: "owner",
		If: "nhận được email invite", Not: "hết 26/9/2026 không có gì", ChatID: "c1",
	})
	if err != nil || first.Entry == nil || first.Entry.ID != "A-1" || first.Entry.Status != "open" ||
		first.Entry.Signer != "bao" || first.Entry.Producer != "claude-desktop" || first.Entry.Revision != 1 {
		t.Fatalf("assume: %+v %v", first.Entry, err)
	}

	again, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "  Mira gửi invite trước ngày 26/9/2026.  ", Type: "A-p", Source: "owner", If: "x", Not: "y",
	})
	if err != nil || again.Entry == nil || again.Entry.ID != first.Entry.ID || len(again.Entry.History) != 1 {
		t.Fatalf("same claim, nothing written: %+v %v", again.Entry, err)
	}
	if _, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "Mira gửi invite trước ngày 26/9/2026", Type: "A-c", Source: "owner", Via: "amendment",
	}); code(t, err) != "TYPE_CONFLICT" {
		t.Fatalf("one claim, one resolve path: %v", err)
	}

	// A claim one word apart is not close enough at the owner's 0.90 — the
	// gate only stops what is nearly the same sentence.
	if apart, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "Mira xác nhận lịch họp trước ngày 26/9/2026", Type: "A-p", Source: "owner", If: "x", Not: "y",
	}); err != nil || apart.Entry == nil || len(apart.Similar) != 0 {
		t.Fatalf("0.90 lets a different sentence through: %+v %v", apart, err)
	}

	// close enough → nothing written, the neighbours come back instead
	loose := &tka.Repo{Pool: r.Pool, Threshold: 0.5}
	near, err := loose.Assume(ctx, a, tka.AssumeInput{
		Claim: "Mira gửi invite trước 26/9", Type: "A-p", Source: "owner", If: "x", Not: "y",
	})
	if err != nil || near.Entry != nil || len(near.Similar) == 0 || near.Similar[0].ID != first.Entry.ID ||
		near.Similar[0].Status != "open" || near.Similar[0].Table != "assumption" {
		t.Fatalf("similar should block: %+v %v", near, err)
	}
	forced, err := loose.Assume(ctx, a, tka.AssumeInput{
		Claim: "Mira gửi invite trước 26/9", Type: "A-p", Source: "owner", If: "x", Not: "y", ConfirmNew: true,
	})
	if err != nil || forced.Entry == nil || forced.Entry.ID == first.Entry.ID ||
		forced.Entry.History[0].Note == nil || *forced.Entry.History[0].Note != "confirm_new" {
		t.Fatalf("confirm_new writes and says so: %+v %v", forced.Entry, err)
	}

	// a rewrite — the same claim said longer — is caught by containment, not by
	// the score, which drops as words are added
	long := "Mira gửi invite trước ngày 26/9/2026 (bản viết lại cho gọn hơn một chút)"
	rewrite, err := r.Assume(ctx, a, tka.AssumeInput{Claim: long, Type: "A-p", Source: "owner", If: "x", Not: "y"})
	if err != nil || rewrite.Entry != nil || len(rewrite.Similar) == 0 ||
		rewrite.Similar[0].ID != first.Entry.ID || rewrite.Similar[0].Reason != "contains" {
		t.Fatalf("a rewrite should be caught: %+v %v", rewrite, err)
	}

	// the owner edits an open assumption; the old wording stays in history
	newClaim := "Mira gửi invite trước ngày 30/9/2026"
	ifObs, notObs := "nhận được email invite", "hết 30/9/2026 không có gì"
	upd, err := r.UpdateAssumption(ctx, a, tka.UpdateInput{
		AssumptionID: first.Entry.ID, Claim: &newClaim, If: &ifObs, Not: &notObs, ConfirmNew: true,
	})
	if err != nil || upd.Entry == nil || upd.Entry.Revision != 2 || upd.Entry.Claim != newClaim {
		t.Fatalf("update: %+v %v", upd.Entry, err)
	}
	last := upd.Entry.History[len(upd.Entry.History)-1]
	if last.Event != "updated" || last.Note == nil || !strings.Contains(*last.Note, "Mira gửi invite trước ngày 26/9/2026") {
		t.Fatalf("history keeps the old wording: %+v", last)
	}
}

// The gate: a fact needs a source, a derivation needs premises that stand, and
// once inside the ledger nothing edits a claim.
func TestGate_AdmitPromoteAndImmutability(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	if _, err := r.AddPrinciple(ctx, a, "P.1", "Strong architecture absorbs weak execution.", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Postgres 16 có logical replication",
		BasisInput: tka.BasisInput{Basis: "fact"}}); code(t, err) != "NO_SOURCE" {
		t.Fatalf("a fact needs a source: %v", err)
	}
	fact, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Postgres 16 có logical replication",
		BasisInput: onDoc(t, r, a, "https://www.postgresql.org/docs/16/logical-replication.html",
			"PostgreSQL 16 — Chapter 31. Logical Replication")})
	if err != nil || fact.Entry == nil || fact.Entry.ID != "T-1" || fact.Entry.Status != "T" ||
		fact.Entry.VerifiedAt.IsZero() || fact.Entry.Source == nil {
		t.Fatalf("admit fact: %+v %v", fact.Entry, err)
	}

	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "x",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{"T-9999", "P.1"}, Derivation: "chuỗi"}}); code(t, err) != "PREMISE_NOT_FOUND" {
		t.Fatalf("unknown premise: %v", err)
	}
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "x",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{"P.7", "P.1"}, Derivation: "chuỗi"}}); code(t, err) != "PRINCIPLE_UNKNOWN" {
		t.Fatalf("unknown principle: %v", err)
	}
	derived, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "tk-cdc đọc được mọi thay đổi của sổ",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{fact.Entry.ID, "P.1"},
			Derivation: fact.Entry.ID + " ⟹ relay đọc được WAL; P.1 ⟹ đường đọc phải là cấu trúc " +
				"chứ không phải thao tác tay ⟹ mọi thay đổi của sổ ra stream"}})
	if err != nil || derived.Entry == nil || len(derived.Entry.Premises) != 2 || derived.Entry.Premises[0] != fact.Entry.ID {
		t.Fatalf("admit derivation: %+v %v", derived.Entry, err)
	}

	// an open assumption holding the same claim routes to promote
	ass, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "Mira gửi invite trước ngày 26/9/2026", Type: "A-p", Source: "owner",
		If: "nhận được email invite", Not: "hết 26/9/2026 không có gì",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Mira gửi invite trước ngày 26/9/2026",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "email 21/9"}}); code(t, err) != "USE_PROMOTE" {
		t.Fatalf("open assumption → promote: %v", err)
	}

	blocked, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "Bao kiểm từng T chứ không đóng dấu rỗng", Type: "A-b", Source: "ai",
		Via: "kênh audit độc lập với owner", ConfirmNew: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Promote(ctx, a, tka.PromoteInput{AssumptionID: blocked.Entry.ID,
		BasisInput: tka.BasisInput{Basis: "fact", Source: "tự thấy"}}); code(t, err) != "CHANNEL_BLOCKED" {
		t.Fatalf("A-b cannot be promoted: %v", err)
	}

	// An email has no public link, so the evidence goes into the store itself
	// and the fact cites it by id.
	mail, err := r.AddDocument(ctx, a, tka.DocumentInput{
		Filename: "mira-21-09.eml", TextProducer: "owner", Method: "opened_source",
		Text:      "From: mira\nDate: 2026-09-21\nSubject: invite\n\nInvite đã gửi.\n",
		SourceURI: "file:///mail/mira-21-09.eml",
	})
	// D-1 is the documentation page the first fact rests on.
	if err != nil || mail.ID != "D-2" || !mail.Bound {
		t.Fatalf("store the email: %+v %v", mail, err)
	}
	got, err := r.Promote(ctx, a, tka.PromoteInput{AssumptionID: ass.Entry.ID,
		BasisInput: tka.BasisInput{Basis: "fact", Source: "email 2026-09-21 từ mira", SourceDoc: mail.ID}})
	if err != nil || got.Entry.Claim != ass.Entry.Claim || got.Entry.PromotedFrom == nil ||
		*got.Entry.PromotedFrom != ass.Entry.ID || got.Assumption.Status != "promoted" {
		t.Fatalf("promote: %+v %v", got, err)
	}
	if _, err := r.UpdateAssumption(ctx, a, tka.UpdateInput{AssumptionID: ass.Entry.ID, Claim: &got.Entry.Claim}); code(t, err) != "NOT_OPEN" {
		t.Fatalf("a promoted assumption is part of the record: %v", err)
	}

	// the ledger refuses edits even to the database superuser
	for _, q := range []string{
		`UPDATE truth_ledger SET claim = 'khác' WHERE id = 'T-1'`,
		`DELETE FROM truth_ledger WHERE id = 'T-1'`,
		`DELETE FROM assumption WHERE id = 'A-1'`,
		`UPDATE principle SET statement = 'khác' WHERE code = 'P.1'`,
	} {
		if _, err := r.Pool.Exec(ctx, q); err == nil {
			t.Fatalf("append-only should refuse: %s", q)
		}
	}
}

// Reading the store back, and the three acts that lower something. A premise
// that falls does not silently take its consequences down — it flags them.
func TestRead_ChainDemoteRetireRecheck(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()

	if _, err := r.AddPrinciple(ctx, a, "P.1", "Strong architecture absorbs weak execution.", ""); err != nil {
		t.Fatal(err)
	}
	fact, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "tk-cdc đọc WAL của Postgres",
		BasisInput: onDoc(t, r, a, "https://www.postgresql.org/docs/16/logical-replication.html",
			"Logical replication reads the write-ahead log")})
	if err != nil {
		t.Fatal(err)
	}
	derived, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Mọi thay đổi của sổ đều ra ngoài được",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{"P.1", fact.Entry.ID},
			Derivation: "P.1 + " + fact.Entry.ID + " ⟹ kênh đọc nằm ngoài đường ghi"}})
	if err != nil {
		t.Fatal(err)
	}

	// read one entry by id, and the chain under it
	if e, err := r.Read(ctx, "", derived.Entry.ID); err != nil || e.Kind != "truth" || e.Truth.Effective != "T" {
		t.Fatalf("read: %+v %v", e, err)
	}
	top, nodes, err := r.Chain(ctx, "", derived.Entry.ID)
	if err != nil || top.ID != derived.Entry.ID || len(nodes) != 2 ||
		nodes[0].Kind != "principle" || !nodes[0].Stands || nodes[1].ID != fact.Entry.ID || !nodes[1].Stands {
		t.Fatalf("chain: %+v %v", nodes, err)
	}

	// the fact is contradicted from outside — P.9's other channel
	demoted, err := r.Demote(ctx, a, fact.Entry.ID, "trang doc đổi, giới hạn không còn đúng", "postgresql.org/docs/17")
	if err != nil || demoted.Status != "A" || demoted.Claim != fact.Entry.Claim {
		t.Fatalf("demote: %+v %v", demoted, err)
	}
	events := demoted.History
	if len(events) != 3 || events[1].Event != "contradicted_by" || events[2].Event != "demoted" {
		t.Fatalf("history of a demotion: %+v", events)
	}

	// what stood on it is flagged, not lowered by the machine
	after, err := r.ReadTruth(ctx, nil, "", derived.Entry.ID)
	if err != nil || after.Status != "T" || after.Effective != "needs_review" ||
		len(after.PremisesFell) != 1 || after.PremisesFell[0] != fact.Entry.ID {
		t.Fatalf("premise fell: %+v %v", after, err)
	}
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "claim mới trên tiền đề đã đổ",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{fact.Entry.ID, "P.1"}, Derivation: "x + P.1 ⟹ y"}}); code(t, err) != "PREMISE_NOT_T" {
		t.Fatalf("a fallen premise cannot carry a new claim: %v", err)
	}
	if list, err := r.Truths(ctx, "", true, 0); err != nil || len(list) != 0 {
		t.Fatalf("standing list is empty once both entries are shaky: %+v %v", list, err)
	}

	// recheck belongs to facts; a derivation is re-derived instead
	if _, err := r.Recheck(ctx, a, derived.Entry.ID, "x"); code(t, err) != "NOT_A_FACT" {
		t.Fatalf("recheck on a derivation: %v", err)
	}
	if _, err := r.Recheck(ctx, a, fact.Entry.ID, "x"); code(t, err) != "NOT_T" {
		t.Fatalf("recheck on a demoted entry: %v", err)
	}

	// retire an assumption; it stays readable
	ass, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Mira sẽ trả lời trong tuần", Type: "A-p",
		Source: "owner", If: "có email", Not: "hết tuần im lặng"})
	if err != nil {
		t.Fatal(err)
	}
	if entry, _, err := r.Retire(ctx, a, ass.Entry.ID, "việc này đóng rồi"); err != nil ||
		entry.Assumption.Status != "retired" || entry.Assumption.Claim != ass.Entry.Claim {
		t.Fatalf("retire: %+v %v", entry, err)
	}
	if _, _, err := r.Retire(ctx, a, ass.Entry.ID, "lần hai"); code(t, err) != "ALREADY_RETIRED" {
		t.Fatalf("retire twice: %v", err)
	}
	if _, _, err := r.Retire(ctx, a, "P.1", "x"); code(t, err) != "USE_PRINCIPLE_RETIRE" {
		t.Fatalf("a principle has its own door: %v", err)
	}
	if open, err := r.OpenAssumptions(ctx, "", ""); err != nil || len(open) != 0 {
		t.Fatalf("nothing open left: %+v %v", open, err)
	}
}

// Two guards the owner decided on 25/9: a machine never signs, and rewording an
// assumption forces its way out to be restated.
func TestGuards_AgentSignerAndRestatedTail(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	r.Agents = map[string]bool{"claude-desktop": true}

	machine := tka.Actor{UserID: "claude-desktop", ActorID: "claude-desktop", GrantID: "gr_x"}
	if _, err := r.Assume(ctx, machine, tka.AssumeInput{Claim: "máy tự ký", Type: "A-r", Source: "ai"}); code(t, err) != "SIGNER_IS_AGENT" {
		t.Fatalf("an agent may produce, never sign: %v", err)
	}
	if _, err := r.Admit(ctx, machine, tka.AdmitInput{Claim: "máy tự ký",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "x"}}); code(t, err) != "SIGNER_IS_AGENT" {
		t.Fatalf("admit signed by a machine: %v", err)
	}

	first, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Mira gửi invite trước 26/9", Type: "A-p",
		Source: "owner", If: "nhận được email invite", Not: "hết 26/9 không có gì"})
	if err != nil {
		t.Fatal(err)
	}
	moved := "Mira gửi invite trước 30/9"
	if _, err := r.UpdateAssumption(ctx, a, tka.UpdateInput{AssumptionID: first.Entry.ID, Claim: &moved}); code(t, err) != "RESTATE_TAIL" {
		t.Fatalf("a new claim needs its if/not restated: %v", err)
	}
	notObs := "hết 30/9 không có gì"
	ifObs := "nhận được email invite"
	upd, err := r.UpdateAssumption(ctx, a, tka.UpdateInput{AssumptionID: first.Entry.ID,
		Claim: &moved, If: &ifObs, Not: &notObs, ConfirmNew: true})
	if err != nil || upd.Entry == nil || *upd.Entry.Not != notObs || upd.Entry.Revision != 2 {
		t.Fatalf("restated: %+v %v", upd.Entry, err)
	}
}

// Two tenants in the same DB: same principle code, same wording, same id number
// — and neither sees the other. This is the pooled shape; siloed is this same
// code with one tenant in its own DB.
func TestTenant_IsolatedAndNumberedPerTenant(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	acme := tka.Actor{UserID: "bao", ActorID: "claude", GrantID: "g1", TenantID: "acme"}
	globex := tka.Actor{UserID: "lan", ActorID: "claude", GrantID: "g2", TenantID: "globex"}

	// the same principle code, two tenants, no conflict
	if _, err := r.AddPrinciple(ctx, acme, "P.1", "Acme: cấu trúc hấp thụ lỗi thi hành", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddPrinciple(ctx, globex, "P.1", "Globex: nguyên tắc khác hẳn", ""); err != nil {
		t.Fatalf("code của tenant khác không phải xung đột: %v", err)
	}

	// each side rests on its own document; one original in two stores is two documents
	one, err := r.Admit(ctx, acme, tka.AdmitInput{Claim: "Hợp đồng gia hạn trong quý 4/2026",
		BasisInput: onDoc(t, r, acme, "https://example.com/contract", "hợp đồng đã ký, trang 1")})
	if err != nil || one.Entry.ID != "T-1" {
		t.Fatalf("acme: %+v %v", one.Entry, err)
	}
	// identical wording, different tenant: not blocked as a duplicate, and ids also start at 1
	two, err := r.Admit(ctx, globex, tka.AdmitInput{Claim: "Hợp đồng gia hạn trong quý 4/2026",
		BasisInput: onDoc(t, r, globex, "https://example.com/contract", "hợp đồng đã ký, trang 1")})
	if err != nil || two.Entry == nil || two.Entry.ID != "T-1" || len(two.Similar) != 0 {
		t.Fatalf("globex đánh số riêng và không thấy claim của acme: %+v %v", two, err)
	}

	// reads do not cross tenants
	if e, err := r.Read(ctx, "acme", "T-1"); err != nil || e.Truth.Signer != "bao" {
		t.Fatalf("acme đọc T-1 của mình: %+v %v", e, err)
	}
	if e, err := r.Read(ctx, "globex", "T-1"); err != nil || e.Truth.Signer != "lan" {
		t.Fatalf("globex đọc T-1 của mình: %+v %v", e, err)
	}
	if list, err := r.Truths(ctx, "acme", false, 0); err != nil || len(list) != 1 || list[0].Signer != "bao" {
		t.Fatalf("sổ của acme chỉ có dòng của acme: %+v %v", list, err)
	}
	if ps, err := r.Principles(ctx, "globex", false); err != nil || len(ps) != 1 ||
		!strings.HasPrefix(ps[0].Statement, "Globex") {
		t.Fatalf("Principle List của globex: %+v %v", ps, err)
	}

	// a third tenant with nothing yet reads as empty, not as an error
	if list, err := r.Truths(ctx, "chua-co", false, 0); err != nil || len(list) != 0 {
		t.Fatalf("kho mới thì trống: %+v %v", list, err)
	}
	if _, err := r.Read(ctx, "chua-co", "T-1"); code(t, err) != "NOT_FOUND" {
		t.Fatalf("id của tenant khác không đọc được: %v", err)
	}
}

// An id is the row's own number, unpadded (owner decision, 1/10): T-100 is row 100 of
// this store's ledger. The padded form broke at 10000 — lpad cut "10000" to
// "1000" and the write collided with T-1000. An id typed the old way still finds
// its entry, and is stored in the one spelling.
func TestIDs_PlainNumberPast9999(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := testapi.StartPostgres(t)
	r, a := tka.New(pool), tka.Actor{UserID: "bao", ActorID: "claude-desktop", GrantID: "gr_test"}
	jump := func(next int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE tenant_counter SET next = $1 WHERE tenant_id = 'default' AND kind = 'T'`, next); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(claim string) string {
		t.Helper()
		res, err := r.Admit(ctx, a, tka.AdmitInput{Claim: claim,
			BasisInput: onDoc(t, r, a, "https://example.com/"+claim[:3], claim)})
		if err != nil || res.Entry == nil {
			t.Fatalf("admit %q: %+v %v", claim, res, err)
		}
		return res.Entry.ID
	}

	ids := []string{admit("Acme ký hợp đồng ngày 2026-01-05")}
	jump(1000)
	ids = append(ids, admit("Globex mở văn phòng Dubai ngày 2026-02-11"))
	jump(9999)
	ids = append(ids, admit("Initech tuyển kiến trúc sư ngày 2026-03-20"), admit("Umbrella gửi offer ngày 2026-04-02"))
	if strings.Join(ids, " ") != "T-1 T-1000 T-9999 T-10000" {
		t.Fatalf("ids: %v", ids)
	}

	for typed, want := range map[string]string{"t-0001": "T-1", " T-01000 ": "T-1000", "T-010000": "T-10000"} {
		if e, err := r.Read(ctx, "default", typed); err != nil || e.Truth.ID != want {
			t.Fatalf("Read(%q): %+v %v", typed, e.Truth, err)
		}
	}
	d, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Hai hợp đồng đầu năm 2026 đều đã ký", BasisInput: tka.BasisInput{
		Basis: "derivation", Premises: []string{"T-0001", "T-01000"}, Derivation: "T-1 ∧ T-1000 ⟹ cả hai đã ký"}})
	if err != nil || d.Entry == nil || strings.Join(d.Entry.Premises, " ") != "T-1 T-1000" {
		t.Fatalf("premises stored in the one spelling: %+v %v", d.Entry, err)
	}
}

func TestCanonID(t *testing.T) {
	for in, want := range map[string]string{
		"T-7": "T-7", "t-0007": "T-7", " A-012 ": "A-12", "D-10000": "D-10000", "T-0": "T-0",
		"P.3": "P.3", "clm_x": "CLM_X", "T-": "T-",
	} {
		if got := tka.CanonID(in); got != want {
			t.Fatalf("CanonID(%q) = %q, want %q", in, got, want)
		}
	}
}
