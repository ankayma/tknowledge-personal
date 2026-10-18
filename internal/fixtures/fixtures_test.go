package fixtures

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

	"github.com/ankayma/tknowledge-personal/internal/api"
	"github.com/ankayma/tknowledge-personal/internal/checker"
	"github.com/ankayma/tknowledge-personal/internal/eff"
	"github.com/ankayma/tknowledge-personal/internal/flags"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/migrate"
)

// F1 + F2, driven by the real alphacorp fixture through the real REST API
// and a real checker sweep: script.md steps 1–3 and 5.
func TestAlphaCorp_F1ImportSweep_F2CascadeDemote(t *testing.T) {
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

	repo := ledger.New(adminPool) // superuser pool doubles for both tk-api and tk-checker here — fine for a test
	evaluator := eff.New(repo, map[string]bool{"loan-agent": true})
	flagsRepo := flags.New(adminPool)
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Each signer holds a grant in their own name; signer and
	// producer come from it, not from the request body.
	tokenFor := func(user string) string {
		tok, err := grant.Sign(key, grant.Grant{UserID: user, ActorID: user, Aud: "tk", Scope: []string{grant.ScopeWrite}})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	srv := api.New(repo, evaluator, flagsRepo, &grant.Verifier{Keys: []ed25519.PublicKey{pub}, Aud: "tk"})
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	chk := checker.New(repo, evaluator, flagsRepo, nil) // Sweep never touches Redis

	principles, err := LoadPrinciples("../../fixtures/alphacorp/principles.json")
	if err != nil {
		t.Fatalf("load principles: %v", err)
	}
	if len(principles) != 0 {
		t.Fatalf("alphacorp fixture is not expected to carry principles, got %d", len(principles))
	}

	claims, err := LoadClaims("../../fixtures/alphacorp/claims.jsonl")
	if err != nil {
		t.Fatalf("load claims: %v", err)
	}
	if len(claims) != 3 {
		t.Fatalf("expected 3 claim lines (102, 880, 905), got %d", len(claims))
	}

	// Step 1: import, translating placeholder refs ("102", "880") to real claim_ids.
	refToClaimID := map[string]int64{}
	for _, c := range claims {
		body := map[string]any{
			"statement": c.Statement,
			"mark":      c.Mark,
		}
		// A T line is written under its signer's grant; an A line under the
		// producer's own grant (loan-agent proposes 905).
		holder := c.Producer
		if c.Mark == "T" {
			holder = *c.Signer
			body["cause_id"] = "fixture-import"
		}
		if c.Basis != nil {
			refs := make([]int64, 0, len(c.Basis.Refs))
			for _, r := range c.Basis.Refs {
				id, ok := refToClaimID[r]
				if !ok {
					t.Fatalf("claims.jsonl refs %q before it's defined", r)
				}
				refs = append(refs, id)
			}
			body["basis"] = map[string]any{"kind": c.Basis.Kind, "refs": refs, "note": c.Basis.Note}
		}

		id := postClaim(t, ts.URL, tokenFor(holder), body)
		refToClaimID[c.Ref] = id
	}

	claim102, claim880, claim905 := refToClaimID["102"], refToClaimID["880"], refToClaimID["905"]

	// Step 2: first sweep, F1. 102, 880 eff=T (facts with a note); 905 still A (not promoted).
	if err := chk.Sweep(ctx); err != nil {
		t.Fatalf("sweep after import: %v", err)
	}
	assertEff(t, ts.URL, claim102, true)
	assertEff(t, ts.URL, claim880, true)
	assertEff(t, ts.URL, claim905, false) // still A, not eligible for eff=T at all

	stats := getStats(t, ts.URL)
	if stats["claims"] != 3 {
		t.Fatalf("expected 3 claims in stats, got %+v", stats)
	}
	if stats["t_marked"] != 2 {
		t.Fatalf("expected t_marked=2 (102, 880) before promote, got %+v", stats)
	}

	// Step 3: CRO promotes 905 with basis derivation refs=[102, 880] → eff(905)=T.
	promoteBody := map[string]any{
		"move": "promote", "cause_id": "fixture-promote-905",
		"basis": map[string]any{"kind": "derivation", "refs": []int64{claim102, claim880}},
	}
	resp := postMove(t, ts.URL, tokenFor("CRO"), claim905, promoteBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("promote 905: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	if err := chk.Sweep(ctx); err != nil {
		t.Fatalf("sweep after promote: %v", err)
	}
	assertEff(t, ts.URL, claim905, true)

	// Step 5 (F2): Alice demotes 880 → eff(905)=A, premise_demoted flag, failing_id=880.
	demoteBody := map[string]any{
		"move": "demote", "cause_id": "fixture-demote-880",
		"reason": "collateral valuation revised down",
	}
	resp2 := postMove(t, ts.URL, tokenFor("Alice"), claim880, demoteBody)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("demote 880: expected 200, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()

	if err := chk.Sweep(ctx); err != nil {
		t.Fatalf("sweep after demote: %v", err)
	}
	assertEff(t, ts.URL, claim905, false)

	openFlags, err := flagsRepo.ForClaim(ctx, claim905)
	if err != nil {
		t.Fatalf("flags: %v", err)
	}
	if len(openFlags) != 1 {
		t.Fatalf("expected exactly one open flag on 905, got %+v", openFlags)
	}
	if openFlags[0].Kind != eff.CodePremiseDemoted {
		t.Fatalf("expected premise_demoted, got %s", openFlags[0].Kind)
	}
	var evidence map[string]any
	if err := json.Unmarshal(openFlags[0].Evidence, &evidence); err != nil {
		t.Fatalf("unmarshal evidence: %v", err)
	}
	failingID, _ := evidence["failing_id"].(float64)
	if int64(failingID) != claim880 {
		t.Fatalf("expected failing_id=%d (claim 880), got %v", claim880, evidence["failing_id"])
	}
}

func post(t *testing.T, url, token string, body any) (*http.Response, error) {
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
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req)
}

func postClaim(t *testing.T, baseURL, token string, body map[string]any) int64 {
	t.Helper()
	resp, err := post(t, baseURL+"/v1/claims", token, []map[string]any{body})
	if err != nil {
		t.Fatalf("POST /claims: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /claims: expected 200, got %d", resp.StatusCode)
	}
	var out []struct {
		ClaimID int64 `json:"claim_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out[0].ClaimID
}

func postMove(t *testing.T, baseURL, token string, claimID int64, body map[string]any) *http.Response {
	t.Helper()
	resp, err := post(t, baseURL+"/v1/claims/"+strconv.FormatInt(claimID, 10)+"/moves", token, body)
	if err != nil {
		t.Fatalf("POST /moves: %v", err)
	}
	return resp
}

func assertEff(t *testing.T, baseURL string, claimID int64, want bool) {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/claims/" + strconv.FormatInt(claimID, 10))
	if err != nil {
		t.Fatalf("GET claim: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Eff bool `json:"eff"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Eff != want {
		t.Fatalf("claim %d: expected eff=%v, got %v", claimID, want, out.Eff)
	}
}

func getStats(t *testing.T, baseURL string) map[string]int64 {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]int64
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
