package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/subject"
)

type storeOut struct {
	Outcome    string     `json:"outcome"`
	DocID      string     `json:"doc_id"`
	Version    int        `json:"version"`
	PreviousID string     `json:"previous_id"`
	Error      *itemError `json:"error"`
}

func store(t *testing.T, env *testEnv, token string, req storeReq) storeOut {
	t.Helper()
	resp := postJSON(t, env.URL+"/v1/docs", token, req)
	if resp.StatusCode != http.StatusOK {
		body := decode[apiErr](t, resp)
		t.Fatalf("store: %d %s %s", resp.StatusCode, body.Error, body.Detail)
	}
	return decode[storeOut](t, resp)
}

type docOut struct {
	DocID      string    `json:"doc_id"`
	Title      string    `json:"title"`
	Kind       string    `json:"kind"`
	Version    int       `json:"version"`
	Body       string    `json:"body"`
	Refs       []string  `json:"refs"`
	Supersedes *string   `json:"supersedes"`
	CreatedAt  time.Time `json:"created_at"`
}

// spec §5.6: same title → new version, identical newest version → duplicate,
// and the refusals.
func TestStore_VersionsDedupAndRefusals(t *testing.T) {
	env := newTestServer(t)
	tok := env.token(t, "bao", "claude-desktop")

	v1 := store(t, env, tok, storeReq{Title: "plan-mvp-t-knowledge", Kind: "plan", Body: "v1 body",
		Refs: []string{"tk:MVP.phase0.done", "Ravi Kumar", "tk:mvp.phase0.done"}})
	if v1.Outcome != "new" || v1.Version != 1 || !strings.HasPrefix(v1.DocID, "doc_") {
		t.Fatalf("v1: %+v", v1)
	}
	if dup := store(t, env, tok, storeReq{Title: "plan-mvp-t-knowledge", Kind: "plan", Body: "v1 body",
		Refs: []string{"ravi-kumar", "tk:mvp.phase0.done"}}); dup.Outcome != "duplicate" || dup.DocID != v1.DocID || dup.Version != 1 {
		t.Fatalf("duplicate: %+v", dup)
	}
	v2 := store(t, env, tok, storeReq{Title: "Plan MVP  T-Knowledge", Kind: "plan", Body: "v2 body"})
	if v2.Outcome != "supersedes" || v2.Version != 2 || v2.PreviousID != v1.DocID {
		t.Fatalf("v2 (same title, spelled differently): %+v", v2)
	}
	if v3 := store(t, env, tok, storeReq{Title: "plan-mvp-t-knowledge", Kind: "spec", Body: "v2 body"}); v3.Outcome != "supersedes" || v3.Version != 3 {
		t.Fatalf("a kind change is a version, not a duplicate: %+v", v3)
	}

	_, doc := getJSON[docOut](t, env, env.URL+"/v1/docs/"+v1.DocID)
	if !slices.Equal(doc.Refs, []string{"ravi-kumar", "tk:mvp.phase0.done"}) || doc.Body != "v1 body" || doc.Supersedes != nil {
		t.Fatalf("v1 as stored: %+v", doc)
	}
	_, doc2 := getJSON[docOut](t, env, env.URL+"/v1/docs/"+v2.DocID)
	if doc2.Supersedes == nil || *doc2.Supersedes != v1.DocID || doc2.Title != "Plan MVP  T-Knowledge" {
		t.Fatalf("v2 as stored: %+v", doc2)
	}

	for _, c := range []struct {
		req  storeReq
		code string
	}{
		{storeReq{Title: "post-1", Kind: "post", Body: "x"}, "USE_POSTED"},
		{storeReq{Title: "memo-1", Kind: "memo", Body: "x"}, "INVALID_KIND"},
		{storeReq{Title: "  ", Kind: "plan", Body: "x"}, "EMPTY_TITLE"},
		{storeReq{Title: "empty", Kind: "plan", Body: " \n "}, "EMPTY_BODY"},
		{storeReq{Title: "bad-ref", Kind: "plan", Body: "x", Refs: []string{"nope:x.y"}}, "INVALID_REF"},
		{storeReq{Title: "bad-ref", Kind: "plan", Body: "x", Refs: []string{"doc_1#g2"}}, "INVALID_REF"},
	} {
		if out := store(t, env, tok, c.req); out.Outcome != "rejected" || out.Error == nil || out.Error.Code != c.code {
			t.Errorf("%+v: want %s, got %+v", c.req, c.code, out)
		}
	}

	expectErr(t, "store without grant", postJSON(t, env.URL+"/v1/docs", "", storeReq{Title: "x", Kind: "plan", Body: "x"}), 401, "missing_grant")
	var n int
	if err := env.Admin.QueryRow(context.Background(), `SELECT COUNT(*) FROM docs`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("docs rows: %d %v", n, err)
	}
}

// Append-only is enforced by the table itself, for every role.
func TestDocs_AppendOnlyEvenForSuperuser(t *testing.T) {
	env := newTestServer(t)
	store(t, env, env.token(t, "bao", "bao"), storeReq{Title: "rule-a", Kind: "rule", Body: "A1"})
	ctx := context.Background()
	for _, sql := range []string{`UPDATE docs SET body = 'tampered'`, `DELETE FROM docs`, `TRUNCATE docs`} {
		_, err := env.Admin.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: want insufficient_privilege, got %v", sql, err)
		}
	}
}

type findOut struct {
	Docs []struct {
		DocID   string `json:"doc_id"`
		Title   string `json:"title"`
		Kind    string `json:"kind"`
		Version int    `json:"version"`
	} `json:"docs"`
	Conversations []any `json:"conversations"`
	Count         int   `json:"count"`
	Truncated     bool  `json:"truncated"`
}

func TestFindAndRead(t *testing.T) {
	env := newTestServer(t)
	tok := env.token(t, "bao", "claude-desktop")
	r1 := store(t, env, tok, storeReq{Title: "rules-comment-connect-filter", Kind: "rule", Body: "A1. Không comment quá hai lần."})
	r2 := store(t, env, tok, storeReq{Title: "rules-comment-connect-filter", Kind: "rule", Body: "A1. Không comment quá hai lần. A2. 100% trả lời."})
	store(t, env, tok, storeReq{Title: "kalshi-cli-brief", Kind: "analysis", Body: "Builders program notes"})
	store(t, env, tok, storeReq{Title: "plan-x", Kind: "plan", Body: "unrelated"})

	find := func(q string) findOut {
		t.Helper()
		code, out := getJSON[findOut](t, env, env.URL+"/v1/find"+q)
		if code != 200 {
			t.Fatalf("find%s: %d", q, code)
		}
		return out
	}
	if convsOnly := find("?kind=conversation"); convsOnly.Count != 0 || len(convsOnly.Docs) != 0 {
		t.Fatalf("no conversations yet: %+v", convsOnly)
	}
	if all := find(""); all.Count != 4 || all.Conversations == nil || len(all.Conversations) != 0 || all.Truncated {
		t.Fatalf("all: %+v", all)
	}
	if rules := find("?kind=rule&latest_only=true"); rules.Count != 1 || rules.Docs[0].DocID != r2.DocID || rules.Docs[0].Version != 2 {
		t.Fatalf("latest rules: %+v", rules)
	}
	if k := find("?query=KALSHI"); k.Count != 1 || k.Docs[0].Title != "kalshi-cli-brief" {
		t.Fatalf("title match: %+v", k)
	}
	if b := find("?query=builders%20PROGRAM"); b.Count != 1 {
		t.Fatalf("body match: %+v", b)
	}
	if pct := find("?query=100%25"); pct.Count != 1 {
		t.Fatalf("literal %% in query: %+v", pct)
	}
	if none := find("?query=%25"); none.Count != 1 {
		t.Fatalf("%% is literal, not a wildcard (only r2 contains it): %+v", none)
	}
	if future := find("?since=" + time.Now().AddDate(0, 0, 2).Format("2006-01-02")); future.Count != 0 {
		t.Fatalf("since: %+v", future)
	}
	for q, want := range map[string]struct {
		status int
		code   string
	}{
		"?kind=memo":  {400, "bad_kind"},
		"?since=nope": {400, "bad_since"},
	} {
		if code, e := getJSON[apiErr](t, env, env.URL+"/v1/find"+q); code != want.status || e.Error != want.code {
			t.Errorf("find%s: %d %s", q, code, e.Error)
		}
	}

	code, doc := getJSON[docOut](t, env, env.URL+"/v1/docs/"+r2.DocID)
	if code != 200 || doc.Version != 2 || !strings.Contains(doc.Body, "A2.") || doc.Supersedes == nil || *doc.Supersedes != r1.DocID {
		t.Fatalf("read: %d %+v", code, doc)
	}
	if code, _ := getJSON[apiErr](t, env, env.URL+"/v1/docs/doc_999999"); code != 404 {
		t.Fatalf("missing doc: %d", code)
	}
	if code, _ := getJSON[apiErr](t, env, env.URL+"/v1/docs/abc"); code != 400 {
		t.Fatalf("bad doc id: %d", code)
	}
	for _, path := range []string{"/v1/find", "/v1/docs/" + r2.DocID, "/v1/open", "/v1/history?subject=tk:a.b"} {
		resp, err := http.Get(env.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a grant: %d", path, resp.StatusCode)
		}
	}
	writeOnly := env.token(t, "bao", "bao", "tk:write")
	req, _ := http.NewRequest(http.MethodGet, env.URL+"/v1/find", nil)
	req.Header.Set("Authorization", "Bearer "+writeOnly)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("find with tk:write only: %d", resp.StatusCode)
	}
}

// A doc_… basis must name a stored document — at admit and at /moves.
func TestBasis_DocumentMustExist(t *testing.T) {
	env := newTestServer(t)
	ctx := context.Background()
	agent := env.token(t, "bao", "claude-desktop")
	owner := env.token(t, "bao", "bao")

	if r := admit(t, env, agent, admitItem{Subject: "tk:mvp.phase0", Statement: "Phase 0 done", Label: "T", Basis: "doc_999999#g3"})[0]; r.Error == nil || r.Error.Code != "BASIS_NOT_FOUND" {
		t.Fatalf("admit on a missing doc: %+v", r)
	}
	var rows int
	if err := env.Admin.QueryRow(ctx, `SELECT COUNT(*) FROM ledger`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused admit wrote %d rows (%v)", rows, err)
	}

	d := store(t, env, owner, storeReq{Title: "plan-mvp", Kind: "plan", Body: "phase 0: done"})
	if r := admit(t, env, agent, admitItem{Subject: "tk:mvp.phase0", Statement: "Phase 0 done", Label: "T", Basis: d.DocID + "#phase0"})[0]; r.Outcome != "new" || !r.NeedsPromote {
		t.Fatalf("admit on a stored doc: %+v", r)
	}

	a := admit(t, env, agent, admitItem{Subject: "tracker:d4.band", Statement: "50–60k", Label: "A"})[0]
	missing := &ledger.Basis{Kind: "fact", Note: "doc_999999 @2026-09-14"}
	if code, e := moveAs(t, env, owner, a.ClaimID, "promote", missing); code != 400 || e != "basis_not_found" {
		t.Fatalf("promote on a missing doc: %d %s", code, e)
	}
	id, _ := subject.ParseDocID(d.DocID)
	present := &ledger.Basis{Kind: "fact", Note: subject.FormatDocID(id) + "#band @2026-09-14"}
	if code, e := moveAs(t, env, owner, a.ClaimID, "promote", present); code != 200 {
		t.Fatalf("promote on a stored doc: %d %s", code, e)
	}
}
