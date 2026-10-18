package cdc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ankayma/tknowledge-personal/internal/migrate"
)

// T-08: a row inserted by superuser (outside tk-api) shows up in the
// tk:ledger stream within 2s. This is the CJ-10 requirement: the change channel
// is the DB's feed, not an API log.
func TestT08_DirectInsertReachesStreamWithin2s(t *testing.T) {
	ctx := context.Background()

	pgCtr, err := tcpostgres.Run(ctx, "postgres:16",
		tcpostgres.WithDatabase("tk"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("dev"),
		testcontainers.WithCmdArgs("-c", "wal_level=logical", "-c", "max_replication_slots=4", "-c", "max_wal_senders=4"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = pgCtr.Terminate(ctx) })

	redisCtr, err := tcredis.Run(ctx, "redis:7",
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = redisCtr.Terminate(ctx) })

	adminDSN, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
	}
	defer adminPool.Close()

	if err := migrate.Apply(ctx, adminPool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	host, err := pgCtr.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := pgCtr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	cdcDSN := fmt.Sprintf("postgres://tk_cdc:dev@%s:%s/tk?sslmode=disable", host, port.Port())

	redisAddr, err := redisCtr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis connection string: %v", err)
	}
	opt, err := redis.ParseURL(redisAddr)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	defer rdb.Close()

	relayCtx, cancelRelay := context.WithCancel(ctx)
	defer cancelRelay()

	relay := New(cdcDSN, rdb)
	relayErrCh := make(chan error, 1)
	go func() { relayErrCh <- relay.Run(relayCtx) }()

	waitForSlotActive(t, ctx, adminPool, 10*time.Second)

	// Insert straight into ledger as superuser, simulating the main CJ-10 case:
	// a row that does not go through tk-api.
	var insertedID int64
	err = adminPool.QueryRow(ctx,
		`INSERT INTO ledger (claim_id, statement, mark, producer, signer, move, cause_id)
		 VALUES (999, 'bypass insert', 'A', 'superuser', 'nobody', 'propose', NULL) RETURNING id`,
	).Scan(&insertedID)
	if err != nil {
		t.Fatalf("direct insert as superuser: %v", err)
	}
	insertedAt := time.Now()

	deadline := insertedAt.Add(2 * time.Second)
	var found bool
	var foundID string
	for time.Now().Before(deadline) {
		entries, err := rdb.XRange(ctx, "tk:ledger", "-", "+").Result()
		if err != nil {
			t.Fatalf("XRANGE tk:ledger: %v", err)
		}
		for _, e := range entries {
			if e.Values["id"] == fmt.Sprintf("%d", insertedID) {
				found = true
				foundID = e.Values["id"].(string)
				break
			}
		}
		if found {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	elapsed := time.Since(insertedAt)
	if !found {
		t.Fatalf("row id=%d not on tk:ledger stream within 2s (waited %s)", insertedID, elapsed)
	}
	t.Logf("row id=%d (stream id=%s) reached tk:ledger after %s", insertedID, foundID, elapsed)

	cancelRelay()
	if err := <-relayErrCh; err != nil && ctx.Err() == nil {
		// relay.Run returns context.Canceled once we cancel it above; anything else is a real failure.
		if relayCtx.Err() == nil {
			t.Fatalf("relay exited with unexpected error: %v", err)
		}
	}
}

func waitForSlotActive(t *testing.T, ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var active bool
		err := pool.QueryRow(ctx,
			`SELECT active FROM pg_replication_slots WHERE slot_name = 'tk_slot'`,
		).Scan(&active)
		if err == nil && active {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("replication slot tk_slot not active within %s", timeout)
}
