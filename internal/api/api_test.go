package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ankayma/tknowledge-personal/internal/eff"
	"github.com/ankayma/tknowledge-personal/internal/flags"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/migrate"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/setup/setuptest"
)

const testAud = "tk"

type testEnv struct {
	URL    string
	Admin  *pgxpool.Pool
	Key    ed25519.PrivateKey
	Server *Server // the same server the httptest handler serves
}

// newTestServer starts one Postgres container, applies migrations as
// superuser, and returns an httptest server backed by the tk_api role — the
// same role tk-api runs as in production, so grants/permissions are exercised too.
// internal/testapi has the same setup for other packages; this file cannot
// import it (testapi imports api → cycle), hence the copy.
func newTestServer(t *testing.T) *testEnv {
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
	apiDSN := "postgres://tk_api:dev@" + host + ":" + port.Port() + "/tk?sslmode=disable"
	apiPool, err := pgxpool.New(ctx, apiDSN)
	if err != nil {
		t.Fatalf("connect tk_api: %v", err)
	}
	t.Cleanup(apiPool.Close)

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := ledger.New(apiPool)
	evaluator := eff.New(repo, map[string]bool{"claude-desktop": true})
	flagsRepo := flags.New(apiPool)
	srv := New(repo, evaluator, flagsRepo, &grant.Verifier{Keys: []ed25519.PublicKey{pub}, Aud: testAud})
	srv.Templates = setup.MustLoad(setup.Embedded, setuptest.Demo)

	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return &testEnv{URL: ts.URL, Admin: adminPool, Key: priv, Server: srv}
}

// token mints a grant for user acting as actor (tk:read + tk:write unless scope given).
func (e *testEnv) token(t *testing.T, user, actor string, scope ...string) string {
	t.Helper()
	return signGrant(t, e.Key, grant.Grant{UserID: user, ActorID: actor, NodeID: "test", Aud: testAud, Scope: scope})
}

func signGrant(t *testing.T, key ed25519.PrivateKey, g grant.Grant) string {
	t.Helper()
	if g.Scope == nil {
		g.Scope = []string{grant.ScopeRead, grant.ScopeWrite}
	}
	tok, err := grant.Sign(key, g)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func postJSON(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

func movesURL(base string, claimID int64) string {
	return base + "/v1/claims/" + strconv.FormatInt(claimID, 10) + "/moves"
}

func TestHealthz(t *testing.T) {
	env := newTestServer(t)
	resp, err := http.Get(env.URL + "/v1/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// T-01: POST /claims with mark=T and no basis → 400.
func TestT01_MarkTWithoutBasisRejected(t *testing.T) {
	env := newTestServer(t)
	cause := "c1"
	resp := postJSON(t, env.URL+"/v1/claims", env.token(t, "owner", "owner"), []claimReq{
		{Statement: "x", Mark: "T", CauseID: &cause},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// T-02: refs pointing at an id ≥ the next id to be issued → 400 (Inv5).
func TestT02_RefToFutureIDRejected(t *testing.T) {
	env := newTestServer(t)
	cause := "c1"
	resp := postJSON(t, env.URL+"/v1/claims", env.token(t, "owner", "owner"), []claimReq{
		{
			Statement: "x", Mark: "T", CauseID: &cause,
			Basis: &ledger.Basis{Kind: "derivation", Refs: []int64{999999}},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// T-03: a move that does not fit the state → 409 (reapprove on a claim not yet demoted).
func TestT03_InvalidMoveForState(t *testing.T) {
	env := newTestServer(t)

	resp := postJSON(t, env.URL+"/v1/claims", env.token(t, "owner", "claude-desktop"), []claimReq{
		{Statement: "x", Mark: "A"},
	})
	claimID := decode[[]claimIDResp](t, resp)[0].ClaimID

	cause := "c1"
	resp2 := postJSON(t, movesURL(env.URL, claimID), env.token(t, "owner", "owner"), moveReq{
		Move: "reapprove", CauseID: &cause, Basis: &ledger.Basis{Kind: "fact", Note: "x"},
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp2.StatusCode)
	}
}

// T-04: promote without a grant → 401; without cause_id → 400. (signer can no
// longer be missing: it is the grant's user.)
func TestT04_PromoteWithoutGrantOrCauseRejected(t *testing.T) {
	env := newTestServer(t)

	resp := postJSON(t, env.URL+"/v1/claims", env.token(t, "owner", "claude-desktop"), []claimReq{
		{Statement: "x", Mark: "A"},
	})
	claimID := decode[[]claimIDResp](t, resp)[0].ClaimID
	cause := "c1"

	noGrant := postJSON(t, movesURL(env.URL, claimID), "", moveReq{
		Move: "promote", CauseID: &cause, Basis: &ledger.Basis{Kind: "fact", Note: "x"},
	})
	defer noGrant.Body.Close()
	if noGrant.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no grant: expected 401, got %d", noGrant.StatusCode)
	}

	noCause := postJSON(t, movesURL(env.URL, claimID), env.token(t, "owner", "owner"), moveReq{
		Move: "promote", Basis: &ledger.Basis{Kind: "fact", Note: "x"},
	})
	defer noCause.Body.Close()
	if noCause.StatusCode != http.StatusBadRequest {
		t.Fatalf("no cause: expected 400, got %d", noCause.StatusCode)
	}
}

// An agent that holds a grant in its own name and signs its own row is still
// detectable (self_signed_agent) — the grant names who signed, it does not
// make an agent a person.
func TestMoveProducer_SelfSignedAgentDetected(t *testing.T) {
	env := newTestServer(t)
	agentTok := env.token(t, "claude-desktop", "claude-desktop")

	resp := postJSON(t, env.URL+"/v1/claims", agentTok, []claimReq{
		{Statement: "agent claim", Mark: "A"},
	})
	claimID := decode[[]claimIDResp](t, resp)[0].ClaimID

	cause := "c1"
	resp2 := postJSON(t, movesURL(env.URL, claimID), agentTok, moveReq{
		Move: "promote", CauseID: &cause, Basis: &ledger.Basis{Kind: "fact", Note: "x"},
	})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("promote: expected 200, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	auditResp, err := http.Get(env.URL + "/v1/claims/" + strconv.FormatInt(claimID, 10) + "/audit")
	if err != nil {
		t.Fatalf("GET audit: %v", err)
	}
	var audit struct {
		Trust predicateResp `json:"trust"`
	}
	audit = decode[struct {
		Trust predicateResp `json:"trust"`
	}](t, auditResp)
	if audit.Trust.OK {
		t.Fatalf("expected is_trust=false (agent signed its own row), got true")
	}
	if audit.Trust.Code != eff.CodeSelfSignedAgent {
		t.Fatalf("expected code=%s, got %s", eff.CodeSelfSignedAgent, audit.Trust.Code)
	}
}

// Full happy path: principle → claim anchored to it → promote → GET /claims, /audit, /stats.
func TestHappyPath_PrincipleClaimPromote(t *testing.T) {
	env := newTestServer(t)
	owner := env.token(t, "owner", "owner")

	pResp := postJSON(t, env.URL+"/v1/principles", owner, []principleReq{
		{Code: "P.1", Statement: "Strong architecture absorbs weak execution.", CauseID: "c0"},
	})
	principleID := decode[[]claimIDResp](t, pResp)[0].ClaimID

	cResp := postJSON(t, env.URL+"/v1/claims", env.token(t, "owner", "claude-desktop"), []claimReq{
		{Statement: "candidate", Mark: "A"},
	})
	claimID := decode[[]claimIDResp](t, cResp)[0].ClaimID

	cause := "c1"
	mResp := postJSON(t, movesURL(env.URL, claimID), owner, moveReq{
		Move: "promote", CauseID: &cause,
		Basis: &ledger.Basis{Kind: "principle", Refs: []int64{principleID}},
	})
	if mResp.StatusCode != http.StatusOK {
		t.Fatalf("promote: expected 200, got %d", mResp.StatusCode)
	}
	mResp.Body.Close()

	getResp, err := http.Get(env.URL + "/v1/claims/" + strconv.FormatInt(claimID, 10))
	if err != nil {
		t.Fatalf("GET claim: %v", err)
	}
	claim := decode[claimResp](t, getResp)
	if !claim.Eff {
		t.Fatalf("expected eff=true after promote anchored to a T principle, got false: %+v", claim)
	}
	if claim.Status != "active" {
		t.Fatalf("expected status=active, got %s", claim.Status)
	}

	auditResp, err := http.Get(env.URL + "/v1/claims/" + strconv.FormatInt(claimID, 10) + "/audit")
	if err != nil {
		t.Fatalf("GET audit: %v", err)
	}
	defer auditResp.Body.Close()
	if auditResp.StatusCode != http.StatusOK {
		t.Fatalf("audit: expected 200, got %d", auditResp.StatusCode)
	}

	statsResp, err := http.Get(env.URL + "/v1/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	stats := decode[map[string]int64](t, statsResp)
	if stats["claims"] < 2 {
		t.Fatalf("expected at least 2 claims in stats, got %+v", stats)
	}
}

// `propose` and `import` are first-row-only. Through /moves,
// `propose` would append an unsigned, cause-less A row over any claim (an
// unsigned demote), so both must be refused before touching the ledger.
func TestMove_FirstRowOnlyMovesRejected(t *testing.T) {
	env := newTestServer(t)
	owner := env.token(t, "owner", "owner")

	cause := "c-first"
	resp := postJSON(t, env.URL+"/v1/claims", owner, []claimReq{
		{Statement: "anchored", Mark: "T", CauseID: &cause,
			Basis: &ledger.Basis{Kind: "fact", Note: "src 2026-09-12"}},
	})
	claimID := decode[[]claimIDResp](t, resp)[0].ClaimID

	for _, move := range []string{"propose", "import"} {
		r := postJSON(t, movesURL(env.URL, claimID), owner, moveReq{Move: move, CauseID: &cause})
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s via /moves: expected 400, got %d", move, r.StatusCode)
		}
		r.Body.Close()
	}

	// The claim is untouched: still the single import row, still T.
	get, err := http.Get(env.URL + "/v1/claims/" + strconv.FormatInt(claimID, 10))
	if err != nil {
		t.Fatal(err)
	}
	body := decode[struct {
		Mark    string `json:"mark"`
		History []any  `json:"history"`
	}](t, get)
	if body.Mark != "T" || len(body.History) != 1 {
		t.Fatalf("claim changed: mark=%s history=%d", body.Mark, len(body.History))
	}
}
