package eff

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/migrate"
)

// newTestEvaluator starts one shared Postgres container for the whole test
// binary run (predicates are read-only queries against whatever rows each
// subtest inserts under its own claim_id range — cheap to share).
func newTestEvaluator(t *testing.T) (*Evaluator, *ledger.Repo) {
	t.Helper()
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("tk"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("dev"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	repo := ledger.New(pool)
	agents := map[string]bool{"loan-agent": true, "claude-desktop": true}
	return New(repo, agents), repo
}

func strptr(s string) *string { return &s }

func TestIsTruth_Principle(t *testing.T) {
	ev, repo := newTestEvaluator(t)
	ctx := context.Background()

	// Principle P.1: imported as T, basis.kind = principle, refs = [].
	_, err := repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 101, Statement: "P.1", Mark: "T",
		Basis:  &ledger.Basis{Kind: "principle", Refs: []int64{}},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert principle: %v", err)
	}

	// Claim anchored to the principle. refs holds claim_id values (101), not row ids.
	_, err = repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 102, Statement: "C", Mark: "T",
		Basis:  &ledger.Basis{Kind: "principle", Refs: []int64{101}},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c2"),
	})
	if err != nil {
		t.Fatalf("insert claim: %v", err)
	}

	res, err := ev.IsTruth(ctx, 102)
	if err != nil {
		t.Fatalf("IsTruth: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected is_truth=true, got false: %+v", res)
	}

	effOK, err := ev.Eff(ctx, 102)
	if err != nil {
		t.Fatalf("Eff: %v", err)
	}
	if !effOK {
		t.Fatalf("expected eff=T for claim anchored on a T principle")
	}
}

func TestIsTruth_FactExpired(t *testing.T) {
	ev, repo := newTestEvaluator(t)
	ctx := context.Background()

	past := time.Now().Add(-24 * time.Hour)
	fID, err := repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 201, Statement: "vendor cert", Mark: "T",
		Basis:  &ledger.Basis{Kind: "fact", Note: "cert 2025", ValidUntil: &past},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert fact: %v", err)
	}

	res, err := ev.IsTruth(ctx, fID)
	if err != nil {
		t.Fatalf("IsTruth: %v", err)
	}
	if res.OK {
		t.Fatalf("expected is_truth=false for expired fact, got true")
	}
	if res.FailingID == nil || *res.FailingID != fID {
		t.Fatalf("expected failing_id=%d, got %+v", fID, res.FailingID)
	}
}

func TestIsTrust_AgentSelfSigned(t *testing.T) {
	ev, repo := newTestEvaluator(t)
	ctx := context.Background()

	id, err := repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 301, Statement: "agent claim", Mark: "A",
		Producer: "claude-desktop", Move: "propose",
	})
	if err != nil {
		t.Fatalf("insert propose: %v", err)
	}
	// propose is allowed to have no signer — is_trust should still hold here.
	res, err := ev.IsTrust(ctx, id)
	if err != nil {
		t.Fatalf("IsTrust: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected is_trust=true for a bare propose row, got: %+v", res)
	}

	// promote where the agent producer signs its own row — must fail.
	badID, err := repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 301, Statement: "agent claim", Mark: "T",
		Basis:  &ledger.Basis{Kind: "fact", Note: "x"},
		Signer: strptr("claude-desktop"), Producer: "claude-desktop", Move: "promote", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert self-signed promote: %v", err)
	}

	res, err = ev.IsTrust(ctx, 301)
	if err != nil {
		t.Fatalf("IsTrust: %v", err)
	}
	if res.OK {
		t.Fatalf("expected is_trust=false when agent producer self-signs, got true")
	}
	if res.FailingID == nil || *res.FailingID != badID {
		t.Fatalf("expected failing_id=%d, got %+v", badID, res.FailingID)
	}
}

func TestIsTraceable_ViolationFlagsRow(t *testing.T) {
	ev, repo := newTestEvaluator(t)
	ctx := context.Background()

	id, err := repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 401, Statement: "x", Mark: "T",
		Basis:  &ledger.Basis{Kind: "fact", Note: "x"},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := ev.IsTraceable(ctx, 401)
	if err != nil {
		t.Fatalf("IsTraceable: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected is_traceable=true before any violation, got: %+v", res)
	}

	// Simulate CJCM having reported this row as unjustified (checker would write this).
	// violations_seen.row_id is the ledger row's own id, not the claim_id.
	if _, err := repo.Pool.Exec(ctx, `INSERT INTO violations_seen (row_id, kind) VALUES ($1, 'unjustified_ledger_row')`, id); err != nil {
		t.Fatalf("seed violations_seen: %v", err)
	}

	res, err = ev.IsTraceable(ctx, 401)
	if err != nil {
		t.Fatalf("IsTraceable after violation: %v", err)
	}
	if res.OK {
		t.Fatalf("expected is_traceable=false once row is in V(w), got true")
	}
	if res.FailingID == nil || *res.FailingID != id {
		t.Fatalf("expected failing_id=%d, got %+v", id, res.FailingID)
	}
}

// F2 cascade shape: claim 2 has refs [1]; demote 1 → eff(2)
// must drop to A even though claim 2's own row never changed.
func TestEff_CascadeThroughDemote(t *testing.T) {
	ev, repo := newTestEvaluator(t)
	ctx := context.Background()

	// basis.refs holds claim_id values ("refs:[claim_id]"), not row ids —
	// use the literal claim_id 501 here, not whatever row id this insert gets.
	_, err := repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 501, Statement: "premise", Mark: "T",
		Basis:  &ledger.Basis{Kind: "fact", Note: "x"},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert premise: %v", err)
	}

	_, err = repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 502, Statement: "derived", Mark: "T",
		Basis:  &ledger.Basis{Kind: "derivation", Refs: []int64{501}},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c2"),
	})
	if err != nil {
		t.Fatalf("insert derived: %v", err)
	}

	effOK, err := ev.Eff(ctx, 502)
	if err != nil {
		t.Fatalf("Eff before demote: %v", err)
	}
	if !effOK {
		t.Fatalf("expected eff(502)=T before demote")
	}

	// Demote the premise: new row, mark=A, move=demote.
	_, err = repo.Insert(ctx, ledger.InsertParams{
		ClaimID: 501, Statement: "premise", Mark: "A",
		Signer: strptr("owner"), Producer: "owner", Move: "demote",
		CauseID: strptr("c3"), Reason: strptr("no longer holds"),
	})
	if err != nil {
		t.Fatalf("insert demote: %v", err)
	}

	effOK, err = ev.Eff(ctx, 502)
	if err != nil {
		t.Fatalf("Eff after demote: %v", err)
	}
	if effOK {
		t.Fatalf("expected eff(502)=A after premise 501 demoted, got T")
	}
}
