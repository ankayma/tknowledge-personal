// Package testapi starts a real tk-api for integration tests: one Postgres
// container, migrations applied as superuser, the HTTP server backed by the
// tk_api role — the same role production runs as, so grants are exercised too.
// It also holds a test grant key so callers can mint grants, and
// loads the complete "demo" template next to the shipped ones.
// Shared by internal/cli, internal/kb, internal/mcpserver and internal/orgio tests.
package testapi

import (
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ankayma/tknowledge-personal/internal/api"
	"github.com/ankayma/tknowledge-personal/internal/eff"
	"github.com/ankayma/tknowledge-personal/internal/flags"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/setup/setuptest"
)

// Aud is the audience the test tk-api accepts.
const Aud = "tk"

type Env struct {
	Server   *httptest.Server
	Admin    *pgxpool.Pool // superuser — for asserting on rows directly
	AdminDSN string
	Key      ed25519.PrivateKey
	API      *api.Server // the server behind Server, for switching on the door or OAuth
}

// Agents is the producer set the test API treats as machines (is_trust).
var Agents = map[string]bool{"claude-desktop": true}

// Token mints a grant for user acting as actor, scope tk:read + tk:write unless given.
func (e *Env) Token(t *testing.T, user, actor string, scope ...string) string {
	t.Helper()
	if len(scope) == 0 {
		scope = []string{grant.ScopeRead, grant.ScopeWrite}
	}
	tok, err := grant.Sign(e.Key, grant.Grant{UserID: user, ActorID: actor, NodeID: "test", Aud: Aud, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// StartPostgres starts Postgres 16 with every migration applied and returns
// its superuser pool, the superuser DSN, and host:port.
func StartPostgres(t *testing.T) (*pgxpool.Pool, string, string) {
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
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })

	adminDSN, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if err := migrate.Apply(ctx, adminPool); err != nil {
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
	return adminPool, adminDSN, host + ":" + port.Port()
}

func Start(t *testing.T) *Env {
	t.Helper()
	ctx := context.Background()
	adminPool, adminDSN, hostport := StartPostgres(t)

	apiPool, err := pgxpool.New(ctx, "postgres://tk_api:dev@"+hostport+"/tk?sslmode=disable")
	if err != nil {
		t.Fatalf("connect tk_api: %v", err)
	}
	t.Cleanup(apiPool.Close)

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := ledger.New(apiPool)
	srv := api.New(repo, eff.New(repo, Agents), flags.New(apiPool),
		&grant.Verifier{Keys: []ed25519.PublicKey{pub}, Aud: Aud})
	srv.Templates = setup.MustLoad(setup.Embedded, setuptest.Demo)
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return &Env{Server: ts, Admin: adminPool, AdminDSN: adminDSN, Key: priv, API: srv}
}
