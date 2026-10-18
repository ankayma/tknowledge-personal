package deploytest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Postgres init scripts (deploy/initdb): one instance, three databases, and
// no role can even connect to another service's database — C-06 for CJCM's
// `audit` role, and the same property for every other role.
func TestInitdbConnectIsolation(t *testing.T) {
	ctx := context.Background()
	s1, _ := filepath.Abs("../../deploy/initdb/01-databases.sql")
	s2, _ := filepath.Abs("../../deploy/initdb/02-roles.sql")
	ctr, err := postgres.Run(ctx, "postgres:16",
		postgres.WithUsername("postgres"), postgres.WithPassword("dev"), postgres.WithDatabase("postgres"),
		postgres.WithInitScripts(s1, s2),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2)),
	)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })
	host, _ := ctr.Host(ctx)
	port, _ := ctr.MappedPort(ctx, "5432/tcp")

	connect := func(role, db string) error {
		conn, err := pgx.Connect(ctx, "postgres://"+role+":dev@"+host+":"+port.Port()+"/"+db+"?sslmode=disable")
		if err != nil {
			return err
		}
		return conn.Close(ctx)
	}
	allowed := map[string]string{
		"tk_api": "tk", "tk_checker": "tk", "tk_cdc": "tk",
		"cc_api": "cc", "cc_exec": "cc", "cc_outbox": "cc",
		"audit": "audit",
	}
	for role, own := range allowed {
		if err := connect(role, own); err != nil {
			t.Fatalf("%s → %s should connect: %v", role, own, err)
		}
		for _, other := range []string{"tk", "cc", "audit"} {
			if other == own {
				continue
			}
			if err := connect(role, other); err == nil {
				t.Fatalf("%s must not be able to connect to %s", role, other)
			}
		}
	}

	// CJCM creates its own tables in `audit` (pgstore) — the owner grant must
	// actually allow CREATE in public on PG16.
	conn, err := pgx.Connect(ctx, "postgres://audit:dev@"+host+":"+port.Port()+"/audit?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE audit_alerts_probe (seq bigint primary key)`); err != nil {
		t.Fatalf("audit role cannot create its own table: %v", err)
	}
}
