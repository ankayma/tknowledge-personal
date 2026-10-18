package kb

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

func TestDocs_StoreFindRead(t *testing.T) {
	svc, tk, _, _ := setup(t)
	ctx := context.Background()

	d, err := svc.Store(ctx, tk, StoreInput{Title: "rules-comment-connect-filter", Kind: "rule", Body: "A1. Không comment quá hai lần.", Refs: []string{"rule:A1.adopted"}})
	if err != nil || d.Outcome != "new" || d.Version != 1 {
		t.Fatalf("store: %+v %v", d, err)
	}
	_, err = svc.Store(ctx, tk, StoreInput{Title: "p", Kind: "post", Body: "x"})
	wantCode(t, "post", err, "USE_POSTED")

	raw, err := svc.Find(ctx, tk, FindInput{Kind: "rule", LatestOnly: true})
	if err != nil || !strings.Contains(string(raw), d.DocID) {
		t.Fatalf("find: %s %v", raw, err)
	}
	if raw, err := svc.Find(ctx, tk, FindInput{Person: "marco"}); err != nil || !strings.Contains(string(raw), `"count":0`) {
		t.Fatalf("find by an unknown person: %s %v", raw, err)
	}

	raw, err = svc.Read(ctx, tk, ReadInput{DocID: d.DocID})
	var doc struct {
		Body string `json:"body"`
	}
	if err != nil || json.Unmarshal(raw, &doc) != nil || doc.Body != "A1. Không comment quá hai lần." {
		t.Fatalf("read: %s %v", raw, err)
	}
	_, err = svc.Read(ctx, tk, ReadInput{DocID: "doc_999999"})
	wantCode(t, "missing", err, "NOT_FOUND")
	_, err = svc.Read(ctx, tk, ReadInput{DocID: "nonsense"})
	wantCode(t, "bad id", err, "NOT_FOUND")
}

// A doc_… basis naming nothing stored is refused before any cause.
func TestDocBasis_RefusedBeforeCause(t *testing.T) {
	svc, tk, _, cc := setup(t)
	ctx := context.Background()

	r := one(t, svc, tk, tkclient.AdmitItem{Subject: "tk:mvp.phase0", Statement: "Phase 0 done", Label: "T", Basis: "doc_999999#g3"})
	if r.Outcome != "rejected" || r.Error == nil || r.Error.Code != "BASIS_NOT_FOUND" {
		t.Fatalf("admit T on a missing doc: %+v", r)
	}
	a := one(t, svc, tk, tkclient.AdmitItem{Subject: "tracker:d4.band", Statement: "50–60k", Label: "A"})
	_, err := svc.Promote(ctx, tk, PromoteInput{ClaimID: a.ID, Basis: "doc_999999"})
	wantCode(t, "promote on a missing doc", err, "BASIS_NOT_FOUND")
	if len(cc.Causes()) != 0 {
		t.Fatalf("refusals issued causes: %d", len(cc.Causes()))
	}

	d, err := svc.Store(ctx, tk, StoreInput{Title: "offer-d4", Kind: "analysis", Body: "band 50–60k"})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := svc.Promote(ctx, tk, PromoteInput{ClaimID: a.ID, Basis: d.DocID + "#band"}); err != nil || p.Outcome != "promoted" {
		t.Fatalf("promote on a stored doc: %+v %v", p, err)
	}
	if r := one(t, svc, tk, tkclient.AdmitItem{Subject: "tk:mvp.phase0", Statement: "Phase 0 done", Label: "T", Basis: d.DocID}); r.Outcome != "new" || r.Error != nil {
		t.Fatalf("admit T on a stored doc: %+v", r)
	}
	if len(cc.Causes()) != 2 {
		t.Fatalf("two real moves, two causes: %d", len(cc.Causes()))
	}
}
