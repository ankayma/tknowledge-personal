package migrate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// T-06: role tk_checker INSERT on ledger → permission error;
// role tk_api UPDATE/DELETE on ledger → permission error. No role has
// UPDATE/DELETE on ledger.
func TestT06_LedgerAppendOnlyPermissions(t *testing.T) {
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

	adminDSN, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
	}
	defer adminPool.Close()

	if err := Apply(ctx, adminPool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	dsnFor := func(role string) string {
		return fmt.Sprintf("postgres://%s:dev@%s:%s/tk?sslmode=disable", role, host, port.Port())
	}

	tkAPI, err := pgxpool.New(ctx, dsnFor("tk_api"))
	if err != nil {
		t.Fatalf("connect as tk_api: %v", err)
	}
	defer tkAPI.Close()

	tkChecker, err := pgxpool.New(ctx, dsnFor("tk_checker"))
	if err != nil {
		t.Fatalf("connect as tk_checker: %v", err)
	}
	defer tkChecker.Close()

	// tk_api can INSERT into ledger (needed to have a row to test UPDATE/DELETE against).
	var id int64
	err = tkAPI.QueryRow(ctx,
		`INSERT INTO ledger (claim_id, statement, mark, producer, move) VALUES (1, 'x', 'A', 'test', 'propose') RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("tk_api INSERT ledger should succeed: %v", err)
	}

	assertPermissionDenied := func(t *testing.T, label string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected permission error, got success", label)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" { // insufficient_privilege
			t.Fatalf("%s: expected insufficient_privilege (42501), got: %v", label, err)
		}
	}

	// tk_api cannot UPDATE ledger.
	_, err = tkAPI.Exec(ctx, `UPDATE ledger SET statement = 'y' WHERE id = $1`, id)
	assertPermissionDenied(t, "tk_api UPDATE ledger", err)

	// tk_api cannot DELETE from ledger.
	_, err = tkAPI.Exec(ctx, `DELETE FROM ledger WHERE id = $1`, id)
	assertPermissionDenied(t, "tk_api DELETE ledger", err)

	// tk_checker cannot INSERT into ledger.
	_, err = tkChecker.Exec(ctx,
		`INSERT INTO ledger (claim_id, statement, mark, producer, move) VALUES (2, 'x', 'A', 'test', 'propose')`,
	)
	assertPermissionDenied(t, "tk_checker INSERT ledger", err)

	// tk_checker cannot UPDATE ledger either.
	_, err = tkChecker.Exec(ctx, `UPDATE ledger SET statement = 'y' WHERE id = $1`, id)
	assertPermissionDenied(t, "tk_checker UPDATE ledger", err)

	// tk_checker CAN read ledger.
	var readBack string
	err = tkChecker.QueryRow(ctx, `SELECT statement FROM ledger WHERE id = $1`, id).Scan(&readBack)
	if err != nil {
		t.Fatalf("tk_checker SELECT ledger should succeed: %v", err)
	}

	// tk_checker CAN insert flags.
	_, err = tkChecker.Exec(ctx,
		`INSERT INTO flags (claim_id, kind, evidence) VALUES (1, 'unanchored', '{}'::jsonb)`,
	)
	if err != nil {
		t.Fatalf("tk_checker INSERT flags should succeed: %v", err)
	}

	// tk_api cannot insert flags (only SELECT granted).
	_, err = tkAPI.Exec(ctx,
		`INSERT INTO flags (claim_id, kind, evidence) VALUES (1, 'unanchored', '{}'::jsonb)`,
	)
	assertPermissionDenied(t, "tk_api INSERT flags", err)
}
