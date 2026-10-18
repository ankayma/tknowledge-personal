package tka_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// The two sentences the owner sent back on 28/9, and what the store now says
// about them. Both fail the same condition: they lean on the conversation they
// came from.
func TestCheckClaim_TheOwnersTwoCorrections(t *testing.T) {
	role := tka.CheckClaim("Owner là co-founder và cổ đông Halden", "fact")
	if role.Admissible || len(role.Problems) != 1 {
		t.Fatalf("a role word standing in for a person: %+v", role)
	}
	if p := role.Problems[0]; p.Condition != "decontextualized" || p.Found != "owner" ||
		p.Why == "" || p.Fix == "" {
		t.Fatalf("the reason must name the words and say what to write: %+v", p)
	}

	when := tka.CheckClaim("Halden là pháp nhân UAE vừa mới mở", "fact")
	if when.Admissible || when.Problems[0].Condition != "decontextualized" {
		t.Fatalf("a time said relative to the reader: %+v", when)
	}

	// Rewritten the way the owner rewrote them.
	for _, claim := range []string{
		"Lan Nguyen (founder) là cổ đông của Halden Ltd",
		"Halden là pháp nhân UAE từ 09/2026",
	} {
		if got := tka.CheckClaim(claim, "fact"); !got.Admissible {
			t.Errorf("%q should pass: %+v", claim, got.Problems)
		}
	}
}

// A check that returns only true/false tells a person nothing they can act on,
// so every problem carries the words found, why, and what to write instead —
// and what no machine decides is listed rather than hidden (P.3).
func TestCheckClaim_AnswersWithReasonsAndNamesWhatItCannotDecide(t *testing.T) {
	got := tka.CheckClaim("Owner sẽ chốt việc này. Quan trọng nhất là tốc độ.", "fact")
	if got.Admissible {
		t.Fatal("this is three problems at once, not a claim")
	}
	conditions := map[string]bool{}
	for _, p := range got.Problems {
		conditions[p.Condition] = true
		if p.Why == "" || p.Fix == "" {
			t.Errorf("a problem with no reason or no fix: %+v", p)
		}
	}
	for _, want := range []string{"atomic", "decontextualized", "checkable"} {
		if !conditions[want] {
			t.Errorf("expected a %s problem in %+v", want, got.Problems)
		}
	}
	if len(got.SignerChecks) == 0 {
		t.Fatal("what the machine cannot decide must be said, not hidden")
	}
	if !strings.Contains(strings.Join(got.SignerChecks, " "), "checkable") {
		t.Errorf("the signer is owed the checkable question: %+v", got.SignerChecks)
	}
}

// Basis narrows the third condition: an assumption is allowed to be about the
// future — that is what A-p is for — and a derivation from principles is allowed
// to be normative, since that is what deriving from a principle produces.
func TestCheckClaim_BasisNarrowsCheckable(t *testing.T) {
	future := "Acme renews the contract before 2026-12-31"
	if got := tka.CheckClaim(future, ""); !got.Admissible {
		t.Errorf("an assumption may be about the future: %+v", got.Problems)
	}
	if got := tka.CheckClaim("Acme will renew the contract before 2026-12-31", "fact"); got.Admissible {
		t.Error("a fact about the future has nothing to cite yet")
	}
	if got := tka.CheckClaim("Kênh kiểm nên đứng ngoài đường ghi", "derivation"); !got.Admissible {
		t.Errorf("a derivation from principles may be normative: %+v", got.Problems)
	}
	if got := tka.CheckClaim("Kênh kiểm nên đứng ngoài đường ghi", "fact"); got.Admissible {
		t.Error("a fact cannot be an evaluation")
	}
}

// The word list matches on word boundaries: "sắp" must not fire inside another
// word, and a claim that happens to contain the letters of a stand-in is fine.
func TestCheckClaim_MatchesWholeWordsOnly(t *testing.T) {
	for _, claim := range []string{
		"Sắp xếp kho theo thứ tự chữ cái là quy ước của Halden từ 2026",
		"Business Bay là địa chỉ đăng ký của Halden Ltd từ 01/2026",
	} {
		if got := tka.CheckClaim(claim, "fact"); !got.Admissible {
			t.Errorf("%q should pass: %+v", claim, got.Problems)
		}
	}
	if got := tka.CheckClaim("Halden sắp tới sẽ mở chi nhánh thứ hai", "fact"); got.Admissible {
		t.Error("a relative time marker must be caught")
	}
	// The honest limit: bare "sắp" and bare "vừa" are ambiguous in Vietnamese, so
	// the machine lets them through and says so in signer_checks.
	if got := tka.CheckClaim("Halden vừa mở chi nhánh thứ hai", "derivation"); !got.Admissible {
		t.Errorf("bare vừa is not machine-decidable, so it must not be refused: %+v", got.Problems)
	}
}

// T/A applies to claims, so the claim check comes before the store is consulted
// at all (owner 28/9). A full trial store must not answer TENANT_FULL to
// something that was never a claim: that answers a question nobody asked, and
// sends the person off to free up space instead of fixing the sentence.
func TestCheckClaim_RunsBeforeTheStoreIsConsulted(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	a := trial(t, r, tka.Tenant{MaxEntries: ptr(1), MaxClaim: ptr(200),
		ExpiresAt: ptr(time.Now().Add(time.Hour))})

	if _, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "Halden Ltd ships the order in October 2026", Type: "A-p", Source: "owner",
		If: "the shipment arrives", Not: "October 2026 ends with nothing delivered",
	}); err != nil {
		t.Fatal(err) // the store is now full
	}

	// Full store, and the sentence is not a claim. The answer is about the
	// sentence, and it comes back with a code like every other refusal.
	_, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "Owner sẽ chốt việc này", Type: "A-p", Source: "owner", If: "x", Not: "y"})
	if code(t, err) != "CLAIM_FAILED" {
		t.Fatalf("expected CLAIM_FAILED, got %v", err)
	}
	var e *tka.Error
	if !errors.As(err, &e) || e.Check == nil || len(e.Check.Problems) == 0 {
		t.Fatalf("the refusal must carry the reasons: %+v", e)
	}

	// Same for admit, and there the code matters even more: it must not be
	// SOURCE_NOT_RETRIEVABLE. Nothing about the basis is worth saying until the
	// sentence is a claim.
	_, err = r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Owner là cổ đông", BasisInput: tka.BasisInput{Basis: "fact", Source: "chat"}})
	if code(t, err) != "CLAIM_FAILED" {
		t.Fatalf("expected CLAIM_FAILED, got %v", err)
	}

	// And a well-formed claim in the same full store still gets the store's own
	// answer — the ordering changes which question is answered, not the limits.
	if _, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "Meridian Tooling ships the order in November 2026", Type: "A-p", Source: "owner",
		If: "the shipment arrives", Not: "November 2026 ends with nothing delivered",
	}); code(t, err) != "TENANT_FULL" {
		t.Fatalf("a real claim still meets the ceiling: %v", err)
	}
}
