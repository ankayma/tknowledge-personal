package tka_test

import (
	"context"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// A unit is not punctuation. Trimming every trailing symbol made "tăng 800%"
// and "tăng 800" ("up 800%", "up 800") one key, so a claim about 800 came back answered with the row
// about 800% — a wrong answer dressed as a duplicate, worse than a refusal.
func TestClaimKey_KeepsAUnit(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"Kalshi institutional volume tăng 800%", "Kalshi institutional volume tăng 800", false},
		{"Cổ phần của Bao là 49%", "Cổ phần của Bao là 49", false},
		{"Nhiệt độ phòng máy là 22°", "Nhiệt độ phòng máy là 22", false},
		// Sentence punctuation still goes: it changes nothing a claim asserts.
		{"Halden là pháp nhân UAE.", "Halden là pháp nhân UAE", true},
		{"Halden là pháp nhân UAE!?", "  halden là pháp nhân   uae  ", true},
	} {
		if got := tka.ClaimKey(c.a) == tka.ClaimKey(c.b); got != c.same {
			t.Errorf("%q vs %q: same key = %v, want %v (%q / %q)",
				c.a, c.b, got, c.same, tka.ClaimKey(c.a), tka.ClaimKey(c.b))
		}
	}
}

// Two claims differing only by a unit are two claims, and the store writes both
// rather than answering the second with the first.
func TestAdmit_AUnitMakesADifferentClaim(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	page := onDoc(t, r, a, "https://kalshi.com/", "search 22/9/2026: Kalshi institutional volume")
	page.SourceDate = "2026-09-22"
	fact := func(claim string) tka.AdmitInput {
		return tka.AdmitInput{Claim: claim, ConfirmNew: true, BasisInput: page}
	}
	first, err := r.Admit(ctx, a, fact("Kalshi institutional volume tăng 800%"))
	if err != nil || first.Entry == nil {
		t.Fatalf("first: %+v %v", first.Entry, err)
	}
	second, err := r.Admit(ctx, a, fact("Kalshi institutional volume tăng 800"))
	if err != nil || second.Entry == nil {
		t.Fatalf("second: %+v %v", second.Entry, err)
	}
	if second.Entry.ID == first.Entry.ID {
		t.Fatalf("800 was answered with the row about 800%%: both are %s", first.Entry.ID)
	}
}

// The way a person actually rewrites a claim is to drop a clause out of the
// middle. That is not a substring of anything, and trigram scoring put the real
// case at 0.507 — under the 0.90 that blocks. The store used to write it as a
// second row and end up holding one fact twice.
func TestAdmit_CatchesAClaimRestatedShorter(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	evidence := onDoc(t, r, a, "drive://halden/License.pdf", "Ajman Free Zone 10001")
	evidence.Where, evidence.SourceDate = "License.pdf trang 1", "2026-09-27"

	full := "Halden - F.Z.C là Free Zone Company - Limited Liability, giấy phép số 10001, Ajman Free Zone"
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: full, BasisInput: evidence}); err != nil {
		t.Fatal(err)
	}

	// Same fact, middle clause gone.
	shorter := "Halden - F.Z.C là Free Zone Company ở Ajman Free Zone"
	res, err := r.Admit(ctx, a, tka.AdmitInput{Claim: shorter, BasisInput: evidence})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry != nil {
		t.Fatalf("the restatement was written as a second row: %s", res.Entry.ID)
	}
	if len(res.Similar) != 1 || res.Similar[0].Reason != "restated" {
		t.Fatalf("expected one neighbour with reason restated, got %+v", res.Similar)
	}

	// The owner may still say it is a different claim, and then it is written.
	res, err = r.Admit(ctx, a, tka.AdmitInput{Claim: shorter, ConfirmNew: true, BasisInput: evidence})
	if err != nil || res.Entry == nil {
		t.Fatalf("confirm_new must still write: %+v %v", res.Entry, err)
	}
}

// A number is content. The restatement check will not fold two claims together
// when the shorter one asserts a number the longer one does not — a different
// licence number is a different claim, however much wording they share.
func TestAdmit_ADifferentNumberIsNotARestatement(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	evidence := onDoc(t, r, a, "drive://halden/License.pdf", "Ajman Free Zone")
	evidence.Where, evidence.SourceDate = "License.pdf trang 1", "2026-09-27"

	if _, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim:      "Halden - F.Z.C là Free Zone Company - Limited Liability, giấy phép số 10001, Ajman Free Zone",
		BasisInput: evidence}); err != nil {
		t.Fatal(err)
	}
	// Most of the wording is shared and it is much shorter, so coverage alone
	// would call it a restatement — but the licence number differs.
	res, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim:      "Halden - F.Z.C giấy phép số 77777, Ajman Free Zone",
		BasisInput: evidence})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Similar {
		if s.Reason == "restated" {
			t.Fatalf("a different licence number is a different claim: %+v", s)
		}
	}
	if res.Entry == nil && len(res.Similar) == 0 {
		t.Fatalf("nothing written and nothing said: %+v", res)
	}
}

// A claim and its opposite share nearly every word, so the coverage check would
// fold them into one "restated" — the one mistake here that actively misleads.
// A denial in one and not the other means they are different claims.
func TestAdmit_AClaimAndItsOppositeAreNotOneClaim(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	say := func(claim string) (*tka.Assumption, []tka.Similar) {
		t.Helper()
		res, err := r.Assume(ctx, a, tka.AssumeInput{
			Claim: claim, Type: "A-c", Source: "owner", Via: "dán vào brief call"})
		if err != nil {
			t.Fatalf("%q: %v", claim, err)
		}
		return res.Entry, res.Similar
	}
	first, _ := say("Trong call DD với Eagle's View thì trình bày method, phase 0, số đúng và giới hạn thẳng")
	if first == nil {
		t.Fatal("the first claim should be written")
	}
	second, similar := say("Trong call DD với Eagle's View thì không trình bày thesis")
	if second == nil {
		t.Fatalf("a denial is a different claim, not a restatement: %+v", similar)
	}
	if second.ID == first.ID {
		t.Fatal("the two were folded into one entry")
	}
}
