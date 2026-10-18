package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"
)

type setupStatus struct {
	UserRegistered  bool     `json:"user_registered"`
	Principles      *string  `json:"principles"`
	RulesCount      int      `json:"rules_count"`
	Namespaces      []string `json:"namespaces"`
	ClaimsCount     int      `json:"claims_count"`
	TemplateApplied *string  `json:"template_applied"`
}

type applyOut struct {
	Docs            []string      `json:"docs"`
	NamespacesAdded []string      `json:"namespaces_added"`
	Claims          []admitResult `json:"claims"`
	Missing         []string      `json:"missing"`
}

func apply(t *testing.T, env *testEnv, token string, body map[string]any) applyOut {
	t.Helper()
	resp := postJSON(t, env.URL+"/v1/setup/apply", token, body)
	if resp.StatusCode != http.StatusOK {
		e := decode[apiErr](t, resp)
		t.Fatalf("apply: %d %s %s", resp.StatusCode, e.Error, e.Detail)
	}
	return decode[applyOut](t, resp)
}

// spec §5.13–5.16: an empty store, the templates on offer, an incomplete one
// applied for what it has, a complete one applied through the ordinary primitives and applied
// again as duplicates, and a hand-written principle list.
func TestSetup_TemplatesStatusApplyPrinciples(t *testing.T) {
	env := newTestServer(t)
	ctx := context.Background()
	agent := env.token(t, "bao", "claude-desktop")

	_, st := getJSON[setupStatus](t, env, env.URL+"/v1/setup/status")
	if st.UserRegistered || st.Principles != nil || st.RulesCount != 0 || len(st.Namespaces) != 8 || st.ClaimsCount != 0 || st.TemplateApplied != nil {
		t.Fatalf("empty store: %+v", st)
	}

	type tplOut struct {
		Templates []struct {
			ID              string   `json:"id"`
			Complete        bool     `json:"complete"`
			Missing         []string `json:"missing"`
			PrinciplesCount int      `json:"principles_count"`
			Rules           []string `json:"rules"`
		} `json:"templates"`
	}
	_, tpls := getJSON[tplOut](t, env, env.URL+"/v1/setup/templates")
	byID := map[string]int{}
	for i, tpl := range tpls.Templates {
		byID[tpl.ID] = i
	}
	demo, js, inc := tpls.Templates[byID["demo"]], tpls.Templates[byID["job-search"]], tpls.Templates[byID["incomplete"]]
	if len(tpls.Templates) != 4 || !demo.Complete || demo.PrinciplesCount != 2 || !js.Complete || js.PrinciplesCount != 7 ||
		inc.Complete || len(inc.Missing) != 1 {
		t.Fatalf("templates: %+v", tpls)
	}

	expectErr(t, "unknown template", postJSON(t, env.URL+"/v1/setup/apply", agent, map[string]any{"template_id": "nope"}), 400, "unknown_template")
	if inc := apply(t, env, agent, map[string]any{"template_id": "incomplete"}); !slices.Equal(inc.NamespacesAdded, []string{"never-added"}) ||
		len(inc.Docs) != 0 || len(inc.Claims) != 0 || !slices.Equal(inc.Missing, []string{"missing-rules"}) {
		t.Fatalf("incomplete: %+v", inc)
	}
	if count(t, env, "docs")+count(t, env, "ledger") != 0 {
		t.Fatal("a missing file wrote a document or claim")
	}

	first := apply(t, env, agent, map[string]any{"template_id": "demo", "overrides": map[string]string{"owner": "team-bao"}})
	if !slices.Equal(first.NamespacesAdded, []string{"demo-ns"}) || len(first.Docs) != 2 || len(first.Claims) != 3 || len(first.Missing) != 0 {
		t.Fatalf("apply: %+v", first)
	}
	today := time.Now().UTC().Format("2006-01-02")
	// the rule document is adopted; the principle list becomes one claim per
	// code — its own trust class (P.9), not a document adoption.
	for i, subj := range []string{"rule:demo-rules.adopted", "principle:p1.statement", "principle:p2.statement"} {
		c := first.Claims[i]
		if c.Subject != subj || c.Outcome != "new" || !c.NeedsPromote || c.Basis == nil || c.Basis.Note != "template:demo@1 @"+today {
			t.Fatalf("claim %d: %+v", i, c)
		}
	}
	var owner string
	if err := env.Admin.QueryRow(ctx, `SELECT owner FROM ledger WHERE id = $1`, first.Claims[0].ClaimID).Scan(&owner); err != nil || owner != "team-bao" {
		t.Fatalf("owner override: %s %v", owner, err)
	}

	// tk-api alone leaves the adoptions as propose rows (kb promotes them);
	// promote here, as kb would, so applying again meets T claims.
	ownerTok := env.token(t, "bao", "bao")
	for _, c := range first.Claims {
		if code, e := moveAs(t, env, ownerTok, c.ClaimID, "promote", c.Basis); code != 200 {
			t.Fatalf("promote %s: %d %s", c.Subject, code, e)
		}
	}
	again := apply(t, env, agent, map[string]any{"template_id": "demo"})
	if len(again.NamespacesAdded) != 0 || !slices.Equal(again.Docs, first.Docs) || again.Claims[0].Outcome != "duplicate" ||
		again.Claims[1].Outcome != "duplicate" || again.Claims[2].Outcome != "duplicate" {
		t.Fatalf("apply again: %+v", again)
	}

	_, st = getJSON[setupStatus](t, env, env.URL+"/v1/setup/status")
	if st.Principles == nil || *st.Principles != first.Docs[1] || st.RulesCount != 1 || !slices.Contains(st.Namespaces, "demo-ns") ||
		st.ClaimsCount != 3 || st.TemplateApplied == nil || *st.TemplateApplied != "demo" {
		t.Fatalf("status after apply: %+v", st)
	}

	// template:<id>@<version> is a basis only for a template that exists at that version.
	r := admit(t, env, agent,
		admitItem{Subject: "tk:setup.done", Statement: "setup from demo", Label: "T", Basis: "template:demo@1"},
		admitItem{Subject: "tk:setup.v2", Statement: "x", Label: "T", Basis: "template:demo@2"},
		admitItem{Subject: "tk:setup.nope", Statement: "x", Label: "T", Basis: "template:nope@1"},
	)
	if r[0].Outcome != "new" || r[1].Error == nil || r[1].Error.Code != "BASIS_NOT_FOUND" || r[2].Error == nil || r[2].Error.Code != "BASIS_NOT_FOUND" {
		t.Fatalf("template basis: %+v", r)
	}

	if blank := apply(t, env, agent, map[string]any{"template_id": "blank"}); len(blank.Docs) != 0 || len(blank.Claims) != 0 {
		t.Fatalf("blank: %+v", blank)
	}
	if _, st = getJSON[setupStatus](t, env, env.URL+"/v1/setup/status"); st.TemplateApplied == nil || *st.TemplateApplied != "blank" {
		t.Fatalf("last applied: %+v", st.TemplateApplied)
	}
}
