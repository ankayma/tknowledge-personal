package tka_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// A fact points at evidence someone other than the writer can open (owner
// decision, 28/9). The person's own word is not evidence — that claim is an assumption,
// and the refusal says so.
func TestFact_NeedsEvidenceSomeoneElseCanOpen(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()

	claim := "Lan Nguyen (founder) là cổ đông của Halden Ltd"
	_, err := r.Admit(ctx, a, tka.AdmitInput{Claim: claim,
		BasisInput: tka.BasisInput{Basis: "fact", Source: "owner xác nhận trong chat"}})
	if code(t, err) != "SOURCE_NOT_RETRIEVABLE" {
		t.Fatalf("a citation on its own is not evidence: %v", err)
	}
	if !strings.Contains(next(t, err), "assume") {
		t.Errorf("the refusal must name assume() as one of the open moves: %q", next(t, err))
	}

	// A bare link is not taken any more (B1, 2/10): it keeps nothing, and the
	// refusal says how to read the page into a document instead.
	_, err = r.Admit(ctx, a, tka.AdmitInput{Claim: claim,
		BasisInput: tka.BasisInput{Basis: "fact", Where: "giấy phép", SourceURL: "https://example.com/licence"}})
	if code(t, err) != "USE_DOCUMENT" || !strings.Contains(next(t, err), "source_uri") {
		t.Fatalf("a link alone is not evidence the store keeps: %v", err)
	}

	// The evidence is the text read out of the licence, and where the PDF lives.
	licence := tka.DocumentInput{
		Text: "NGOC QUOC BAO TRINH 49%", TextProducer: "owner", Method: "opened_source",
		SourceURI: "drive://halden/License.pdf", SHA256: strings.Repeat("0a", 32), MediaType: "application/pdf",
		Filename: "License.pdf",
	}
	doc, err := r.AddDocument(ctx, a, licence)
	if err != nil || doc.ID != "D-1" || doc.Text == nil || !doc.Bound || doc.Legacy {
		t.Fatalf("record the licence: %+v %v", doc, err)
	}
	// The same original is the same evidence, not a second copy.
	licence.Filename = "License-copy.pdf"
	again, err := r.AddDocument(ctx, a, licence)
	if err != nil || again.ID != doc.ID {
		t.Fatalf("the same original twice is one document: %+v %v", again, err)
	}

	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: claim,
		BasisInput: tka.BasisInput{Basis: "fact", Where: "License.pdf trang 2", SourceDoc: "D-9"}}); code(t, err) != "DOCUMENT_UNKNOWN" {
		t.Fatalf("a fact cannot cite evidence the store does not hold: %v", err)
	}

	// `source` is the old name of `where` and is still taken (B2).
	got, err := r.Admit(ctx, a, tka.AdmitInput{Claim: claim,
		BasisInput: tka.BasisInput{Basis: "fact", Source: "License.pdf trang 2",
			SourceDoc: doc.ID, SourceDate: "2026-09-27"}})
	if err != nil || got.Entry == nil || got.Entry.SourceDoc == nil || *got.Entry.SourceDoc != doc.ID {
		t.Fatalf("admit on recorded evidence: %+v %v", got.Entry, err)
	}
	// The document is the claim's first evidence row, written with it.
	if len(got.Entry.Evidence) != 1 || got.Entry.Evidence[0].Where == nil ||
		*got.Entry.Evidence[0].Where != "License.pdf trang 2" || got.Entry.Strength != "opened_source" {
		t.Fatalf("first evidence row: %+v", got.Entry)
	}

	// Read back: the record and its text; there is no file to return.
	back, body, err := r.ReadDocument(ctx, a.Tenant(), doc.ID)
	if err != nil || body != nil || back.Text == nil || !strings.Contains(*back.Text, "BAO TRINH") ||
		len(back.EvidenceFor) != 1 || back.EvidenceFor[0] != got.Entry.ID {
		t.Fatalf("read the evidence back: %+v %v", back, err)
	}
}

// A stance is a T only when the person has the standing to decide it, and that
// standing is itself a claim in the store (owner 28/9): "if T-3 is a T then
// T-4 is a T, otherwise both are A". So a decision is a derivation, never a fact
// sourced to the decider's own name — and it needs the bridge premise that says
// standing carries the decision, which is a claim of its own.
func TestStance_StandsOnStandingNotOnWhoSaidIt(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()

	if _, err := r.AddPrinciple(ctx, a, "P.1",
		"A company's direction is set by the people who own it.", ""); err != nil {
		t.Fatal(err)
	}
	register := onDoc(t, r, a, "drive://halden/shareholders.pdf", "Halden Ltd shareholders: Anh Do 51, Thu Ho 49")
	register.Where, register.SourceDate = "shareholders.pdf page 1", "2026-09-27"
	standing, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Thu Ho holds 49 per cent of Halden Ltd", BasisInput: register})
	if err != nil {
		t.Fatal(err)
	}

	// The stance as a fact on the decider's own word: refused.
	if _, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim:      "Halden Ltd raises no venture capital before 2028",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "Thu Ho"}}); code(t, err) != "SOURCE_NOT_RETRIEVABLE" {
		t.Fatalf("a stance is not a fact about the world: %v", err)
	}

	// The stance on the standing alone: still refused, because the step from
	// holding shares to setting direction is a claim nobody has named.
	if _, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Halden Ltd raises no venture capital before 2028",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{standing.Entry.ID},
			Derivation: standing.Entry.ID + " ⟹ the decision is hers"}}); code(t, err) != "SINGLE_PREMISE" {
		t.Fatalf("one premise is a rereading of that premise: %v", err)
	}

	// With the bridge named, it stands.
	stance, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Halden Ltd raises no venture capital before 2028",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{standing.Entry.ID, "P.1"},
			Derivation: standing.Entry.ID + " ⟹ she is an owner; P.1 ⟹ owners set the direction " +
				"⟹ this is a decision by someone with the standing to take it"}})
	if err != nil || stance.Entry == nil || stance.Entry.Effective != "T" {
		t.Fatalf("a decision by someone with the standing: %+v %v", stance.Entry, err)
	}

	// Take the standing away and the stance stops standing on its own.
	if _, err := r.Demote(ctx, a, standing.Entry.ID, "shares transferred", "share register 2026-11"); err != nil {
		t.Fatal(err)
	}
	after, err := r.Read(ctx, a.Tenant(), stance.Entry.ID)
	if err != nil || after.Truth == nil {
		t.Fatal(err)
	}
	if after.Truth.Effective != "needs_review" || len(after.Truth.PremisesFell) != 1 {
		t.Fatalf("the stance must fall with the standing under it: %+v", after.Truth)
	}
}

// next is the move a refusal leaves open.
func next(t *testing.T, err error) string {
	t.Helper()
	var e *tka.Error
	if err == nil || !errors.As(err, &e) {
		return ""
	}
	return e.Next
}

// A fact may have more than one source (owner 28/9). The owner's own example:
// the minutes prove what was agreed but carry no date; the entry for the meeting
// carries the date and the fact that it happened. Two sources for one fact — not
// a derivation, because the new content comes from the document, not from
// reasoning. A document is never a premise: premises are claims.
func TestFact_MaySourceAnEntryForWhatTheDocumentCannotCarry(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()

	invite, err := r.AddDocument(ctx, a, tka.DocumentInput{Filename: "invite.txt", TextProducer: "owner",
		Method: "opened_source", SourceURI: "file:///calendar/invite.ics",
		Text: "Teams invite\nDate: 2026-09-25 21:00 Dubai\nAttendees: Minh, Khoa, Bao\n"})
	if err != nil {
		t.Fatal(err)
	}
	minutes, err := r.AddDocument(ctx, a, tka.DocumentInput{Filename: "minutes.txt", TextProducer: "owner",
		Method: "opened_source", SourceURI: "file:///notes/minutes.txt",
		Text: "Agreed: monthly settlement extract shared read-only.\n"})
	if err != nil {
		t.Fatal(err)
	}

	meeting, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Lan Nguyen họp DD với Minh và Khoa ngày 25/9/2026",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "Teams invite, dòng Date và Attendees",
			SourceDoc: invite.ID, SourceDate: "2026-09-25"}})
	if err != nil || meeting.Entry == nil {
		t.Fatalf("the meeting: %+v %v", meeting.Entry, err)
	}

	// A document cannot be a premise, and saying so points at the right field.
	if _, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Lan Nguyen và Minh thống nhất cách chia sẻ data ngày 25/9/2026",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "biên bản họp",
			SourceDoc: minutes.ID, Premises: []string{meeting.Entry.ID}}}); code(t, err) != "INVALID_BASIS" {
		t.Fatalf("an entry cited by a fact is evidence, not a premise: %v", err)
	}
	if !strings.Contains(next(t, errOf(r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Lan Nguyen và Minh thống nhất cách chia sẻ data ngày 25/9/2026",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "biên bản họp",
			SourceDoc: minutes.ID, Premises: []string{meeting.Entry.ID}}}))), "source_entry") {
		t.Error("the refusal must name the field that is right")
	}

	// An entry alone is not evidence: something openable is always required.
	if _, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Lan Nguyen và Minh thống nhất điều khoản phí ngày 25/9/2026",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "cuộc họp",
			SourceEntry: meeting.Entry.ID}}); code(t, err) != "SOURCE_NOT_RETRIEVABLE" {
		t.Fatalf("a chain of claims citing each other never touches the world: %v", err)
	}

	// Document plus entry: the shape the owner described.
	agreed, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Lan Nguyen và Minh thống nhất cách chia sẻ data ngày 25/9/2026",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "biên bản họp, dòng Agreed",
			SourceDoc: minutes.ID, SourceEntry: meeting.Entry.ID}})
	if err != nil || agreed.Entry == nil || agreed.Entry.SourceEntry == nil ||
		*agreed.Entry.SourceEntry != meeting.Entry.ID || agreed.Entry.Effective != "T" {
		t.Fatalf("a fact with two sources: %+v %v", agreed.Entry, err)
	}
	if len(agreed.Entry.Premises) != 0 {
		t.Errorf("it is still a fact, with no premises: %+v", agreed.Entry.Premises)
	}

	// The meeting falls, and the agreement loses the thing that dated it.
	if _, err := r.Demote(ctx, a, meeting.Entry.ID, "invite hoá ra của cuộc họp khác", "calendar 2026-09"); err != nil {
		t.Fatal(err)
	}
	after, err := r.Read(ctx, a.Tenant(), agreed.Entry.ID)
	if err != nil || after.Truth == nil {
		t.Fatal(err)
	}
	if after.Truth.Effective != "needs_review" || len(after.Truth.PremisesFell) != 1 ||
		after.Truth.PremisesFell[0] != meeting.Entry.ID {
		t.Fatalf("a fallen source must flag the claim that leaned on it: %+v", after.Truth)
	}

	// And it cannot be cited as a source once it has fallen.
	if _, err := r.Admit(ctx, a, tka.AdmitInput{
		Claim: "Lan Nguyen và Khoa thống nhất lịch báo cáo ngày 25/9/2026",
		BasisInput: tka.BasisInput{Basis: "fact", Source: "biên bản họp",
			SourceDoc: minutes.ID, SourceEntry: meeting.Entry.ID}}); code(t, err) != "SOURCE_ENTRY_NOT_T" {
		t.Fatalf("a fallen entry is not a source: %v", err)
	}
}

func errOf(_ tka.AdmitResult, err error) error { return err }
