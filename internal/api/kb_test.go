package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/subject"
)

func admit(t *testing.T, env *testEnv, token string, items ...admitItem) []admitResult {
	t.Helper()
	resp := postJSON(t, env.URL+"/v1/admit", token, map[string]any{"claims": items})
	if resp.StatusCode != http.StatusOK {
		body := decode[apiErr](t, resp)
		t.Fatalf("admit: %d %s %s", resp.StatusCode, body.Error, body.Detail)
	}
	return decode[struct {
		Results []admitResult `json:"results"`
	}](t, resp).Results
}

// getJSON reads under a tk:read grant (the knowledge reads require one).
func getJSON[T any](t *testing.T, env *testEnv, url string) (int, T) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+env.token(t, "reader", "reader"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, decode[T](t, resp)
}

func moveAs(t *testing.T, env *testEnv, token string, claimID int64, move string, basis *ledger.Basis) (int, string) {
	t.Helper()
	cause := "c-" + move
	resp := postJSON(t, movesURL(env.URL, claimID), token, moveReq{Move: move, CauseID: &cause, Basis: basis})
	if resp.StatusCode == http.StatusOK {
		resp.Body.Close()
		return resp.StatusCode, ""
	}
	return resp.StatusCode, decode[apiErr](t, resp).Error
}

func rowID(t *testing.T, id string) int64 {
	t.Helper()
	n, err := subject.ParseID(id)
	if err != nil {
		t.Fatalf("id %q: %v", id, err)
	}
	return n
}

// spec §5.1: outcomes new / duplicate / supersedes / rejected, and every
// rejection code, in one batch — a refused claim does not stop the others.
func TestAdmit_OutcomesAndCodes(t *testing.T) {
	env := newTestServer(t)
	ctx := context.Background()
	agent := env.token(t, "bao", "claude-desktop")

	r := admit(t, env, agent,
		admitItem{Subject: "Tracker:D4.Band", Statement: "Band cho D4 = 60–70k AED/tháng", Label: "A", ChatID: "chat-1"},
		admitItem{Subject: "tracker:d4.band", Statement: "  band cho D4 = 60–70k aed/tháng. ", Label: "a"},
		admitItem{Subject: "tracker:d4.band", Statement: "Band cho D4 = 50–60k AED/tháng", Label: "A"},
		admitItem{Subject: "tk:mvp.phase0", Statement: "Phase 0 done", Label: "T", Basis: "chat:c1#2"},
		admitItem{Subject: "tk:mvp.phase1", Statement: "x", Label: "T"},
		admitItem{Subject: "tk:mvp.phase1", Statement: "x", Label: "T", Basis: "trust me"},
		admitItem{Subject: "nope:x.y", Statement: "x", Label: "A"},
		admitItem{Subject: "no-colon", Statement: "x", Label: "A"},
		admitItem{Subject: "tk:mvp.phase1", Statement: "x", Label: "B"},
		admitItem{Subject: "tk:mvp.phase1", Statement: "  ", Label: "A"},
		admitItem{Subject: "tracker:d4.band", Statement: "Band cho D4 = 50–60k AED/tháng", Label: "T", Basis: "chat:c1#3"},
	)
	want := []struct{ outcome, code string }{
		{"new", ""}, {"duplicate", ""}, {"supersedes", ""}, {"new", ""},
		{"rejected", "T_WITHOUT_BASIS"}, {"rejected", "INVALID_BASIS"}, {"rejected", "UNKNOWN_NAMESPACE"},
		{"rejected", "INVALID_SUBJECT"}, {"rejected", "INVALID_LABEL"}, {"rejected", "EMPTY_STATEMENT"},
		{"rejected", "LABEL_UPGRADE_USE_PROMOTE"},
	}
	if len(r) != len(want) {
		t.Fatalf("results: %d, want %d", len(r), len(want))
	}
	for i, w := range want {
		code := ""
		if r[i].Error != nil {
			code = r[i].Error.Code
		}
		if r[i].Outcome != w.outcome || code != w.code {
			t.Errorf("#%d: %s %s, want %s %s", i, r[i].Outcome, code, w.outcome, w.code)
		}
	}

	if r[0].Subject != "tracker:d4.band" || r[1].ID != r[0].ID {
		t.Fatalf("normalised subject / duplicate id: %+v %+v", r[0], r[1])
	}
	if r[2].PreviousID != r[0].ID || r[2].PreviousStatement != "Band cho D4 = 60–70k AED/tháng" || r[2].ID == r[0].ID {
		t.Fatalf("supersedes: %+v", r[2])
	}
	if !r[3].NeedsPromote || r[3].Basis == nil || r[3].Basis.Note != "chat:c1#2 @"+time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("T admission: %+v", r[3])
	}
	if r[10].ID != r[2].ID {
		t.Fatalf("upgrade refusal should point at the A to promote: %+v", r[10])
	}

	var n int
	if err := env.Admin.QueryRow(ctx, `SELECT COUNT(*) FROM ledger`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows written: %d %v (want 3: new, supersedes, T propose)", n, err)
	}

	var subj, owner, scope, chat, producer, move, mark string
	var ab bool
	var signer, grantID *string
	var supersedes *int64
	if err := env.Admin.QueryRow(ctx, `SELECT subject, owner, scope, chat_id, producer, signer, grant_id, ab FROM ledger WHERE id = $1`,
		rowID(t, r[0].ID)).Scan(&subj, &owner, &scope, &chat, &producer, &signer, &grantID, &ab); err != nil {
		t.Fatal(err)
	}
	if subj != "tracker:d4.band" || owner != "bao" || scope != "user:bao" || chat != "chat-1" || producer != "claude-desktop" || signer != nil || grantID == nil || ab {
		t.Fatalf("new row: %s %s %s %s %s %v %v %v", subj, owner, scope, chat, producer, signer, grantID, ab)
	}
	if err := env.Admin.QueryRow(ctx, `SELECT supersedes FROM ledger WHERE id = $1`, rowID(t, r[2].ID)).Scan(&supersedes); err != nil ||
		supersedes == nil || *supersedes != rowID(t, r[0].ID) {
		t.Fatalf("supersedes column: %v %v", supersedes, err)
	}
	if err := env.Admin.QueryRow(ctx, `SELECT move::text, mark FROM ledger WHERE id = $1`, rowID(t, r[3].ID)).Scan(&move, &mark); err != nil ||
		move != "propose" || mark != "A" {
		t.Fatalf("T admission is a propose row until promoted: %s %s %v", move, mark, err)
	}
}

func TestAdmit_LabelRules(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	owner := env.token(t, "bao", "bao")
	fact := &ledger.Basis{Kind: "fact", Note: "chat:c1#1 @2026-09-14"}

	// A-b → same statement as A: a new version (label change inside the A family).
	q := admit(t, env, agent, admitItem{Subject: "tracker:x.reply", Statement: "Reply gửi chưa", Label: "A-b"})[0]
	a := admit(t, env, agent, admitItem{Subject: "tracker:x.reply", Statement: "Reply gửi chưa", Label: "A"})[0]
	if a.Outcome != "supersedes" || a.PreviousID != q.ID {
		t.Fatalf("A-b → A: %+v", a)
	}

	// T on the subject: same statement again as T is a duplicate, as A is refused.
	tNew := admit(t, env, agent, admitItem{Subject: "tk:mvp.done", Statement: "MVP done", Label: "T", Basis: "chat:c1#1"})[0]
	if code, e := moveAs(t, env, owner, tNew.ClaimID, "promote", fact); code != http.StatusOK {
		t.Fatalf("promote: %d %s", code, e)
	}
	if d := admit(t, env, agent, admitItem{Subject: "tk:mvp.done", Statement: "MVP done.", Label: "T", Basis: "chat:c1#2"})[0]; d.Outcome != "duplicate" {
		t.Fatalf("T again: %+v", d)
	}
	if d := admit(t, env, agent, admitItem{Subject: "tk:mvp.done", Statement: "MVP done", Label: "A"})[0]; d.Error == nil || d.Error.Code != "LABEL_DOWNGRADE_NOT_ADMITTED" {
		t.Fatalf("T → A: %+v", d)
	}

	// A closed question asked again is a new version, not a duplicate of the closed one.
	c := admit(t, env, agent, admitItem{Subject: "tracker:y.sent", Statement: "Đã gửi chưa", Label: "A-b"})[0]
	if code, e := moveAs(t, env, owner, c.ClaimID, "resolve", nil); code != http.StatusOK {
		t.Fatalf("resolve: %d %s", code, e)
	}
	if again := admit(t, env, agent, admitItem{Subject: "tracker:y.sent", Statement: "Đã gửi chưa", Label: "A-b"})[0]; again.Outcome != "supersedes" {
		t.Fatalf("re-asked closed A-b: %+v", again)
	}
}

func TestMoves_CloseAndSupersededClaims(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	owner := env.token(t, "bao", "bao")
	ctx := context.Background()

	q := admit(t, env, agent, admitItem{Subject: "person:paul.reply", Statement: "Paul trả lời chưa", Label: "A-b", Owner: "paul", ChatID: "c9"})[0]
	if code, e := moveAs(t, env, owner, q.ClaimID, "resolve", nil); code != http.StatusOK {
		t.Fatalf("resolve: %d %s", code, e)
	}
	var subj, ownerCol, chat, mark string
	var ab bool
	if err := env.Admin.QueryRow(ctx, `SELECT subject, owner, chat_id, ab, mark FROM ledger WHERE claim_id = $1 ORDER BY id DESC LIMIT 1`, q.ClaimID).
		Scan(&subj, &ownerCol, &chat, &ab, &mark); err != nil {
		t.Fatal(err)
	}
	if subj != "person:paul.reply" || ownerCol != "paul" || chat != "c9" || !ab || mark != "A" {
		t.Fatalf("resolve row must copy the claim's attributes: %s %s %s %v %s", subj, ownerCol, chat, ab, mark)
	}
	if code, body := getJSON[claimResp](t, env, env.URL+"/v1/claims/"+strconv.FormatInt(q.ClaimID, 10)); code != 200 || body.Status != "resolved" {
		t.Fatalf("status after resolve: %d %s", code, body.Status)
	}
	if code, e := moveAs(t, env, owner, q.ClaimID, "drop", nil); code != http.StatusConflict || e != "invalid_move_for_state" {
		t.Fatalf("closing a closed A-b: %d %s", code, e)
	}
	plain := admit(t, env, agent, admitItem{Subject: "person:paul.tier", Statement: "thesis-ally", Label: "A"})[0]
	if code, _ := moveAs(t, env, owner, plain.ClaimID, "resolve", nil); code != http.StatusConflict {
		t.Fatalf("resolve on a plain A: %d", code)
	}

	// A superseded claim is frozen.
	old := admit(t, env, agent, admitItem{Subject: "tracker:d4.band", Statement: "60–70k", Label: "A"})[0]
	cur := admit(t, env, agent, admitItem{Subject: "tracker:d4.band", Statement: "50–60k", Label: "A"})[0]
	fact := &ledger.Basis{Kind: "fact", Note: "chat:c1#1 @2026-09-14"}
	if code, e := moveAs(t, env, owner, old.ClaimID, "promote", fact); code != http.StatusConflict || e != "claim_superseded" {
		t.Fatalf("move on superseded claim: %d %s", code, e)
	}
	if code, e := moveAs(t, env, owner, cur.ClaimID, "promote", fact); code != http.StatusOK {
		t.Fatalf("move on current claim: %d %s", code, e)
	}
}

type openResp struct {
	Claims    []openClaim `json:"claims"`
	Count     int         `json:"count"`
	Truncated bool        `json:"truncated"`
}

func TestOpen_WindowScopeLabels(t *testing.T) {
	env := newTestServer(t)
	ctx := context.Background()

	// Rows with a past ts can only be written behind the API (the API stamps
	// now()); the superuser inserts them, as a migration would.
	insert := func(claimID int64, subj, statement, mark string, ab bool, move string, ageDays int) int64 {
		t.Helper()
		var id int64
		err := env.Admin.QueryRow(ctx, `
			WITH n AS (SELECT nextval('ledger_id_seq') AS id)
			INSERT INTO ledger (id, claim_id, statement, mark, producer, move, subject, ab, ts)
			SELECT n.id, CASE WHEN $1::bigint = 0 THEN n.id ELSE $1::bigint END, $2, $3, 'test', $4::move_t, $5, $6,
			       now() - make_interval(days => $7)
			FROM n RETURNING id`, claimID, statement, mark, move, subj, ab, ageDays).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert(0, "tracker:a.old", "old A", "A", false, "propose", 10)
	insert(0, "tracker:b.old-q", "old question", "A", true, "propose", 10)
	insert(0, "tracker:c.new", "new A", "A", false, "propose", 1)
	insert(0, "tk:d.done", "new T", "T", false, "import", 1)
	closed := insert(0, "tracker:e.closed", "closed question", "A", true, "propose", 1)
	insert(closed, "tracker:e.closed", "closed question", "A", true, "resolve", 0)
	insert(0, "tracker:f.band", "v1", "A", false, "propose", 2)
	v2 := insert(0, "tracker:f.band", "v2", "A", false, "propose", 1)

	subjects := func(o openResp) string {
		var s []string
		for _, c := range o.Claims {
			s = append(s, c.Subject)
		}
		sort.Strings(s)
		return strings.Join(s, " ")
	}

	code, def := getJSON[openResp](t, env, env.URL+"/v1/open")
	if code != 200 || subjects(def) != "tk:d.done tracker:b.old-q tracker:c.new tracker:f.band" || def.Count != 4 || def.Truncated {
		t.Fatalf("default open: %d %s count=%d", code, subjects(def), def.Count)
	}
	for _, c := range def.Claims {
		if c.Status != "open" {
			t.Fatalf("status: %+v", c)
		}
		if c.Subject == "tracker:f.band" && (c.ID != subject.FormatID(v2) || c.Statement != "v2") {
			t.Fatalf("only the newest version of a subject: %+v", c)
		}
		if c.Subject == "tk:d.done" && c.Label != "T" || c.Subject == "tracker:b.old-q" && c.Label != "A-b" {
			t.Fatalf("label: %+v", c)
		}
	}
	if _, o := getJSON[openResp](t, env, env.URL+"/v1/open?scope=tk:"); subjects(o) != "tk:d.done" {
		t.Fatalf("scope: %s", subjects(o))
	}
	if _, o := getJSON[openResp](t, env, env.URL+"/v1/open?labels=A-b"); subjects(o) != "tracker:b.old-q" {
		t.Fatalf("labels: %s", subjects(o))
	}
	since := time.Now().AddDate(0, 0, -15).Format("2006-01-02")
	if _, o := getJSON[openResp](t, env, env.URL+"/v1/open?since="+since); subjects(o) != "tk:d.done tracker:a.old tracker:b.old-q tracker:c.new tracker:f.band" {
		t.Fatalf("since: %s", subjects(o))
	}
	if code, e := getJSON[apiErr](t, env, env.URL+"/v1/open?labels=B"); code != 400 || e.Error != "bad_labels" {
		t.Fatalf("bad labels: %d %s", code, e.Error)
	}
	if code, e := getJSON[apiErr](t, env, env.URL+"/v1/open?since=yesterday"); code != 400 || e.Error != "bad_since" {
		t.Fatalf("bad since: %d %s", code, e.Error)
	}

	// Ceiling: 200 rows and truncated.
	if _, err := env.Admin.Exec(ctx, `
		INSERT INTO ledger (id, claim_id, statement, mark, producer, move, subject, ab)
		SELECT i, i, 'q', 'A', 'test', 'propose', 'tracker:bulk-' || i || '.x', true
		FROM (SELECT nextval('ledger_id_seq') AS i FROM generate_series(1, 201)) s`); err != nil {
		t.Fatal(err)
	}
	if _, o := getJSON[openResp](t, env, env.URL+"/v1/open?labels=A-b"); o.Count != 200 || !o.Truncated {
		t.Fatalf("ceiling: count=%d truncated=%v", o.Count, o.Truncated)
	}
}

func TestHistoryAndRow(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	owner := env.token(t, "bao", "bao")

	v1 := admit(t, env, agent, admitItem{Subject: "tracker:D4.band", Statement: "60–70k", Label: "A"})[0]
	v2 := admit(t, env, agent, admitItem{Subject: "tracker:d4.band", Statement: "50–60k", Label: "A"})[0]
	if code, e := moveAs(t, env, owner, v2.ClaimID, "promote", &ledger.Basis{Kind: "fact", Note: "chat:c9#4 @2026-09-14"}); code != 200 {
		t.Fatalf("promote: %d %s", code, e)
	}

	type history struct {
		Subject string         `json:"subject"`
		Chain   []historyEntry `json:"chain"`
	}
	code, h := getJSON[history](t, env, env.URL+"/v1/history?subject=Tracker:D4.Band")
	if code != 200 || h.Subject != "tracker:d4.band" || len(h.Chain) != 3 {
		t.Fatalf("history: %d %+v", code, h)
	}
	got := []string{}
	for _, e := range h.Chain {
		got = append(got, e.Label+"/"+e.Move)
	}
	if strings.Join(got, " ") != "A/propose A/propose T/promote" || h.Chain[1].Supersedes != v1.ID || h.Chain[2].Basis != "chat:c9#4" ||
		h.Chain[2].Signer == nil || *h.Chain[2].Signer != "bao" {
		t.Fatalf("chain: %v %+v", got, h.Chain)
	}
	if code, e := getJSON[apiErr](t, env, env.URL+"/v1/history?subject=nonsense"); code != 400 || e.Error != "invalid_subject" {
		t.Fatalf("bad subject: %d %s", code, e.Error)
	}

	type rowResp struct {
		Superseded bool `json:"superseded"`
		Current    struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			Move  string `json:"move"`
		} `json:"current"`
	}
	if _, r := getJSON[rowResp](t, env, env.URL+"/v1/rows/"+strconv.FormatInt(rowID(t, v1.ID), 10)); !r.Superseded || r.Current.Move != "propose" {
		t.Fatalf("row v1: %+v", r)
	}
	if _, r := getJSON[rowResp](t, env, env.URL+"/v1/rows/"+strconv.FormatInt(rowID(t, v2.ID), 10)); r.Superseded || r.Current.Label != "T" || r.Current.ID == v2.ID {
		t.Fatalf("row v2: %+v", r)
	}
	if code, _ := getJSON[apiErr](t, env, env.URL+"/v1/rows/999999"); code != 404 {
		t.Fatalf("missing row: %d", code)
	}
}
