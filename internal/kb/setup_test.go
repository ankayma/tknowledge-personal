package kb

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestSetupApply_PromotesAdoptionClaims(t *testing.T) {
	svc, tk, env, cc := setup(t)
	ctx := context.Background()

	// demo writes one rule adoption and one claim per principle of its list.
	out, err := svc.SetupApply(ctx, tk, SetupApplyInput{TemplateID: "demo"})
	if err != nil || len(out.Docs) != 2 || !slices.Equal(out.NamespacesAdded, []string{"demo-ns"}) || len(out.Claims) != 3 {
		t.Fatalf("apply: %+v %v", out, err)
	}
	for _, id := range out.Claims {
		var mark, signer, note string
		if err := env.Admin.QueryRow(ctx, `SELECT mark, signer, basis->>'note' FROM ledger WHERE id = $1`, mustID(t, id)).Scan(&mark, &signer, &note); err != nil ||
			mark != "T" || signer != "bao" || !strings.HasPrefix(note, "template:demo@1 @") {
			t.Fatalf("adoption claim %s: %s %s %s %v", id, mark, signer, note, err)
		}
	}
	if len(cc.Causes()) != 3 {
		t.Fatalf("three T rows, three causes: %d", len(cc.Causes()))
	}

	again, err := svc.SetupApply(ctx, tk, SetupApplyInput{TemplateID: "demo"})
	if err != nil || !slices.Equal(again.Claims, out.Claims) || len(cc.Causes()) != 3 {
		t.Fatalf("apply again: %+v %v causes=%d", again, err, len(cc.Causes()))
	}

	inc, err := svc.SetupApply(ctx, tk, SetupApplyInput{TemplateID: "incomplete"})
	if err != nil || len(inc.Docs) != 0 || !slices.Equal(inc.Missing, []string{"missing-rules"}) || len(cc.Causes()) != 3 {
		t.Fatalf("incomplete: %+v %v", inc, err)
	}

	if raw, err := svc.SetupStatus(ctx, tk); err != nil || !strings.Contains(string(raw), `"template_applied":"incomplete"`) {
		t.Fatalf("status: %s %v", raw, err)
	}
	if raw, err := svc.SetupTemplates(ctx, tk); err != nil || !strings.Contains(string(raw), `"id":"job-search"`) {
		t.Fatalf("templates: %s %v", raw, err)
	}
	// The owner revises one principle: its own series gets a version, the
	// document is redrawn, and the other principles are untouched.
	set, err := svc.PrinciplesSet(ctx, tk, PrinciplesSetInput{Items: []PrincipleItem{
		{Code: "P.2", Statement: "Honest gaps over false promises, always.", Note: "một giới hạn giấu đi thì đắt hơn"},
		{Code: "P.9", Statement: "Principle and facts above assumption."},
	}})
	if err != nil || len(set.Principles) != 3 || set.Principles[0].Outcome != "supersedes" || set.Principles[2].Outcome != "new" || set.DocID == "" {
		t.Fatalf("principles set: %+v %v", set, err)
	}
	raw, err := svc.Principles(ctx, tk)
	if err != nil || !strings.Contains(string(raw), `"P.9"`) || !strings.Contains(string(raw), "Honest gaps over false promises, always.") ||
		!strings.Contains(string(raw), "một giới hạn giấu đi thì đắt hơn") {
		t.Fatalf("principles: %s %v", raw, err)
	}
}
