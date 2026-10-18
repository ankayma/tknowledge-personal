package checker

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ankayma/tknowledge-personal/internal/eff"
	"github.com/ankayma/tknowledge-personal/internal/flags"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/migrate"
)

func newTestChecker(t *testing.T) (*Checker, *ledger.Repo, *flags.Repo) {
	t.Helper()
	ctx := context.Background()

	pgCtr, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase("tk"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("dev"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pgCtr.Terminate(ctx) })

	redisCtr, err := tcredis.Run(ctx, "redis:7",
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() { _ = redisCtr.Terminate(ctx) })

	// Admin pool: migrations + the test's own ledger writes / flag reads.
	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
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

	// The checker under test runs as tk_checker, exactly like the container,
	// so a missing GRANT surfaces here instead of in compose.
	host, err := pgCtr.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := pgCtr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	checkerDSN := "postgres://tk_checker:dev@" + host + ":" + port.Port() + "/tk?sslmode=disable"
	checkerPool, err := pgxpool.New(ctx, checkerDSN)
	if err != nil {
		t.Fatalf("connect tk_checker: %v", err)
	}
	t.Cleanup(checkerPool.Close)

	redisAddr, err := redisCtr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	opt, err := redis.ParseURL(redisAddr)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })

	checkerRepo := ledger.New(checkerPool)
	evaluator := eff.New(checkerRepo, map[string]bool{"claude-desktop": true})
	c := New(checkerRepo, evaluator, flags.New(checkerPool), rdb)
	return c, ledger.New(pool), flags.New(pool)
}

func strptr(s string) *string { return &s }

// T-05: chain 1 → 2 (refs 1) → demote 1: eff(2) = A, premise_demoted flag on 2, failing_id=1.
func TestT05_CascadeDemoteOpensPremiseDemotedFlag(t *testing.T) {
	c, repo, flagsRepo := newTestChecker(t)
	ctx := context.Background()

	// InsertValidated with ClaimID=0 makes claim_id = its own row id (on a
	// claim's first row, claim_id = id), so the returned id is both.
	premiseClaimID, err := repo.InsertValidated(ctx, ledger.InsertParams{
		Statement: "premise", Mark: "T",
		Basis:  &ledger.Basis{Kind: "fact", Note: "x"},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert premise: %v", err)
	}

	derivedID, err := repo.InsertValidated(ctx, ledger.InsertParams{
		Statement: "derived", Mark: "T",
		Basis:  &ledger.Basis{Kind: "derivation", Refs: []int64{premiseClaimID}},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c2"),
	})
	if err != nil {
		t.Fatalf("insert derived: %v", err)
	}

	if err := c.Sweep(ctx); err != nil {
		t.Fatalf("sweep before demote: %v", err)
	}
	openBefore, err := flagsRepo.ForClaim(ctx, derivedID)
	if err != nil {
		t.Fatalf("flags before demote: %v", err)
	}
	if len(openBefore) != 0 {
		t.Fatalf("expected no flags before demote, got %+v", openBefore)
	}

	_, err = repo.InsertValidated(ctx, ledger.InsertParams{
		ClaimID: premiseClaimID, Statement: "premise", Mark: "A",
		Signer: strptr("owner"), Producer: "owner", Move: "demote",
		CauseID: strptr("c3"), Reason: strptr("no longer holds"),
	})
	if err != nil {
		t.Fatalf("insert demote: %v", err)
	}

	if err := c.Sweep(ctx); err != nil {
		t.Fatalf("sweep after demote: %v", err)
	}

	openAfter, err := flagsRepo.ForClaim(ctx, derivedID)
	if err != nil {
		t.Fatalf("flags after demote: %v", err)
	}
	if len(openAfter) != 1 {
		t.Fatalf("expected exactly one open flag on the derived claim, got %+v", openAfter)
	}
	if openAfter[0].Kind != eff.CodePremiseDemoted {
		t.Fatalf("expected kind=%s, got %s", eff.CodePremiseDemoted, openAfter[0].Kind)
	}
}

// T-07: fact past valid_until → is_truth drops, expired_basis flag, mark unchanged.
func TestT07_ExpiredFactOpensExpiredBasisFlag(t *testing.T) {
	c, repo, flagsRepo := newTestChecker(t)
	ctx := context.Background()

	past := time.Now().Add(-24 * time.Hour)
	id, err := repo.InsertValidated(ctx, ledger.InsertParams{
		Statement: "vendor cert", Mark: "T",
		Basis:  &ledger.Basis{Kind: "fact", Note: "cert 2025", ValidUntil: &past},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert expired fact: %v", err)
	}

	if err := c.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	open, err := flagsRepo.ForClaim(ctx, id)
	if err != nil {
		t.Fatalf("flags: %v", err)
	}
	if len(open) != 1 || open[0].Kind != eff.CodeExpiredBasis {
		t.Fatalf("expected one expired_basis flag, got %+v", open)
	}

	cur, found, err := repo.Current(ctx, id)
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if cur.Mark != "T" {
		t.Fatalf("expected mark to stay T (checker never writes ledger), got %s", cur.Mark)
	}
}

// T-09: refute 1, propose the same statement → previously_refuted flag.
func TestT09_DuplicateOfRefutedStatementFlagged(t *testing.T) {
	c, repo, flagsRepo := newTestChecker(t)
	ctx := context.Background()

	id, err := repo.InsertValidated(ctx, ledger.InsertParams{
		Statement: "Vendor X is compliant.", Mark: "T",
		Basis:  &ledger.Basis{Kind: "fact", Note: "audit 2025"},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := c.Sweep(ctx); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}

	_, err = repo.InsertValidated(ctx, ledger.InsertParams{
		ClaimID: id, Statement: "Vendor X is compliant.", Mark: "A",
		Signer: strptr("owner"), Producer: "owner", Move: "refute",
		CauseID: strptr("c2"), Reason: strptr("audit was fraudulent"),
	})
	if err != nil {
		t.Fatalf("insert refute: %v", err)
	}
	if err := c.Sweep(ctx); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}

	dupID, err := repo.InsertValidated(ctx, ledger.InsertParams{
		Statement: "vendor x is COMPLIANT!!", Mark: "A",
		Producer: "claude-desktop", Move: "propose",
	})
	if err != nil {
		t.Fatalf("insert duplicate propose: %v", err)
	}
	if err := c.Sweep(ctx); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}

	open, err := flagsRepo.ForClaim(ctx, dupID)
	if err != nil {
		t.Fatalf("flags: %v", err)
	}
	if len(open) != 1 || open[0].Kind != previouslyRefuted {
		t.Fatalf("expected one previously_refuted flag, got %+v", open)
	}
}

// Sweep step 5: CJCM alert unjustified_ledger_row → violations_seen +
// flag unjustified_row, delivered outside any Sweep (an UPDATE/DELETE never
// produces a "new row" Sweep would see).
func TestConsumeAlerts_UnjustifiedRowOpensFlag(t *testing.T) {
	c, repo, flagsRepo := newTestChecker(t)
	ctx := context.Background()

	claimID, err := repo.InsertValidated(ctx, ledger.InsertParams{
		Statement: "x", Mark: "T",
		Basis:  &ledger.Basis{Kind: "fact", Note: "x"},
		Signer: strptr("owner"), Producer: "owner", Move: "import", CauseID: strptr("c1"),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	consumeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- c.ConsumeAlerts(consumeCtx) }()

	// give XGROUP CREATE a moment to land before publishing (poll, not a bare sleep).
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := c.Rdb.XInfoGroups(ctx, alertsStream).Result()
		if err == nil && len(info) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("consumer group never appeared on %s", alertsStream)
		}
		time.Sleep(50 * time.Millisecond)
	}

	rowID := claimID // this claim's only row id equals its claim_id (first row)
	if err := c.Rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: alertsStream,
		Values: map[string]interface{}{
			"vocab": "c", "kind": "unjustified_ledger_row",
			"row_id": rowID, "claim_id": claimID,
		},
	}).Err(); err != nil {
		t.Fatalf("XADD alert: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	var open []flags.Flag
	for time.Now().Before(deadline) {
		open, err = flagsRepo.ForClaim(ctx, claimID)
		if err != nil {
			t.Fatalf("flags: %v", err)
		}
		if len(open) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(open) != 1 || open[0].Kind != eff.CodeUnjustifiedRow {
		t.Fatalf("expected one unjustified_row flag, got %+v", open)
	}

	var inViolations bool
	if err := repo.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM violations_seen WHERE row_id = $1)`, rowID).Scan(&inViolations); err != nil {
		t.Fatalf("check violations_seen: %v", err)
	}
	if !inViolations {
		t.Fatalf("expected row %d in violations_seen", rowID)
	}

	cancel()
	<-errCh
}
