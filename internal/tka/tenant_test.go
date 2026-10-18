package tka_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// trial opens a store with terms on it and returns an actor writing into it.
func trial(t *testing.T, r *tka.Repo, terms tka.Tenant) tka.Actor {
	t.Helper()
	terms.ID = "trial-" + fmt.Sprint(time.Now().UnixNano())
	if _, err := r.CreateTenant(context.Background(), terms); err != nil {
		t.Fatalf("open store: %v", err)
	}
	return tka.Actor{UserID: "stranger", ActorID: "claude-desktop", GrantID: "gr_trial", TenantID: terms.ID}
}

func ptr[T any](v T) *T { return &v }

// A trial store holds a set number of claims. Reaching the ceiling refuses the
// next claim and says what is still possible; it never half-writes one.
func TestTrial_CeilingRefusesTheNextClaim(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	a := trial(t, r, tka.Tenant{MaxEntries: ptr(2), ExpiresAt: ptr(time.Now().Add(time.Hour))})

	for _, name := range []string{"Halden Ltd", "Meridian Tooling"} {
		res, err := r.Assume(ctx, a, tka.AssumeInput{
			Claim: name + " ships the order in October 2026", Type: "A-p", Source: "owner",
			If: "the shipment arrives", Not: "October 2026 ends with nothing delivered",
		})
		if err != nil || res.Entry == nil {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
	}
	_, err := r.Assume(ctx, a, tka.AssumeInput{
		Claim: "A third supplier exists in the 2026 list", Type: "A-p", Source: "owner",
		If: "we find one", Not: "we do not",
	})
	if code(t, err) != "TENANT_FULL" {
		t.Fatalf("a full trial store must refuse: %v", err)
	}
	var e *tka.Error
	if errors.As(err, &e) && e.Next == "" {
		t.Fatal("a refusal carries the move still open")
	}

	// The ceiling counts new claims. Settling one that is already written is
	// still allowed — a full store must not strand an open assumption.
	open, err := r.OpenAssumptions(ctx, a.Tenant(), "")
	if err != nil || len(open) == 0 {
		t.Fatalf("open items: %v %v", open, err)
	}
	// Recording the evidence is not a new claim either, so a full store takes it.
	notice := onDoc(t, r, a, "https://example.com/shipments/1", "shipping notice 2026-10-02")
	notice.SourceDate = "2026-10-02"
	if _, err := r.Promote(ctx, a, tka.PromoteInput{AssumptionID: open[0].ID, BasisInput: notice}); err != nil {
		t.Fatalf("a full store must still let an open claim be settled: %v", err)
	}
}

// A store past its date takes no more writes of any kind, and still reads.
func TestTrial_ExpiredClosesEveryWriteAndStillReads(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	live := trial(t, r, tka.Tenant{ExpiresAt: ptr(time.Now().Add(time.Hour))})

	res, err := r.Assume(ctx, live, tka.AssumeInput{
		Claim: "The lease renews in March 2027", Type: "A-p", Source: "owner",
		If: "the landlord signs", Not: "March ends unsigned",
	})
	if err != nil || res.Entry == nil {
		t.Fatalf("write while open: %+v %v", res, err)
	}
	id := res.Entry.ID

	// Same store, now closed.
	if _, err := r.Pool.Exec(ctx, `UPDATE tenant SET expires_at = now() - interval '1 day' WHERE id = $1`,
		live.Tenant()); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Assume(ctx, live, tka.AssumeInput{Claim: "Something else entirely", Type: "A-r", Source: "owner"}); code(t, err) != "TENANT_EXPIRED" {
		t.Fatalf("assume after expiry: %v", err)
	}
	if _, err := r.AddPrinciple(ctx, live, "P.1", "Anything.", ""); code(t, err) != "TENANT_EXPIRED" {
		t.Fatalf("principle_add after expiry: %v", err)
	}
	if _, err := r.Promote(ctx, live, tka.PromoteInput{AssumptionID: id,
		BasisInput: tka.BasisInput{Basis: "fact", Source: "signed lease", SourceDate: "2026-03-01"}}); code(t, err) != "TENANT_EXPIRED" {
		t.Fatalf("promote after expiry: %v", err)
	}
	if _, _, err := r.Retire(ctx, live, id, "no longer relevant"); code(t, err) != "TENANT_EXPIRED" {
		t.Fatalf("retire after expiry: %v", err)
	}

	// Closed, not deleted: everything written is still readable.
	entry, err := r.Read(ctx, live.Tenant(), id)
	if err != nil || entry.Assumption == nil || entry.Assumption.Status != "open" {
		t.Fatalf("a closed store still reads: %+v %v", entry, err)
	}
}

// A claim is one sentence. A trial store says so with a number.
func TestTrial_ClaimLengthIsARefusalNotATruncation(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	a := trial(t, r, tka.Tenant{MaxClaim: ptr(40), ExpiresAt: ptr(time.Now().Add(time.Hour))})

	long := "This claim runs on well past the point where it is still one claim rather than several"
	if _, err := r.Assume(ctx, a, tka.AssumeInput{Claim: long, Type: "A-r", Source: "owner"}); code(t, err) != "CLAIM_TOO_LONG" {
		t.Fatalf("over-long claim: %v", err)
	}
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: long,
		BasisInput: tka.BasisInput{Basis: "fact", Source: "somewhere", SourceDate: "2026-09-01"}}); code(t, err) != "CLAIM_TOO_LONG" {
		t.Fatalf("over-long claim on admit: %v", err)
	}
	if _, err := r.Assume(ctx, a, tka.AssumeInput{Claim: "The lease renews in 2027", Type: "A-r", Source: "owner"}); err != nil {
		t.Fatalf("a short claim still goes in: %v", err)
	}
}

// A customer's store carries no terms, so it carries no ceiling and records
// nothing in the funnel. One mechanism, different numbers.
func TestCustomerStore_NoLimitsAndNoFunnel(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	a := trial(t, r, tka.Tenant{Tier: "pro"}) // no expiry, no ceiling

	for i := 1; i <= 3; i++ {
		if _, err := r.Assume(ctx, a, tka.AssumeInput{
			Claim: fmt.Sprintf("Customer claim number %d stands on its own", i), Type: "A-r", Source: "owner",
		}); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
	var events int
	if err := r.Pool.QueryRow(ctx, `SELECT count(*) FROM demo_event WHERE tenant_id = $1`, a.Tenant()).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("a customer's store is not a funnel: %d event(s) recorded", events)
	}
}

// The funnel counts how far a stranger got, once per step, and never the claim.
func TestFunnel_CountsStepsOncePerStore(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	a := trial(t, r, tka.Tenant{ExpiresAt: ptr(time.Now().Add(time.Hour))})

	claim := "Acme renews the contract before 2026-12-31"
	res, err := r.Assume(ctx, a, tka.AssumeInput{Claim: claim, Type: "A-p", Source: "owner",
		If: "a signed renewal arrives", Not: "the quarter ends with no signature"})
	if err != nil || res.Entry == nil {
		t.Fatalf("assume: %+v %v", res, err)
	}
	// admit on a claim already held as a guess: the refusal the demo is about.
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: claim,
		BasisInput: tka.BasisInput{Basis: "fact", Source: "gut feeling"}}); code(t, err) != "USE_PROMOTE" {
		t.Fatalf("expected USE_PROMOTE: %v", err)
	}
	renewal := onDoc(t, r, a, "https://example.com/renewals/acme", "renewal email")
	renewal.SourceDate = "2026-10-02"
	if _, err := r.Promote(ctx, a, tka.PromoteInput{AssumptionID: res.Entry.ID, BasisInput: renewal}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// Act 2: a principle falls and names what was resting on it.
	if _, err := r.AddPrinciple(ctx, a, "P.1", "Only contract data from the CRM is trusted.", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddPrinciple(ctx, a, "P.2", "A decision is taken from the record, not from memory.", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "The CRM renewal date is the one we act on",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{"P.1", "P.2"},
			Derivation: "P.1 ⟹ the CRM is the trusted record for contract dates; " +
				"P.2 ⟹ the decision is taken from that record ⟹ its renewal date is the one we act on"}}); err != nil {
		t.Fatal(err)
	}
	if _, affected, err := r.RetirePrinciple(ctx, a, "P.1", "CRM data turned out unreliable"); err != nil || len(affected) != 1 {
		t.Fatalf("retire P.1: %v %v", affected, err)
	}

	steps := map[string]int{}
	rows, err := r.Pool.Query(ctx, `SELECT kind, count(*) FROM demo_event WHERE tenant_id = $1 GROUP BY kind`, a.Tenant())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			t.Fatal(err)
		}
		steps[kind] = n
	}
	for _, want := range []string{tka.StepWrite, tka.StepRefusal, tka.StepPromote, tka.StepAct2} {
		if steps[want] != 1 {
			t.Errorf("%s recorded %d times, want once: %v", want, steps[want], steps)
		}
	}
}
