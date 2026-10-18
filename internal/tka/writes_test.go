package tka_test

import (
	"context"
	"testing"

	"golang.org/x/text/unicode/norm"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// A trial store lives forever and takes a fixed number of writes, then reads
// only (owner 3/10/2026). A write is one successful move: it is charged once
// however many guards the move runs, a refused move costs nothing, and at the
// cap the next write is refused while every read keeps working.
func TestWrites_TrialCapOncePerMoveReadsAfter(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	cap := 4
	if _, err := r.CreateTenant(ctx, tka.Tenant{ID: "capped", Tier: "trial"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetWriteCap(ctx, "capped", cap); err != nil {
		t.Fatal(err)
	}
	a := base
	a.TenantID = "capped"
	writes := func() int64 {
		t.Helper()
		ten, err := r.ReadTenant(ctx, "capped")
		if err != nil {
			t.Fatal(err)
		}
		return ten.Writes
	}

	// 1: a principle.
	if _, err := r.AddPrinciple(ctx, a, "P.20", "Small moves, each one checkable.", ""); err != nil {
		t.Fatal(err)
	}
	// A refused move costs nothing: the code is taken.
	if _, err := r.AddPrinciple(ctx, a, "P.20", "Something else.", ""); code(t, err) != "PRINCIPLE_EXISTS" {
		t.Fatalf("taken code: %v", err)
	}
	if n := writes(); n != 1 {
		t.Fatalf("a refused write must not be counted: %d", n)
	}
	// 2: a thread. 3: a document.
	th, err := r.OpenThread(ctx, a, tka.ThreadOpenInput{Title: "Renewal", Keywords: []string{"acme"}})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := r.AddDocument(ctx, a, tka.DocumentInput{
		Text: "Acme renewal email, 2026-10-02: the contract is renewed.", TextProducer: "owner", Method: "read_text",
		SourceURI: "https://example.com/acme-renewal"})
	if err != nil {
		t.Fatal(err)
	}
	// 4: admit linked to the thread — two guards (admits, then the link) in one
	// transaction, one write.
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Acme renewed its contract on 2026-10-02",
		BasisInput: tka.BasisInput{Basis: "fact", SourceDoc: doc.ID, Where: "line 1"}, Thread: th.ID}); err != nil {
		t.Fatal(err)
	}
	if n := writes(); n != int64(cap) {
		t.Fatalf("four moves, four writes: %d", n)
	}

	// Two guards in one transaction are one write; a rolled-back one is none.
	if err := r.SetWriteCap(ctx, "capped", cap+2); err != nil {
		t.Fatal(err)
	}
	for _, commit := range []bool{true, false} {
		tx, err := r.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit; frees the connection on a failed check
		if err := tka.SetTenant(ctx, tx, "capped"); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := r.Writable(ctx, tx, a); err != nil {
				t.Fatal(err)
			}
		}
		if commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := writes(); n != int64(cap+1) {
		t.Fatalf("two guards in one committed transaction count once, a rolled-back one not at all: %d", n)
	}
	if err := r.SetWriteCap(ctx, "capped", cap+1); err != nil {
		t.Fatal(err)
	}
	cap++

	// At the cap: the next write is refused, and the refusal does not move the count.
	if _, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "Acme renews again before 2027-10-02",
		Type: "A-p", Source: "owner", If: "a renewal email", Not: "2027-10-02 passes"}); code(t, err) != "WRITE_LIMIT_REACHED" {
		t.Fatalf("over the cap: %v", err)
	}
	if n := writes(); n != int64(cap) {
		t.Fatalf("a refused write over the cap must not count: %d", n)
	}
	// Reads keep working.
	if ps, err := r.Principles(ctx, "capped", false); err != nil || len(ps) != 1 {
		t.Fatalf("reads after the cap: %v %v", ps, err)
	}
	if list, err := r.Truths(ctx, "capped", false, 0); err != nil || len(list) != 1 {
		t.Fatalf("ledger after the cap: %v %v", list, err)
	}
	if ten, _ := r.ReadTenant(ctx, "capped"); ten.WritesLeft() == nil || *ten.WritesLeft() != 0 {
		t.Fatalf("writes left: %+v", ten)
	}
}

// A store with no cap (paid, or a single-tenant deployment) is never counted.
func TestWrites_UncappedStoreNotCounted(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	if _, err := r.CreateTenant(ctx, tka.Tenant{ID: "paid", Tier: "paid"}); err != nil {
		t.Fatal(err)
	}
	a := base
	a.TenantID = "paid"
	for i, code := range []string{"P.31", "P.32", "P.33"} {
		if _, err := r.AddPrinciple(ctx, a, code, "Principle number "+code+".", ""); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if ten, err := r.ReadTenant(ctx, "paid"); err != nil || ten.Writes != 0 || ten.MaxWrites != nil || ten.WritesLeft() != nil {
		t.Fatalf("an uncapped store keeps no count: %+v %v", ten, err)
	}
}

// The same sentence typed precomposed and pasted decomposed is one claim:
// same key, same length, and the second assume finds the first.
func TestClaims_NFCDuplicateAcrossForms(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	vi := "Nguyễn Văn Bảo phỏng vấn vòng ba ngày 2026-10-02"
	nfc, nfd := norm.NFC.String(vi), norm.NFD.String(vi)
	if nfc == nfd {
		t.Fatal("test sample must differ between forms")
	}
	first, err := r.Assume(ctx, a, tka.AssumeInput{Claim: nfc, Type: "A-r", Source: "owner"})
	if err != nil || first.Entry == nil {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := r.Assume(ctx, a, tka.AssumeInput{Claim: nfd, Type: "A-r", Source: "owner"})
	if err != nil || second.Entry == nil || second.Entry.ID != first.Entry.ID {
		t.Fatalf("the decomposed copy must be the same claim: %+v %v", second, err)
	}
	if first.Entry.Claim != nfc {
		t.Fatalf("stored claim must be NFC: %q", first.Entry.Claim)
	}
}

func TestClaims_KeyLengthAcrossLanguages(t *testing.T) {
	for _, s := range []string{
		"Nguyễn Văn Bảo phỏng vấn vòng ba ngày 2026-10-02",
		"애크미는 2026년 10월 2일 계약을 갱신했다",
	} {
		nfc, nfd := norm.NFC.String(s), norm.NFD.String(s)
		if tka.ClaimKey(nfc) != tka.ClaimKey(nfd) || tka.ClaimLength(nfc) != tka.ClaimLength(nfd) {
			t.Fatalf("%q: key or length differs between NFC and NFD", s)
		}
	}
	for s, want := range map[string]int{"👩‍👩‍👧‍👦": 1, "🇻🇳": 1, "阿克米公司于2026年10月2日续签了合同": 21} {
		if got := tka.ClaimLength(s); got != want {
			t.Fatalf("ClaimLength(%q) = %d, want %d", s, got, want)
		}
	}
	// Scripts with combining marks are counted by what a reader sees, which is
	// fewer than their code points (exact clusters depend on the Unicode version).
	for _, s := range []string{"एक्मे ने अनुबंध का नवीनीकरण किया", "แอคมีต่อสัญญาแล้ว"} {
		if got, runes := tka.ClaimLength(s), len([]rune(s)); got >= runes {
			t.Fatalf("ClaimLength(%q) = %d, must be below its %d code points", s, got, runes)
		}
	}
	// Full-width sentence ends are taken off the key like ASCII ones.
	if tka.ClaimKey("阿克米续签了合同。") != tka.ClaimKey("阿克米续签了合同") {
		t.Fatal("。 must not change the key")
	}
}

// Chinese and Japanese put no space after 。！？, so two sentences run together
// are still two claims.
func TestCheckClaim_CJKSentenceBreak(t *testing.T) {
	if c := tka.CheckClaim("阿克米于2026年10月2日续签了合同。贝塔于2026年10月3日退出了。", ""); c.Admissible {
		t.Fatal("two Chinese sentences must not pass as one claim")
	}
	if c := tka.CheckClaim("阿克米于2026年10月2日续签了合同。", ""); !c.Admissible {
		t.Fatalf("one sentence with its full stop is one claim: %+v", c.Problems)
	}
	if c := tka.CheckClaim("他说「阿克米于2026年10月2日续签了合同。」", ""); !c.Admissible {
		t.Fatalf("a full stop before a closing quote ends the same sentence: %+v", c.Problems)
	}
}
