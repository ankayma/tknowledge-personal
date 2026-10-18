package tka_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

func catalogIDs(es []tka.CatalogEntry) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

var draftTags = tka.InstructionTags{Who: []string{"agent"}, What: []string{"message"}, When: []string{"before-draft"},
	Where: []string{"chat"}, Why: "a draft cut for length loses what the reader needs to act", How: "cut words, not information"}

// The catalog (owner 14/10/2026, spec-instruction-catalog-14-10-26.md): its
// first entries are the default list, all starters and approved; a store
// begins with them, each line naming its entry; an entry added later reaches
// a store only as an offer, which the person takes in their own language or
// declines; nobody sees an entry the maintainer has not approved.
func TestCatalog_StartersOffersAdoptDecline(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	admin := tka.Actor{UserID: "owner", ActorID: "tk-cli", GrantID: "gr_admin"}

	cur, err := r.Catalog(ctx, tka.CatalogFilter{Maintainer: true})
	if err != nil || len(cur) != 27 || cur[0].ID != "BP-1" || !cur[26].Starter || cur[14].Tags.When[0] != "on-cost" {
		t.Fatalf("the catalog starts as the default list: %d %v", len(cur), err)
	}
	if again, _ := r.Catalog(ctx, tka.CatalogFilter{Maintainer: true}); len(again) != 27 {
		t.Fatalf("seeding the catalog twice: %d", len(again))
	}
	defaults := tka.DefaultInstructions()
	for i, e := range cur {
		if e.Text != defaults[i].Text {
			t.Fatalf("%s is default line %d word for word", e.ID, i+1)
		}
	}

	// A new store begins with the starters, each naming its entry.
	list, _ := r.Instructions(ctx, "", false)
	if len(list) != 27 || list[14].Label != "13" || list[14].CatalogRef == nil || *list[14].CatalogRef != "BP-15" {
		t.Fatalf("a new store's lines: %d %+v", len(list), list[14])
	}

	// A proposal: the maintainer sees it, nobody else does.
	bp, err := r.CatalogAdd(ctx, admin, tka.CatalogAddInput{Text: "When a draft is too long, cut the words around the information, never the information the reader needs to act on.",
		Tags: draftTags, Evidence: "A person corrected the same cut six times in one session.", ReviewNote: "rewritten from a private directive"})
	if err != nil || bp.ID != "BP-28" || bp.Status != tka.CatalogProposed {
		t.Fatalf("propose: %+v %v", bp, err)
	}
	if pub, _ := r.Catalog(ctx, tka.CatalogFilter{Statuses: []string{tka.CatalogCurrent}}); len(pub) != 12 || slices.Contains(catalogIDs(pub), "BP-28") {
		t.Fatalf("a proposal is not public: %v", catalogIDs(pub))
	}
	if _, offers, _ := r.InstructionsWithOffers(ctx, "", false); len(offers.New) != 0 {
		t.Fatalf("a proposal is not offered: %+v", offers.New)
	}
	if _, err := r.AddInstruction(ctx, a, tka.InstructionAddInput{Catalog: "BP-28"}); code(t, err) != "CATALOG_UNKNOWN" {
		t.Fatalf("a proposal cannot be taken: %v", err)
	}
	if mine, _ := r.Catalog(ctx, tka.CatalogFilter{Statuses: []string{tka.CatalogProposed}, Maintainer: true}); len(mine) != 1 || mine[0].ReviewNote == nil {
		t.Fatalf("the maintainer reads proposals with notes: %+v", mine)
	}
	if _, err := r.CatalogAdd(ctx, admin, tka.CatalogAddInput{Text: bp.Text, Tags: draftTags}); code(t, err) != "CATALOG_DUPLICATE" {
		t.Fatalf("the same words twice: %v", err)
	}

	// Approved through the gate (A-157): the store it came from and the
	// incident's date; then public, offered, and suggested when it fits.
	for _, c := range []struct {
		in   tka.CatalogReviewInput
		want string
	}{
		{tka.CatalogReviewInput{ID: "BP-28", Decision: "approve"}, "CATALOG_GATE"},
		{tka.CatalogReviewInput{ID: "BP-28", Decision: "approve", Source: "a founder's store, UAE"}, "CATALOG_GATE"},
		{tka.CatalogReviewInput{ID: "BP-28", Decision: "approve", Source: "a founder's store, UAE", IncidentOn: "13/10/2026"}, "INVALID_DATE"},
		{tka.CatalogReviewInput{ID: "BP-28", Decision: "approve", Source: "a founder's store, UAE", IncidentOn: "2999-01-01"}, "INVALID_DATE"},
		{tka.CatalogReviewInput{ID: "BP-28", Decision: "reject", Note: "no", Source: "x"}, "INVALID_DECISION"},
	} {
		if _, err := r.CatalogReview(ctx, admin, c.in); code(t, err) != c.want {
			t.Fatalf("%+v: want %s, got %v", c.in, c.want, err)
		}
	}
	got28, err := r.CatalogReview(ctx, admin, tka.CatalogReviewInput{ID: "bp-28", Decision: "approve", Source: "a founder's store, UAE", IncidentOn: "2026-10-13"})
	if err != nil || got28.Status != tka.CatalogCurrent || got28.ApprovedAt == nil || !got28.Public || *got28.Source != "a founder's store, UAE" || *got28.IncidentOn != "2026-10-13" {
		t.Fatalf("approve: %+v %v", got28, err)
	}
	pub, _ := r.Catalog(ctx, tka.CatalogFilter{})
	if len(pub) != 13 || pub[12].ID != "BP-28" || pub[12].HeldBack != "" {
		t.Fatalf("public: the definitions and BP-28: %v", catalogIDs(pub))
	}
	for _, e := range pub[:12] {
		if e.Kind != "definition" {
			t.Fatalf("a starter directive without an incident is not public: %s", e.ID)
		}
	}
	if mine, _ := r.Catalog(ctx, tka.CatalogFilter{Maintainer: true}); mine[2].Public || mine[2].HeldBack != "no incident, no incident date, no source" {
		t.Fatalf("the maintainer sees why a starter is held back: %+v", mine[2])
	}
	_, offers, _ := r.InstructionsWithOffers(ctx, "", false)
	if !slices.Equal(catalogIDs(offers.New), []string{"BP-28"}) || offers.Next == "" || offers.New[0].ReviewNote != nil {
		t.Fatalf("offered once approved, without the review note: %+v", offers)
	}
	h, _ := r.HelpFor(ctx, "", "draft a linkedin message")
	if len(h.Suggested) != 1 || h.Suggested[0].ID != "BP-28" || h.Suggested[0].Evidence == nil || h.SuggestedNext == "" {
		t.Fatalf("help suggests it: %+v", h.Suggested)
	}
	if h, _ := r.HelpFor(ctx, "", "close session"); len(h.Suggested) != 0 {
		t.Fatalf("not for an unrelated situation: %+v", h.Suggested)
	}

	// Taken in the person's language: the English word for word, the
	// translation beside it.
	for _, c := range []struct {
		in   tka.InstructionAddInput
		want string
	}{
		{tka.InstructionAddInput{Catalog: "BP-28", Text: bp.Text + " Always."}, "CATALOG_TEXT_CHANGED"},
		{tka.InstructionAddInput{Catalog: "BP-28", Translation: "Khi bản nháp quá dài…"}, "TRANSLATION_WITHOUT_LANG"},
		{tka.InstructionAddInput{Text: "Quote prices with currency.", Translation: "Ghi giá kèm đơn vị.", Lang: "vi"}, "TRANSLATION_WITHOUT_CATALOG"},
		{tka.InstructionAddInput{Catalog: "BP-28", Kind: "meta"}, "INVALID_INSTRUCTION"},
		{tka.InstructionAddInput{Catalog: "BP-999"}, "CATALOG_UNKNOWN"},
	} {
		if _, err := r.AddInstruction(ctx, a, c.in); code(t, err) != c.want {
			t.Fatalf("%+v: want %s, got %v", c.in, c.want, err)
		}
	}
	vi := "Khi bản nháp quá dài, cắt lời quanh thông tin, không bao giờ cắt thông tin người đọc cần để hành động."
	res, err := r.AddInstruction(ctx, a, tka.InstructionAddInput{Catalog: "BP-28", Translation: vi, Lang: "vi"})
	if err != nil || res.Instruction == nil {
		t.Fatalf("adopt: %+v %v", res, err)
	}
	got := res.Instruction
	if got.Text != bp.Text || got.Origin != "catalog" || *got.CatalogRef != "BP-28" || *got.Translation != vi || *got.Lang != "vi" ||
		*got.TranslatedBy != "claude-desktop" || got.Label != "14" || got.Tags.Source != "catalog" {
		t.Fatalf("adopted line: %+v", got)
	}
	if again, err := r.AddInstruction(ctx, a, tka.InstructionAddInput{Catalog: "BP-28", Translation: vi, Lang: "vi"}); err != nil || !again.Existing {
		t.Fatalf("taking it twice returns the line: %+v %v", again, err)
	}
	// A better translation supersedes; the English does not change.
	vi2 := "Bản nháp dài thì cắt lời, không cắt thông tin người đọc cần để làm."
	if fix, err := r.AddInstruction(ctx, a, tka.InstructionAddInput{Catalog: "BP-28", Translation: vi2, Lang: "vi", Supersedes: got.ID}); err != nil ||
		fix.Instruction == nil || *fix.Instruction.Translation != vi2 || fix.Instruction.Text != bp.Text {
		t.Fatalf("new translation: %+v %v", fix, err)
	}
	if _, offers, _ := r.InstructionsWithOffers(ctx, "", false); len(offers.New) != 0 {
		t.Fatalf("taken, no longer offered: %+v", offers.New)
	}
	if h, _ := r.HelpFor(ctx, "", "draft a linkedin message"); len(h.Suggested) != 0 || !slices.ContainsFunc(h.Directives, func(l tka.HelpLine) bool {
		return l.CatalogRef != nil && *l.CatalogRef == "BP-28" && l.Translation != nil && *l.Translation == vi2
	}) {
		t.Fatalf("help returns the store's own line with its translation: %+v", h)
	}

	// A newer wording: the store that holds BP-28 is offered the update.
	bp2, err := r.CatalogAdd(ctx, admin, tka.CatalogAddInput{Text: "When a draft is too long, cut words, never what the reader needs to act on: a URL, the steps, a time, an address.",
		Tags: draftTags, Supersedes: "BP-28"})
	if err != nil {
		t.Fatal(err)
	}
	// A new wording carries its ancestor's incident and source.
	if got, err := r.CatalogReview(ctx, admin, tka.CatalogReviewInput{ID: bp2.ID, Decision: "approve", Note: "clearer"}); err != nil ||
		!got.Public || got.Evidence == nil || *got.IncidentOn != "2026-10-13" {
		t.Fatalf("approve a new wording: %+v %v", got, err)
	}
	all, _ := r.Catalog(ctx, tka.CatalogFilter{Statuses: []string{tka.CatalogCurrent, tka.CatalogSuperseded}, Maintainer: true})
	if old := all[27]; old.ID != "BP-28" || old.Status != tka.CatalogSuperseded || *old.SupersededBy != bp2.ID {
		t.Fatalf("approving the new wording retires the old: %+v", old)
	}
	_, offers, _ = r.InstructionsWithOffers(ctx, "", false)
	if len(offers.New) != 0 || len(offers.Updates) != 1 || offers.Updates[0].Was != "BP-28" || offers.Updates[0].Entry.ID != bp2.ID {
		t.Fatalf("an update, not a new entry: %+v", offers)
	}
	if _, err := r.AddInstruction(ctx, a, tka.InstructionAddInput{Catalog: "BP-28"}); code(t, err) != "CATALOG_SUPERSEDED" {
		t.Fatalf("the old wording cannot be taken now: %v", err)
	}

	// Declining: one write, the reason kept, the offer gone.
	bare, _ := r.CatalogAdd(ctx, admin, tka.CatalogAddInput{Text: "Before analysing a market, name the principles it rests on.",
		Tags: tka.InstructionTags{What: []string{"analysis"}, When: []string{"before-analysis"}}})
	if _, err := r.CatalogReview(ctx, admin, tka.CatalogReviewInput{ID: bare.ID, Decision: "approve", Source: "s", IncidentOn: "2026-10-01"}); code(t, err) != "CATALOG_NO_INCIDENT" {
		t.Fatalf("no incident, no approval: %v", err)
	}
	other, _ := r.CatalogAdd(ctx, admin, tka.CatalogAddInput{Text: "Before analysing a strategy, name the principles it rests on.",
		Tags: tka.InstructionTags{What: []string{"analysis"}, When: []string{"before-analysis"}}, Evidence: "An analysis came out wrong until the principles were named."})
	if _, err := r.CatalogReview(ctx, admin, tka.CatalogReviewInput{ID: other.ID, Decision: "approve", Source: "a founder's store, UAE", IncidentOn: "2026-10-01"}); err != nil {
		t.Fatal(err)
	}
	dec, err := r.RetireInstruction(ctx, a, other.ID, "this store holds no strategy work")
	if err != nil || dec.Status != "retired" || !strings.HasPrefix(dec.Retired.Reason, "declined: ") || *dec.CatalogRef != other.ID {
		t.Fatalf("decline: %+v %v", dec, err)
	}
	if _, err := r.RetireInstruction(ctx, a, other.ID, "again"); code(t, err) != "ALREADY_RETIRED" {
		t.Fatalf("declining twice: %v", err)
	}
	if _, offers, _ := r.InstructionsWithOffers(ctx, "", false); slices.Contains(catalogIDs(offers.New), other.ID) {
		t.Fatalf("a declined entry is not offered again: %+v", offers.New)
	}
	if active, _ := r.Instructions(ctx, "", false); slices.ContainsFunc(active, func(in tka.Instruction) bool { return in.ID == dec.ID }) {
		t.Fatal("a declined entry is not an instruction of the store")
	}

	// Withdrawn: the store keeps its line and is told.
	if _, err := r.CatalogWithdraw(ctx, admin, "BP-3", "folded into BP-2"); err != nil {
		t.Fatal(err)
	}
	_, offers, _ = r.InstructionsWithOffers(ctx, "", false)
	if len(offers.Withdrawn) != 1 || offers.Withdrawn[0].Entry != "BP-3" || offers.Withdrawn[0].Line != "I-3" {
		t.Fatalf("withdrawn notice: %+v", offers.Withdrawn)
	}
	if list, _ := r.Instructions(ctx, "", false); !slices.ContainsFunc(list, func(in tka.Instruction) bool { return in.ID == "I-3" }) {
		t.Fatal("a withdrawn entry stays in the store until the person retires it")
	}

	// A rejection says why; the maintainer only decides proposals.
	for _, c := range []struct {
		err  error
		want string
	}{
		{func() error {
			_, e := r.CatalogReview(ctx, admin, tka.CatalogReviewInput{ID: "BP-1", Decision: "approve"})
			return e
		}(), "CATALOG_NOT_PROPOSED"},
		{func() error {
			_, e := r.CatalogReview(ctx, admin, tka.CatalogReviewInput{ID: bp2.ID, Decision: "reject"})
			return e
		}(), "EMPTY_REASON"},
		{func() error { _, e := r.CatalogWithdraw(ctx, admin, "BP-28", "x"); return e }(), "CATALOG_NOT_CURRENT"},
		{func() error { _, e := r.CatalogAdd(ctx, admin, tka.CatalogAddInput{Text: "Untagged."}); return e }(), "INVALID_TAG"},
	} {
		if got := code(t, c.err); got != c.want {
			t.Fatalf("want %s, got %v", c.want, c.err)
		}
	}
}

// A store seeded before the catalog existed is read against it by its words:
// its default lines are the starters, so nothing is offered twice; a line the
// person wrote that the catalog later took up word for word is the entry.
func TestCatalog_OldStoresMatchByText(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	// The default list as a store seeded it before 0036 — without directive
	// 13, which joined on 14/10 — and with no catalog_ref.
	old := slices.Delete(slices.Clone(tka.DefaultInstructions()), 14, 15)
	for i, d := range old {
		if _, err := r.Pool.Exec(ctx, `INSERT INTO instruction (tenant_id, num, kind, label, text, origin, term, why, rationale_ref, signer, producer, grant_id)
			VALUES ('default', $1, $2, $3, $4, 'default', nullif($5, ''), nullif($6, ''), nullif($7, ''), 'tk', 'tk-default-instructions', 'default')`,
			i+1, d.Kind, d.Label, d.Text, d.Term, d.Why, d.RationaleRef); err != nil {
			t.Fatal(err)
		}
	}
	_, offers, err := r.InstructionsWithOffers(ctx, "", false)
	if err != nil || !slices.Equal(catalogIDs(offers.New), []string{"BP-15"}) {
		t.Fatalf("a store seeded with 26 lines is offered the 27th only: %+v %v", offers, err)
	}
	// The owner's directive 27 was written by hand before it joined the
	// defaults: the same words are the same entry.
	last := tka.DefaultInstructions()[14]
	if _, err := r.AddInstruction(ctx, a, tka.InstructionAddInput{Text: last.Text}); err != nil {
		t.Fatal(err)
	}
	if _, offers, _ := r.InstructionsWithOffers(ctx, "", false); len(offers.New) != 0 {
		t.Fatalf("the person's own copy is the entry: %+v", offers.New)
	}
}

// A store's grant cannot write the catalog: it is another tenant.
func TestCatalog_StoreGrantCannotWriteIt(t *testing.T) {
	ctx := context.Background()
	r, _ := newRepo(t)
	if _, err := r.Catalog(ctx, tka.CatalogFilter{}); err != nil {
		t.Fatal(err)
	}
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET ROLE tk_api`); err != nil {
		t.Skipf("no tk_api role here: %v", err)
	}
	defer conn.Exec(ctx, `RESET ROLE`)
	if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', 'demo-x', false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO catalog_review (instruction_id, decision, signer, producer, grant_id)
		VALUES ('I-1', 'reject', 'x', 'x', 'x')`); err == nil {
		t.Fatal("a store's session must not review catalog entries")
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM instruction WHERE tenant_id = 'catalog'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a store's session does not even read catalog rows directly: %d %v", n, err)
	}
}
