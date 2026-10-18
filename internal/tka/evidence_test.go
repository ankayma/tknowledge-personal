package tka_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// T–D, phase 1 (owner approved 2/10/2026): a document is text plus a pointer
// to the original; a fact rests on documents through evidence rows; the store
// raises flags and a person answers them.

func text(producer, method, body string) tka.DocumentInput {
	return tka.DocumentInput{Text: body, TextProducer: producer, Method: method}
}

// What a document must carry, and what it may not.
func TestDocument_GatesAndBound(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	refused := func(in tka.DocumentInput, want string) {
		t.Helper()
		if _, err := r.AddDocument(ctx, a, in); code(t, err) != want {
			t.Fatalf("want %s, got %v", want, err)
		}
	}
	ok := text("owner", "read_text", "Applied to Senior SA at Northwind on 2026-08-24")

	withContent := ok
	withContent.Content = "UEsDBBQ="
	refused(withContent, "FILE_NOT_KEPT")
	refused(text("owner", "read_text", "   "), "EMPTY_TEXT")
	refused(text("someone", "read_text", "x"), "INVALID_TEXT_PRODUCER")
	refused(text("owner", "looked", "x"), "INVALID_METHOD")
	for _, uri := range []string{"ftp://host/file", "not a uri", "https://", "s3://"} {
		in := ok
		in.SourceURI = uri
		refused(in, "INVALID_SOURCE_URI")
	}
	badSHA := ok
	badSHA.SHA256 = "abc"
	refused(badSHA, "INVALID_SHA256")
	badContent := ok
	badContent.ContentSHA256 = strings.Repeat("z", 64)
	refused(badContent, "INVALID_SHA256")
	zero := 0
	badSize := ok
	badSize.Size = &zero
	refused(badSize, "INVALID_SIZE")
	badDate := ok
	badDate.CapturedAt = "yesterday"
	refused(badDate, "INVALID_DATE")

	// Text only: recorded, and it says on every read that nothing binds it.
	loose, err := r.AddDocument(ctx, a, ok)
	if err != nil || loose.Bound || loose.Text == nil || *loose.Method != "read_text" || loose.Signer != "bao" {
		t.Fatalf("text-only document: %+v %v", loose, err)
	}
	if back, err := r.Read(ctx, a.Tenant(), loose.ID); err != nil || back.Kind != "document" || back.Document.Bound {
		t.Fatalf("bound is read back, not a one-off warning: %+v %v", back, err)
	}
	// A pointer to the original binds it; so does the original's hash.
	for _, in := range []tka.DocumentInput{
		{Text: "page", TextProducer: "claude", Method: "opened_source", SourceURI: "https://www.linkedin.com/jobs/applied",
			CapturedAt: "2026-10-01T09:30:00+04:00", Locator: "LinkedIn > My jobs > Applied"},
		{Text: "shot", TextProducer: "owner", Method: "rehashed", SHA256: strings.Repeat("AB", 32)},
		{Text: "file", TextProducer: "extract", Method: "opened_source", SourceURI: "file:///Users/bao/x.png"},
		{Text: "drive", TextProducer: "owner", Method: "opened_source", SourceURI: "drive://1AbC"},
	} {
		d, err := r.AddDocument(ctx, a, in)
		if err != nil || !d.Bound {
			t.Fatalf("bound document %+v: %+v %v", in, d, err)
		}
	}
	docs, err := r.Documents(ctx, a.Tenant())
	if err != nil || len(docs) != 5 || docs[0].Text == nil {
		t.Fatalf("documents carry their text: %+v %v", docs, err)
	}
}

// A claim rests first on its signer's own document; more documents attach with
// attest; the claim reads its strength from the best way anyone looked.
func TestEvidence_AttestStrengthAndOwnership(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	lan := tka.Actor{UserID: "lan", ActorID: "claude", GrantID: "g-lan"}

	note := text("owner", "read_text", "Screenshot: Northwind application submitted 2026-08-24")
	note.SourceURI = "file:///iphone/IMG_2041.png"
	mine, err := r.AddDocument(ctx, a, note)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := r.AddDocument(ctx, lan, tka.DocumentInput{Text: "Lan's own reading of the portal",
		TextProducer: "owner", Method: "opened_source", SourceURI: "https://careers.example.com/status/1"})
	if err != nil {
		t.Fatal(err)
	}
	claim := "Lan Nguyen applied for Senior SA at Northwind on 2026-08-24"

	// Someone else's document cannot be the first evidence of my claim.
	if _, err := r.Admit(ctx, a, tka.AdmitInput{Claim: claim,
		BasisInput: tka.BasisInput{Basis: "fact", Where: "status line", SourceDoc: theirs.ID}}); code(t, err) != "DOCUMENT_NOT_YOURS" {
		t.Fatalf("admit on another signer's document: %v", err)
	}
	got, err := r.Admit(ctx, a, tka.AdmitInput{Claim: claim,
		BasisInput: tka.BasisInput{Basis: "fact", Where: "status line", SourceDoc: mine.ID}})
	if err != nil || got.Entry == nil {
		t.Fatal(err)
	}
	if got.Entry.Strength != "read_text" || got.Entry.Attesters == nil || *got.Entry.Attesters != 1 {
		t.Fatalf("one read_text document: %+v", got.Entry)
	}
	tID := got.Entry.ID

	if _, err := r.Attest(ctx, a, tka.AttestInput{Truth: tID, Document: mine.ID}); code(t, err) != "EMPTY_WHERE" {
		t.Fatalf("attest says where: %v", err)
	}
	if _, err := r.Attest(ctx, a, tka.AttestInput{Truth: tID, Document: theirs.ID, Where: "x"}); code(t, err) != "DOCUMENT_NOT_YOURS" {
		t.Fatalf("attest with someone else's document: %v", err)
	}
	// Attaching the same pair again is the same row.
	rep, err := r.Attest(ctx, a, tka.AttestInput{Truth: tID, Document: mine.ID, Where: "status line"})
	if err != nil || !rep.Repeat {
		t.Fatalf("repeat attest: %+v %v", rep, err)
	}

	// A stronger look raises the claim's strength.
	portal, err := r.AddDocument(ctx, a, tka.DocumentInput{Text: "Application status: submitted, 2026-08-24",
		TextProducer: "owner", Method: "opened_source", SourceURI: "https://careers.example.com/status/1",
		ContentSHA256: strings.Repeat("1", 64)})
	if err != nil {
		t.Fatal(err)
	}
	up, err := r.Attest(ctx, a, tka.AttestInput{Truth: "T-0" + strings.TrimPrefix(tID, "T-"), Document: portal.ID, Where: "status row"})
	if err != nil || up.Repeat || up.Strength != "opened_source" || up.Attesters != 1 {
		t.Fatalf("second document of the same signer: %+v %v", up, err)
	}
	// A different person standing behind it with their own document is a second attester.
	two, err := r.Attest(ctx, lan, tka.AttestInput{Truth: tID, Document: theirs.ID, Where: "status row"})
	if err != nil || two.Attesters != 2 {
		t.Fatalf("independent attester: %+v %v", two, err)
	}
	entry, err := r.Read(ctx, a.Tenant(), tID)
	if err != nil || len(entry.Truth.Evidence) != 3 || *entry.Truth.Attesters != 2 || entry.Truth.Signer != "bao" {
		t.Fatalf("entry lists every evidence row, the signer does not change: %+v %v", entry.Truth, err)
	}
	if d, _ := r.Read(ctx, a.Tenant(), portal.ID); len(d.Document.EvidenceFor) != 1 || d.Document.EvidenceFor[0] != tID {
		t.Fatalf("evidence_for: %+v", d.Document)
	}

	// A derivation has no documents of its own.
	if _, err := r.AddPrinciple(ctx, a, "P.1", "A record beats a memory.", ""); err != nil {
		t.Fatal(err)
	}
	der, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Northwind has Lan Nguyen's application on record",
		BasisInput: tka.BasisInput{Basis: "derivation", Premises: []string{tID, "P.1"}, Derivation: tID + " + P.1 ⟹ on record"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Attest(ctx, a, tka.AttestInput{Truth: der.Entry.ID, Document: mine.ID, Where: "x"}); code(t, err) != "NOT_A_FACT" {
		t.Fatalf("attest on a derivation: %v", err)
	}
	if der.Entry.Strength != "" || der.Entry.Attesters != nil {
		t.Fatalf("a derivation reads no strength: %+v", der.Entry)
	}

	// Evidence against a fallen claim is recorded, with a warning.
	if _, err := r.Demote(ctx, a, tID, "wrong date", ""); err != nil {
		t.Fatal(err)
	}
	late, err := r.AddDocument(ctx, a, text("owner", "read_text", "email confirming the application"))
	if err != nil {
		t.Fatal(err)
	}
	warned, err := r.Attest(ctx, a, tka.AttestInput{Truth: tID, Document: late.ID, Where: "body"})
	if err != nil || warned.Warning == "" {
		t.Fatalf("attest on a demoted claim warns: %+v %v", warned, err)
	}
}

// Taking evidence back never lowers a claim; taking back the last of it opens a
// flag for the owner, who answers it with a named move.
func TestEvidence_WithdrawAndFlags(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	lan := tka.Actor{UserID: "lan", ActorID: "claude", GrantID: "g-lan"}

	b := onDoc(t, r, a, "https://example.com/offer", "Offer letter, start date 2026-10-02")
	got, err := r.Admit(ctx, a, tka.AdmitInput{Claim: "Northwind offered Lan Nguyen a start date of 2026-10-02", BasisInput: b})
	if err != nil {
		t.Fatal(err)
	}
	tID, dID := got.Entry.ID, b.SourceDoc
	second := onDoc(t, r, a, "file:///mail/offer.eml", "the offer email")

	if _, err := r.WithdrawEvidence(ctx, a, tka.WithdrawInput{Truth: tID, Document: dID}); code(t, err) != "EMPTY_REASON" {
		t.Fatalf("withdraw needs a reason: %v", err)
	}
	if _, err := r.WithdrawEvidence(ctx, a, tka.WithdrawInput{Truth: tID, Document: second.SourceDoc, Reason: "x"}); code(t, err) != "NOT_ATTACHED" {
		t.Fatalf("withdraw what is not attached: %v", err)
	}
	if _, err := r.WithdrawEvidence(ctx, lan, tka.WithdrawInput{Truth: tID, Document: dID, Reason: "x"}); code(t, err) != "EVIDENCE_NOT_YOURS" {
		t.Fatalf("only the one who attached it takes it back: %v", err)
	}

	// Two documents: taking one back leaves the other standing, no flag.
	if _, err := r.Attest(ctx, a, tka.AttestInput{Truth: tID, Document: second.SourceDoc, Where: "body"}); err != nil {
		t.Fatal(err)
	}
	w1, err := r.WithdrawEvidence(ctx, a, tka.WithdrawInput{Truth: tID, Document: dID, Reason: "the page now shows another date"})
	if err != nil || w1.Flag != nil || w1.Attesters != 1 || !w1.Evidence.Withdrawn {
		t.Fatalf("withdraw one of two: %+v %v", w1, err)
	}
	if _, err := r.WithdrawEvidence(ctx, a, tka.WithdrawInput{Truth: tID, Document: dID, Reason: "again"}); code(t, err) != "ALREADY_WITHDRAWN" {
		t.Fatalf("withdraw twice: %v", err)
	}
	// The last one: the claim does not move, a flag opens.
	w2, err := r.WithdrawEvidence(ctx, a, tka.WithdrawInput{Truth: tID, Document: second.SourceDoc, Reason: "wrong thread"})
	if err != nil || w2.Flag == nil || w2.Flag.Kind != "evidence_withdrawn" || w2.Flag.Subject != tID ||
		w2.Strength != tka.StrengthNone || w2.Attesters != 0 {
		t.Fatalf("withdraw the last: %+v %v", w2, err)
	}
	entry, err := r.Read(ctx, a.Tenant(), tID)
	if err != nil || entry.Truth.Status != "T" || len(entry.Truth.Flags) != 1 || len(entry.Truth.Evidence) != 2 ||
		entry.Truth.Evidence[0].WithdrawnReason == nil {
		t.Fatalf("the claim stands, flagged, rows readable: %+v %v", entry.Truth, err)
	}

	// The flag is answered by a person.
	if _, err := r.CloseFlag(ctx, a, tka.CloseFlagInput{Flag: w2.Flag.ID}); code(t, err) != "EMPTY_REASON" {
		t.Fatalf("closing says what was found: %v", err)
	}
	if _, err := r.CloseFlag(ctx, a, tka.CloseFlagInput{Flag: "F-99", Note: "x"}); code(t, err) != "FLAG_UNKNOWN" {
		t.Fatalf("unknown flag: %v", err)
	}
	if _, err := r.Flags(ctx, a.Tenant(), "pending"); code(t, err) != "INVALID_STATUS" {
		t.Fatalf("status filter: %v", err)
	}
	closed, err := r.CloseFlag(ctx, a, tka.CloseFlagInput{Flag: "f-0" + strings.TrimPrefix(w2.Flag.ID, "F-"), Note: "will re-admit with the signed letter"})
	if err != nil || closed.Status != "closed" || closed.ClosedBy == nil || *closed.ClosedBy != "bao" {
		t.Fatalf("close: %+v %v", closed, err)
	}
	if _, err := r.CloseFlag(ctx, a, tka.CloseFlagInput{Flag: closed.ID, Note: "x"}); code(t, err) != "ALREADY_CLOSED" {
		t.Fatalf("close twice: %v", err)
	}
	open, _ := r.Flags(ctx, a.Tenant(), "")
	all, _ := r.Flags(ctx, a.Tenant(), "all")
	if len(open) != 0 || len(all) != 1 {
		t.Fatalf("open %d, all %d", len(open), len(all))
	}
}

// A live page is a live source: read twice and found changed, or read the same
// and written down differently — two findings, flagged apart.
func TestDocument_SamePageFlags(t *testing.T) {
	r, a := newRepo(t)
	ctx := context.Background()
	page := func(hash, body string) tka.Document {
		t.Helper()
		d, err := r.AddDocument(ctx, a, tka.DocumentInput{Text: body, TextProducer: "claude", Method: "opened_source",
			SourceURI: "https://example.com/pricing", ContentSHA256: strings.Repeat(hash, 64)})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	first := page("1", "Free tier: 3 users")
	if len(first.Flags) != 0 {
		t.Fatalf("first reading flags nothing: %+v", first.Flags)
	}
	changed := page("2", "Free tier: 5 users")
	if len(changed.Flags) != 1 || changed.Flags[0].Kind != "page_changed" || changed.Flags[0].Subject != changed.ID {
		t.Fatalf("page changed: %+v", changed.Flags)
	}
	// The third reading matches the first page and not the second: the page
	// changed again, and the first reading was written down differently. Two
	// findings, two flags.
	differently := page("1", "Free tier: three users, one admin")
	if len(differently.Flags) != 2 || differently.Flags[0].Kind != "page_changed" ||
		differently.Flags[1].Kind != "read_differently" || !strings.Contains(differently.Flags[1].Detail, first.ID) {
		t.Fatalf("read differently: %+v", differently.Flags)
	}
	open, err := r.Flags(ctx, a.Tenant(), "open")
	if err != nil || len(open) != 3 {
		t.Fatalf("three open flags: %+v %v", open, err)
	}
	if d, _ := r.Read(ctx, a.Tenant(), changed.ID); len(d.Document.Flags) != 1 {
		t.Fatalf("a document reads its open flags: %+v", d.Document)
	}
}

// Documents, evidence and flags belong to one store.
func TestEvidence_TenantIsolationAndLookup(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	acme := tka.Actor{UserID: "bao", ActorID: "claude", GrantID: "g1", TenantID: "acme"}
	globex := tka.Actor{UserID: "bao", ActorID: "claude", GrantID: "g2", TenantID: "globex"}

	b := onDoc(t, r, acme, "https://example.com/renewal", "Acme renewal countersigned on 2026-10-02")
	if _, err := r.Admit(ctx, acme, tka.AdmitInput{Claim: "Acme countersigned the renewal on 2026-10-02", BasisInput: b}); err != nil {
		t.Fatal(err)
	}
	// globex cannot rest a claim on acme's document: it is not in its store.
	if _, err := r.Admit(ctx, globex, tka.AdmitInput{Claim: "Acme countersigned the renewal on 2026-10-02",
		BasisInput: tka.BasisInput{Basis: "fact", Where: "x", SourceDoc: b.SourceDoc}}); code(t, err) != "DOCUMENT_UNKNOWN" {
		t.Fatalf("another store's document: %v", err)
	}
	if docs, _ := r.Documents(ctx, "globex"); len(docs) != 0 {
		t.Fatalf("globex sees acme's documents: %+v", docs)
	}
	found, err := r.LookupDocuments(ctx, "acme", "COUNTERSIGNED")
	if err != nil || len(found) != 1 || found[0].Text == nil || len(found[0].EvidenceFor) != 1 {
		t.Fatalf("lookup on text: %+v %v", found, err)
	}
	if none, _ := r.LookupDocuments(ctx, "globex", "countersigned"); len(none) != 0 {
		t.Fatalf("lookup crosses stores: %+v", none)
	}
}

// A running total is not a claim; a date after "as of" makes it one; "and"
// is the signer's question, never a refusal.
func TestCheckClaim_RunningTotalsAndConjunctions(t *testing.T) {
	for _, s := range []string{
		"Lan Nguyen has passed three interview rounds at Northwind so far",
		"Tính đến nay Lan Nguyen đã qua ba vòng phỏng vấn ở Northwind",
		"Northwind has interviewed Lan Nguyen three times as of now",
	} {
		if c := tka.CheckClaim(s, "fact"); c.Admissible {
			t.Errorf("a running total stands as a claim: %q", s)
		}
	}
	for _, s := range []string{
		"As of 2026-10-01 Northwind had interviewed Lan Nguyen three times",
		"Tính đến ngày 2026-10-01 Northwind đã phỏng vấn Lan Nguyen ba lần",
	} {
		if c := tka.CheckClaim(s, "fact"); !c.Admissible {
			t.Errorf("an absolute date is fine: %q %+v", s, c.Problems)
		}
	}
	c := tka.CheckClaim("Northwind sent the offer on 2026-10-01 và Lan Nguyen signed it on 2026-10-02", "fact")
	joined := false
	for _, s := range c.SignerChecks {
		joined = joined || strings.Contains(s, `"và"`)
	}
	if !c.Admissible || !joined {
		t.Fatalf("\"và\" goes to the signer, it does not refuse: %+v", c)
	}
}
