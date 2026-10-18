package tka_test

import (
	"context"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// A lesson is stored as a document; the Case is a copy made from it when the
// person agrees (owner 17/10/2026, D-239). Kind and tags are open and are what
// cases() filters by; re-tagging adds a line, the Case's words never change.
func TestCase_CopyOfALessonFilteredByTags(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()

	// No document, no Case: the lesson is stored first.
	if _, err := r.AddCase(ctx, a, tka.CaseInput{Title: "x", Text: "y"}); code(t, err) != "NO_SOURCE" {
		t.Fatalf("a Case without from_doc: %v", err)
	}
	if _, err := r.AddCase(ctx, a, tka.CaseInput{Title: "x", Text: "y", FromDoc: "D-999"}); code(t, err) != "DOCUMENT_UNKNOWN" {
		t.Fatalf("a Case from a document not in the store: %v", err)
	}
	lesson := onDoc(t, r, a, "file:///notes/2026-10-12.md", "The agent read the old price list and quoted it to the client; refund 1,200 USD, 6 hours.")

	c, err := r.AddCase(ctx, a, tka.CaseInput{Title: "Agent quoted a stale price list",
		Text:    "Name the price list's date in the instruction; the agent reads the newest by default otherwise.",
		FromDoc: lesson.SourceDoc, Agent: "Claude", CostUSD: ptr(1200.0), CostHours: ptr(6.0), When: "2026-10-12",
		CaseTags: tka.CaseTags{Kind: " Agent Misread ", Keywords: []string{"Price list", "price list", "quote"},
			Topics: []string{"AI Enablement"}, CaseStudies: []string{"Acme rollout"}}})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "C-1" || c.FromDoc != lesson.SourceDoc || c.Kind == nil || *c.Kind != "agent misread" ||
		len(c.Keywords) != 2 || c.Keywords[0] != "price list" || c.SHA256 == "" {
		t.Fatalf("case: %+v", c)
	}

	// The kind is open: any word the person uses is taken.
	other := onDoc(t, r, a, "file:///notes/2026-10-14.md", "We spent two days building a dashboard nobody opened.")
	if _, err := r.AddCase(ctx, a, tka.CaseInput{Title: "Dashboard nobody opened", Text: "Ask who will open it before building it.",
		FromDoc: other.SourceDoc, CaseTags: tka.CaseTags{Kind: "insight", Topics: []string{"ai governance"}}}); err != nil {
		t.Fatal(err)
	}

	list, facets, err := r.Cases(ctx, a.Tenant(), tka.CaseFilter{Topic: "AI enablement"})
	if err != nil || len(list) != 1 || list[0].ID != "C-1" {
		t.Fatalf("filter by topic: %v %+v", err, list)
	}
	if facets.Kinds["agent misread"] != 1 || facets.Kinds["insight"] != 1 || facets.Topics["ai governance"] != 1 {
		t.Fatalf("facets: %+v", facets)
	}
	if list, _, _ := r.Cases(ctx, a.Tenant(), tka.CaseFilter{Q: "dashboard"}); len(list) != 1 || list[0].ID != "C-2" {
		t.Fatalf("filter by words: %+v", list)
	}

	// Re-tagging adds a line; the words and their hash stay.
	tagged, err := r.TagCase(ctx, a, "c-0001", tka.CaseTags{Kind: "price paid", CaseStudies: []string{"acme rollout", "pod s155"}})
	if err != nil || tagged.Kind == nil || *tagged.Kind != "price paid" || len(tagged.Keywords) != 0 || tagged.SHA256 != c.SHA256 {
		t.Fatalf("re-tag: %v %+v", err, tagged)
	}
	if list, _, _ := r.Cases(ctx, a.Tenant(), tka.CaseFilter{CaseStudy: "pod s155"}); len(list) != 1 {
		t.Fatalf("filter by case study after re-tag: %+v", list)
	}

	// A Case is read by id like any entry, and is not evidence for a fact.
	e, err := r.Read(ctx, a.Tenant(), "C-1")
	if err != nil || e.Kind != "case" || e.Case.Title != c.Title {
		t.Fatalf("entry C-1: %v %+v", err, e)
	}
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Stale price lists cost refunds.",
		BasisInput: tka.BasisInput{Basis: "fact", SourceDoc: "C-1", Where: "lesson"}}); err == nil {
		t.Fatal("a Case was taken as evidence for a fact")
	}

	// Retiring takes it out of cases(), keeps it readable, and says why.
	if _, err := r.RetireCase(ctx, a, "C-1", ""); code(t, err) != "EMPTY_REASON" {
		t.Fatalf("retire without reason: %v", err)
	}
	if _, err := r.RetireCase(ctx, a, "C-1", "kept by mistake"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RetireCase(ctx, a, "C-1", "again"); code(t, err) != "ALREADY_RETIRED" {
		t.Fatalf("retire twice: %v", err)
	}
	if list, facets, _ := r.Cases(ctx, a.Tenant(), tka.CaseFilter{}); len(list) != 1 || facets.Kinds["price paid"] != 0 {
		t.Fatalf("retired Case still listed: %+v %+v", list, facets)
	}
	if list, _, _ := r.Cases(ctx, a.Tenant(), tka.CaseFilter{Retired: true}); len(list) != 2 || list[1].Retired == nil {
		t.Fatalf("retired on request: %+v", list)
	}

	// The next boot runs every migration again over a store that holds C rows.
	if err := migrateAgain(ctx, r); err != nil {
		t.Fatalf("migrations again with a Case stored: %v", err)
	}
}
