package kb

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/ccclient"
	"github.com/ankayma/tknowledge-personal/internal/subject"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// bao, acting through claude-desktop, against a real tk-api and a recording
// cause-channel.
func setup(t *testing.T) (*Service, *tkclient.Client, *testapi.Env, *testapi.FakeCC) {
	t.Helper()
	env := testapi.Start(t)
	cc := testapi.StartCC(t)
	tk := tkclient.New(env.Server.URL).WithToken(env.Token(t, "bao", "claude-desktop"))
	return &Service{CC: ccclient.New(cc.URL)}, tk, env, cc
}

func wantCode(t *testing.T, label string, err error, code string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("%s: want %s, got %v", label, code, err)
	}
}

func one(t *testing.T, svc *Service, tk *tkclient.Client, item tkclient.AdmitItem) AdmitResult {
	t.Helper()
	out, err := svc.Admit(context.Background(), tk, AdmitInput{Claims: []tkclient.AdmitItem{item}})
	if err != nil || len(out.Results) != 1 {
		t.Fatalf("admit: %v %+v", err, out)
	}
	return out.Results[0]
}

func TestAdmitT_ProposeThenCauseThenPromote(t *testing.T) {
	svc, tk, env, cc := setup(t)
	ctx := context.Background()

	r := one(t, svc, tk, tkclient.AdmitItem{Subject: "tk:mvp.phase0", Statement: "Phase 0 done", Label: "T", Basis: "chat:c1#1", ChatID: "c1"})
	if r.Outcome != "new" || r.Error != nil {
		t.Fatalf("admit T: %+v", r)
	}
	var claimID int64
	var mark, move, producer, subj, chat string
	var signer, causeID *string
	if err := env.Admin.QueryRow(ctx, `SELECT claim_id, mark, move::text, signer, producer, cause_id, subject, chat_id FROM ledger WHERE id = $1`,
		mustID(t, r.ID)).Scan(&claimID, &mark, &move, &signer, &producer, &causeID, &subj, &chat); err != nil {
		t.Fatal(err)
	}
	if mark != "T" || move != "promote" || signer == nil || *signer != "bao" || producer != "claude-desktop" ||
		causeID == nil || *causeID != "01CAUSE1" || subj != "tk:mvp.phase0" || chat != "c1" {
		t.Fatalf("T row: %s %s %v %s %v %s %s", mark, move, signer, producer, causeID, subj, chat)
	}
	causes := cc.Causes()
	if len(causes) != 1 || causes[0]["approver"] != "bao" || causes[0]["producer"] != "claude-desktop" ||
		causes[0]["actor"] != Actor || causes[0]["effect_type"] != "ledger_move" || causes[0]["scope"] != "claim:"+itoa(claimID) {
		t.Fatalf("cause: %v", causes)
	}
	raw, err := tk.GetClaim(ctx, claimID)
	if err != nil {
		t.Fatal(err)
	}
	var claim struct {
		Eff bool `json:"eff"`
	}
	if json.Unmarshal(raw, &claim) != nil || !claim.Eff {
		t.Fatalf("an admitted T with basis, signer and producer apart is eff=T: %s", raw)
	}

	// The same claim again: duplicate, and no cause.
	if d := one(t, svc, tk, tkclient.AdmitItem{Subject: "tk:mvp.phase0", Statement: "phase 0 done.", Label: "T", Basis: "chat:c1#9"}); d.Outcome != "duplicate" || d.ID != r.ID {
		t.Fatalf("duplicate: %+v", d)
	}
	if len(cc.Causes()) != 1 {
		t.Fatalf("a duplicate must not issue a cause: %d", len(cc.Causes()))
	}
}

func TestAdmitT_CauseFailureLeavesClaimA(t *testing.T) {
	svc, tk, env, cc := setup(t)
	ctx := context.Background()
	cc.SetFail(true)

	r := one(t, svc, tk, tkclient.AdmitItem{Subject: "tk:mvp.phase1", Statement: "Phase 1 done", Label: "T", Basis: "chat:c2#1"})
	if r.Outcome != "rejected" || r.Error == nil || r.Error.Code != "PROMOTE_FAILED" || r.ID == "" {
		t.Fatalf("failed promote: %+v", r)
	}
	var tRows int
	if err := env.Admin.QueryRow(ctx, `SELECT COUNT(*) FROM ledger WHERE mark = 'T'`).Scan(&tRows); err != nil || tRows != 0 {
		t.Fatalf("no T row may exist: %d %v", tRows, err)
	}

	// Once cause-channel is back, the A written above is promoted by id.
	cc.SetFail(false)
	p, err := svc.Promote(ctx, tk, PromoteInput{ClaimID: r.ID, Basis: "chat:c2#1"})
	if err != nil || p.Outcome != "promoted" || p.PreviousID != r.ID {
		t.Fatalf("promote after recovery: %+v %v", p, err)
	}
}

func TestPromote_RefusedBeforeAnyCause(t *testing.T) {
	svc, tk, _, cc := setup(t)
	ctx := context.Background()

	a := one(t, svc, tk, tkclient.AdmitItem{Subject: "tracker:d4.band", Statement: "50–60k", Label: "A"})
	_, err := svc.Promote(ctx, tk, PromoteInput{ClaimID: a.ID})
	wantCode(t, "no basis", err, "T_WITHOUT_BASIS")
	_, err = svc.Promote(ctx, tk, PromoteInput{ClaimID: a.ID, Basis: "I think so"})
	wantCode(t, "bad basis", err, "INVALID_BASIS")
	_, err = svc.Promote(ctx, tk, PromoteInput{ClaimID: "clm_999999", Basis: "chat:c3#2"})
	wantCode(t, "missing claim", err, "NOT_FOUND")
	_, err = svc.Promote(ctx, tk, PromoteInput{ClaimID: "nonsense", Basis: "chat:c3#2"})
	wantCode(t, "bad id", err, "NOT_FOUND")
	if len(cc.Causes()) != 0 {
		t.Fatalf("refusals issued causes: %d", len(cc.Causes()))
	}

	p, err := svc.Promote(ctx, tk, PromoteInput{ClaimID: a.ID, Basis: "chat:c3#1"})
	if err != nil || p.Outcome != "promoted" || p.PreviousID != a.ID || p.ID == a.ID {
		t.Fatalf("promote: %+v %v", p, err)
	}
	_, err = svc.Promote(ctx, tk, PromoteInput{ClaimID: p.ID, Basis: "chat:c3#2"})
	wantCode(t, "already T", err, "ALREADY_T")

	v1 := one(t, svc, tk, tkclient.AdmitItem{Subject: "tracker:x.y", Statement: "one", Label: "A"})
	one(t, svc, tk, tkclient.AdmitItem{Subject: "tracker:x.y", Statement: "two", Label: "A"})
	_, err = svc.Promote(ctx, tk, PromoteInput{ClaimID: v1.ID, Basis: "chat:c3#2"})
	wantCode(t, "superseded", err, "SUPERSEDED")
	if len(cc.Causes()) != 1 {
		t.Fatalf("only the one real promote issues a cause: %d", len(cc.Causes()))
	}
}

func TestClose_OpenAB(t *testing.T) {
	svc, tk, _, cc := setup(t)
	ctx := context.Background()

	q := one(t, svc, tk, tkclient.AdmitItem{Subject: "tracker:capital-com-068.reply-sent", Statement: "Reply Helena gửi chưa", Label: "A-b"})
	openCount := func() int {
		t.Helper()
		raw, err := svc.Open(ctx, tk, OpenInput{Labels: []string{"A-b"}})
		if err != nil {
			t.Fatal(err)
		}
		var o struct {
			Count int `json:"count"`
		}
		_ = json.Unmarshal(raw, &o)
		return o.Count
	}
	if openCount() != 1 {
		t.Fatal("the question should be open")
	}

	_, err := svc.Close(ctx, tk, CloseInput{ClaimID: q.ID, Status: "done"})
	wantCode(t, "bad status", err, "INVALID_STATUS")
	c, err := svc.Close(ctx, tk, CloseInput{ClaimID: q.ID, Status: "resolved", Note: "reply đã gửi 12/9"})
	if err != nil || c.Outcome != "closed" || c.PreviousID != q.ID {
		t.Fatalf("close: %+v %v", c, err)
	}
	if openCount() != 0 {
		t.Fatal("a closed question is not open")
	}
	_, err = svc.Close(ctx, tk, CloseInput{ClaimID: q.ID, Status: "dropped"})
	wantCode(t, "closed twice", err, "NOT_A_B")
	a := one(t, svc, tk, tkclient.AdmitItem{Subject: "tracker:d5.status", Statement: "applied", Label: "A"})
	_, err = svc.Close(ctx, tk, CloseInput{ClaimID: a.ID, Status: "resolved"})
	wantCode(t, "plain A", err, "NOT_A_B")
	if len(cc.Causes()) != 1 {
		t.Fatalf("causes: %d", len(cc.Causes()))
	}

	raw, err := svc.History(ctx, tk, HistoryInput{Subject: "tracker:capital-com-068.reply-sent"})
	if err != nil || !strings.Contains(string(raw), `"move":"resolve"`) {
		t.Fatalf("history: %s %v", raw, err)
	}
}

func TestErrors_GrantProblemsSurface(t *testing.T) {
	svc, tk, _, _ := setup(t)
	_, err := svc.Admit(context.Background(), tk.WithToken(""), AdmitInput{Claims: []tkclient.AdmitItem{{Subject: "tk:a.b", Statement: "x", Label: "A"}}})
	wantCode(t, "no grant", err, "MISSING_GRANT")
	_, err = svc.Admit(context.Background(), tk, AdmitInput{})
	wantCode(t, "empty", err, "BAD_REQUEST")
}

func mustID(t *testing.T, id string) int64 {
	t.Helper()
	n, err := subject.ParseID(id)
	if err != nil {
		t.Fatalf("%q: %v", id, err)
	}
	return n
}

func itoa(n int64) string { return subject.FormatID(n)[len("clm_"):] }
