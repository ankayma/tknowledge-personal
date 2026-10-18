package api

import (
	"context"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/subject"
)

type principleList struct {
	Count      int `json:"count"`
	Principles []struct {
		Code      string `json:"code"`
		Statement string `json:"statement"`
		Note      string `json:"note"`
		Label     string `json:"label"`
		ClaimID   int64  `json:"claim_id"`
	} `json:"principles"`
}

// SPEC §18: the Principle List is a trust class of its own (P.9). A principle's
// own row is the axiom (basis kind principle, no refs); a claim resting on one
// carries basis principle:<code> and is anchored to that row, so is_truth
// reaches the principle. An unknown code names nothing and is refused.
func TestPrinciples_ListAnchorAndRefusals(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	ownerTok := env.token(t, "bao", "bao")

	first := apply(t, env, agent, map[string]any{"template_id": "demo"})
	p1 := first.Claims[1]
	if p1.Subject != "principle:p1.statement" || !p1.NeedsPromote {
		t.Fatalf("template principle: %+v", p1)
	}

	_, list := getJSON[principleList](t, env, env.URL+"/v1/principles")
	if list.Count != 2 || list.Principles[0].Code != "P.1" || list.Principles[1].Code != "P.2" ||
		list.Principles[0].Statement != "Strong architecture absorbs weak execution." || list.Principles[0].Label != "A" {
		t.Fatalf("list while the rows wait for promote: %+v", list)
	}

	// a principle that has not been promoted carries nothing yet
	if r := admit(t, env, agent, admitItem{Subject: "tk:mvp.early", Statement: "x", Label: "T", Basis: "principle:P.1"}); r[0].Error == nil || r[0].Error.Code != "BASIS_NOT_FOUND" {
		t.Fatalf("basis on an unpromoted principle: %+v", r[0])
	}
	if code, msg := moveAs(t, env, ownerTok, p1.ClaimID, "promote", p1.Basis); code != 200 {
		t.Fatalf("promote principle: %d %s", code, msg)
	}

	r := admit(t, env, agent,
		admitItem{Subject: "tk:mvp.structure", Statement: "Cause channel đứng ngoài đường ghi", Label: "T", Basis: "principle:P.1"},
		admitItem{Subject: "tk:mvp.later", Statement: "x", Label: "T", Basis: "principle:P.7"},
		admitItem{Subject: "tk:mvp.bad", Statement: "x", Label: "T", Basis: "principle:nope"},
	)
	if r[0].Outcome != "new" || r[0].Basis == nil || r[0].Basis.Kind != "principle" ||
		len(r[0].Basis.Refs) != 1 || r[0].Basis.Refs[0] != p1.ClaimID {
		t.Fatalf("claim anchored to P.1: %+v", r[0])
	}
	if r[1].Error == nil || r[1].Error.Code != "BASIS_NOT_FOUND" || r[2].Error == nil || r[2].Error.Code != "INVALID_BASIS" {
		t.Fatalf("refusals: %+v %+v", r[1], r[2])
	}

	_, list = getJSON[principleList](t, env, env.URL+"/v1/principles")
	if list.Principles[0].Label != "T" || list.Principles[0].ClaimID != p1.ClaimID {
		t.Fatalf("promoted principle: %+v", list.Principles[0])
	}
}

// [T derivation]: a claim that follows from claims already admitted. Every
// premise must stand; is_truth then requires each to be eff=T (eff §4).
func TestPrinciples_DerivationBasis(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	ownerTok := env.token(t, "bao", "bao")

	premises := admit(t, env, agent,
		admitItem{Subject: "tk:mvp.ledger", Statement: "Ledger chỉ có INSERT", Label: "T", Basis: "https://example.org/spec#2"},
		admitItem{Subject: "tk:mvp.cdc", Statement: "tk-cdc đọc change feed của ledger", Label: "T", Basis: "https://example.org/spec#8"},
	)
	if premises[0].Outcome != "new" || premises[1].Outcome != "new" {
		t.Fatalf("premises: %+v", premises)
	}
	// a premise cited by the row the caller was given (here the T row of a
	// promote) is recorded as the claim it belongs to
	promoted := admit(t, env, agent, admitItem{Subject: "tk:mvp.relay", Statement: "tk-cdc là relay, không phải nguồn", Label: "A"})[0]
	code, msg := moveAs(t, env, ownerTok, promoted.ClaimID, "promote", &ledger.Basis{Kind: "fact", Note: "https://example.org/spec#8 @2026-09-18"})
	if code != 200 {
		t.Fatalf("promote premise: %d %s", code, msg)
	}
	var promotedRow int64
	if err := env.Admin.QueryRow(context.Background(), `SELECT id FROM ledger WHERE claim_id = $1 ORDER BY id DESC LIMIT 1`, promoted.ClaimID).Scan(&promotedRow); err != nil {
		t.Fatal(err)
	}
	if promotedRow == promoted.ClaimID {
		t.Fatal("the promote row should have an id of its own")
	}
	derived := admit(t, env, agent, admitItem{Subject: "tk:mvp.relay-consequence", Statement: "Mất tk-cdc không mất dòng nào", Label: "T",
		Basis: "derivation:" + subject.FormatID(promotedRow)})[0]
	if derived.Outcome != "new" || derived.Basis == nil || len(derived.Basis.Refs) != 1 || derived.Basis.Refs[0] != promoted.ClaimID {
		t.Fatalf("premise cited by row id: %+v", derived)
	}

	closed := admit(t, env, agent, admitItem{Subject: "tk:mvp.open-question", Statement: "Còn hỏi?", Label: "A-b"})[0]
	if code, msg := moveAs(t, env, ownerTok, closed.ClaimID, "drop", nil); code != 200 {
		t.Fatalf("drop: %d %s", code, msg)
	}

	r := admit(t, env, agent,
		admitItem{Subject: "tk:mvp.audit", Statement: "Mọi thay đổi ledger đều thấy được ngoài đường ghi", Label: "T",
			Basis: "derivation:" + premises[0].ID + "," + premises[1].ID},
		admitItem{Subject: "tk:mvp.gone", Statement: "x", Label: "T", Basis: "derivation:clm_999999"},
		admitItem{Subject: "tk:mvp.closed", Statement: "x", Label: "T", Basis: "derivation:" + closed.ID},
		admitItem{Subject: "tk:mvp.bad", Statement: "x", Label: "T", Basis: "derivation:nope"},
	)
	if r[0].Outcome != "new" || r[0].Basis == nil || r[0].Basis.Kind != "derivation" || len(r[0].Basis.Refs) != 2 ||
		r[0].Basis.Refs[0] != premises[0].ClaimID || r[0].Basis.Refs[1] != premises[1].ClaimID {
		t.Fatalf("derived claim: %+v", r[0])
	}
	for i, want := range map[int]string{1: "BASIS_NOT_FOUND", 2: "BASIS_NOT_FOUND", 3: "INVALID_BASIS"} {
		if r[i].Error == nil || r[i].Error.Code != want {
			t.Fatalf("claim %d: want %s, got %+v", i, want, r[i])
		}
	}
}
