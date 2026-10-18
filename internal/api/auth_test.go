package api

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"testing"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
)

type apiErr struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

func expectErr(t *testing.T, label string, resp *http.Response, status int, code string) {
	t.Helper()
	body := decode[apiErr](t, resp)
	if resp.StatusCode != status || body.Error != code {
		t.Fatalf("%s: want %d %s, got %d %s (%s)", label, status, code, resp.StatusCode, body.Error, body.Detail)
	}
}

// SPEC §13: no write without a valid, unrevoked grant carrying tk:write.
// Reads stay open.
func TestGrant_WritesRequireValidGrant(t *testing.T) {
	env := newTestServer(t)
	proposal := []claimReq{{Statement: "x", Mark: "A"}}
	_, otherKey, _ := ed25519.GenerateKey(nil)
	base := grant.Grant{UserID: "owner", ActorID: "claude-desktop", Aud: testAud}
	with := func(mut func(*grant.Grant)) grant.Grant { g := base; mut(&g); return g }

	cases := []struct {
		label  string
		token  string
		status int
		code   string
	}{
		{"no grant", "", 401, "missing_grant"},
		{"garbage", "not-a-grant", 401, "invalid_grant"},
		{"untrusted key", signGrant(t, otherKey, base), 401, "invalid_grant"},
		{"wrong purpose", signGrant(t, env.Key, with(func(g *grant.Grant) { g.Purpose = "agent-session-v1" })), 401, "invalid_grant"},
		{"wrong audience", signGrant(t, env.Key, with(func(g *grant.Grant) { g.Aud = "someone-else" })), 401, "invalid_grant"},
		{"expired", signGrant(t, env.Key, with(func(g *grant.Grant) { g.ExpiresAt = time.Now().Add(-time.Minute).Unix() })), 401, "invalid_grant"},
		{"read-only scope", signGrant(t, env.Key, with(func(g *grant.Grant) { g.Scope = []string{grant.ScopeRead} })), 403, "insufficient_scope"},
	}
	for _, c := range cases {
		expectErr(t, c.label, postJSON(t, env.URL+"/v1/claims", c.token, proposal), c.status, c.code)
	}

	var rows int
	if err := env.Admin.QueryRow(context.Background(), `SELECT COUNT(*) FROM ledger`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("refused writes left %d rows", rows)
	}

	stats, err := http.Get(env.URL + "/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	stats.Body.Close()
	if stats.StatusCode != http.StatusOK {
		t.Fatalf("reads must stay open, GET /stats = %d", stats.StatusCode)
	}
}

// signer is the grant's user and producer the grant's actor; a request can
// repeat them but not replace them — except a person naming the agent whose
// draft they sign.
func TestGrant_IdentityComesFromGrant(t *testing.T) {
	env := newTestServer(t)
	ctx := context.Background()
	owner := env.token(t, "owner", "owner")
	agent := env.token(t, "owner", "claude-desktop")
	cause := "c1"
	fact := &ledger.Basis{Kind: "fact", Note: "src 2026-09-14"}
	mallory, loanAgent := "mallory", "loan-agent"

	// Someone else's name as signer or producer is refused.
	expectErr(t, "signer mismatch", postJSON(t, env.URL+"/v1/claims", owner, []claimReq{
		{Statement: "x", Mark: "T", Basis: fact, CauseID: &cause, Signer: &mallory},
	}), 403, "signer_mismatch")
	expectErr(t, "agent names another producer", postJSON(t, env.URL+"/v1/claims", agent, []claimReq{
		{Statement: "x", Mark: "A", Producer: loanAgent},
	}), 403, "producer_mismatch")
	expectErr(t, "proposal with a signer", postJSON(t, env.URL+"/v1/claims", agent, []claimReq{
		{Statement: "x", Mark: "A", Signer: &mallory},
	}), 400, "signer_on_proposal")

	type row struct {
		signer, producer, grantID *string
	}
	readRow := func(id int64) row {
		t.Helper()
		var r row
		if err := env.Admin.QueryRow(ctx, `SELECT signer, producer, grant_id FROM ledger WHERE id = $1`, id).
			Scan(&r.signer, &r.producer, &r.grantID); err != nil {
			t.Fatal(err)
		}
		return r
	}
	str := func(p *string) string { return deref(p) }

	// Agent proposal: producer = actor, no signer, grant recorded.
	id := decode[[]claimIDResp](t, postJSON(t, env.URL+"/v1/claims", agent, []claimReq{{Statement: "drafted", Mark: "A"}}))[0].ClaimID
	if r := readRow(id); r.signer != nil || str(r.producer) != "claude-desktop" || str(r.grantID) == "" {
		t.Fatalf("agent proposal row: signer=%v producer=%s grant=%s", r.signer, str(r.producer), str(r.grantID))
	}

	// A person may record that an agent drafted what they propose.
	id2 := decode[[]claimIDResp](t, postJSON(t, env.URL+"/v1/claims", owner, []claimReq{{Statement: "by agent", Mark: "A", Producer: loanAgent}}))[0].ClaimID
	if r := readRow(id2); str(r.producer) != "loan-agent" {
		t.Fatalf("human-declared producer: %s", str(r.producer))
	}

	// A move under the agent's grant: signer = owner (the person), producer = the agent.
	mv := postJSON(t, movesURL(env.URL, id), agent, moveReq{Move: "promote", CauseID: &cause, Basis: fact})
	moved := decode[map[string]int64](t, mv)
	if mv.StatusCode != http.StatusOK {
		t.Fatalf("agent-grant promote: %d", mv.StatusCode)
	}
	if r := readRow(moved["id"]); str(r.signer) != "owner" || str(r.producer) != "claude-desktop" {
		t.Fatalf("agent-grant move row: signer=%s producer=%s", str(r.signer), str(r.producer))
	}

	// Repeating the grant's own user as signer is fine.
	ownerName := "owner"
	ok := postJSON(t, env.URL+"/v1/claims", owner, []claimReq{{Statement: "y", Mark: "T", Basis: fact, CauseID: &cause, Signer: &ownerName}})
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("explicit matching signer: %d", ok.StatusCode)
	}
}

func TestGrant_WhoamiAndRevoke(t *testing.T) {
	env := newTestServer(t)
	get := func(token string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, env.URL+"/v1/whoami", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	expectErr(t, "whoami without grant", get(""), 401, "missing_grant")

	victim := env.token(t, "bao", "claude-desktop")
	me := decode[grant.Grant](t, get(victim))
	if me.UserID != "bao" || me.ActorID != "claude-desktop" || me.GrantID == "" {
		t.Fatalf("whoami: %+v", me)
	}

	revoke := map[string]string{"grant_id": me.GrantID, "reason": "laptop lost"}
	expectErr(t, "revoke without tk:admin", postJSON(t, env.URL+"/v1/grants/revoke", victim, revoke), 403, "insufficient_scope")

	admin := env.token(t, "bao", "bao", grant.ScopeAdmin)
	for i := 0; i < 2; i++ { // revoking twice is a no-op, not an error
		r := postJSON(t, env.URL+"/v1/grants/revoke", admin, revoke)
		r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("revoke #%d: %d", i+1, r.StatusCode)
		}
	}

	expectErr(t, "write with revoked grant", postJSON(t, env.URL+"/v1/claims", victim, []claimReq{{Statement: "x", Mark: "A"}}), 401, "grant_revoked")
	expectErr(t, "whoami with revoked grant", get(victim), 401, "grant_revoked")
}
