package tka_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// The seed is the owner's list as written: META M1–M2, directives 1–13 in seven
// groups, and twelve definitions each with its why and docs pointer; the copy
// compiled in is the one in the docs repo. Directive 13 is directive 27 of the
// owner's own store (I-43), joined to the default set with its text unchanged
// (owner 13/10/2026): testdata/owner-directive-27.txt is that text as the
// owner's store holds it.
func TestDefaultInstructions_TheOwnersList(t *testing.T) {
	got := tka.DefaultInstructions()
	var labels []string
	kinds := map[string]int{}
	for _, d := range got {
		labels = append(labels, d.Label)
		kinds[d.Kind]++
		if strings.TrimSpace(d.Text) == "" || strings.Contains(d.Text, "**") || strings.Contains(d.Text, "*Why:*") {
			t.Fatalf("%s: %+v", d.Label, d)
		}
		if (d.Kind == "definition") != (d.Why != "" && strings.HasPrefix(d.RationaleRef, "docs/")) {
			t.Fatalf("%s: a definition, and only a definition, carries why and rationale_ref: %+v", d.Label, d)
		}
	}
	if kinds["meta"] != 2 || kinds["directive"] != 13 || kinds["definition"] != 12 {
		t.Fatalf("kinds: %v", kinds)
	}
	want := "M1 M2 1 2 3 4 5 6 7 8 9 10 11 12 13 def:claim def:truth [T] def:assumption [A] def:document def:document/bound-vs-unbound " +
		"def:summary def:thread def:conversation def:principle def:instruction def:flag def:attester"
	if strings.Join(labels, " ") != want {
		t.Fatalf("labels:\n got %s\nwant %s", strings.Join(labels, " "), want)
	}
	if m := got[0]; m.Title != "How to read every instruction" || !strings.HasPrefix(m.Text, "An instruction is guidance to follow") {
		t.Fatalf("M1: %+v", m)
	}
	if d := got[5]; d.Group != "Group II — How to write a claim (so it passes the gate)" || !strings.HasSuffix(d.Text, "M1 does not override it.") {
		t.Fatalf("directive 4 runs over two lines and names M1: %+v", d)
	}
	if d := got[13]; d.Label != "12" || d.Group != "Group VI — Evidence without a link" {
		t.Fatalf("directive 12: %+v", d)
	}
	owner, err := os.ReadFile("testdata/owner-directive-27.txt")
	if err != nil {
		t.Fatal(err)
	}
	if d := got[14]; d.Label != "13" || d.Group != "Group VII — When recording costs more than it keeps" || d.Text != strings.TrimRight(string(owner), "\n") {
		t.Fatalf("directive 13 is the owner's directive 27, word for word:\n got %q\nwant %q", d.Text, owner)
	}
	if d := got[23]; d.Term != "principle" || d.Why != "principles and sourced facts are the only things trusted without audit; everything else is an assumption." ||
		d.RationaleRef != "docs/principle" {
		t.Fatalf("a definition whose pointer sits on its own line: %+v", d)
	}

	mvp, err := os.ReadFile("../../../mvp/tk-default-instructions.md")
	if os.IsNotExist(err) {
		t.Skip("docs repo not checked out next to this one")
	}
	here, err2 := os.ReadFile("defaults/tk-default-instructions.md")
	if err != nil || err2 != nil || string(mvp) != string(here) {
		t.Fatalf("defaults/tk-default-instructions.md must equal mvp/tk-default-instructions.md (%v %v)", err, err2)
	}
}

// byLabel is the active instruction a list calls label.
func byLabel(t *testing.T, list []tka.Instruction, label string) tka.Instruction {
	t.Helper()
	for _, in := range list {
		if in.Label == label {
			return in
		}
	}
	t.Fatalf("no %s", label)
	return tka.Instruction{}
}

// A store's instructions start as the default list; the person adds, replaces
// and retires, never edits, and the history keeps every one with when and why.
// Wording written twice is caught as for claims; meaning is not.
func TestInstructions_AddSupersedeRetire(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)

	list, err := r.Instructions(ctx, "", false)
	if err != nil || len(list) != 27 || list[0].ID != "I-1" || list[26].ID != "I-27" {
		t.Fatalf("the defaults: %d %v", len(list), err)
	}
	// Reading writes nothing: the defaults are shown as they will be seeded,
	// and the first add or retire seeds them.
	var rows int
	if err := r.Pool.QueryRow(ctx, `SELECT count(*) FROM instruction WHERE tenant_id <> 'catalog'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a read seeded %d rows (%v)", rows, err)
	}
	if m := list[0]; m.Kind != "meta" || m.Label != "M1" || m.Origin != "default" || m.Title == nil || m.Status != "active" || m.Signer != "tk" {
		t.Fatalf("I-1 is M1: %+v", m)
	}
	if d := byLabel(t, list, "def:claim"); d.Kind != "definition" || d.Term == nil || *d.Term != "claim" || d.Why == nil || d.RationaleRef == nil || *d.RationaleRef != "docs/claim" {
		t.Fatalf("def:claim: %+v", d)
	}
	if again, _ := r.Instructions(ctx, "", false); len(again) != 27 {
		t.Fatalf("second read: %d", len(again))
	}

	add := func(in tka.InstructionAddInput) tka.InstructionAddResult {
		t.Helper()
		res, err := r.AddInstruction(ctx, a, in)
		if err != nil {
			t.Fatalf("add %+v: %v", in, err)
		}
		return res
	}
	// A directive by default, numbered after the last.
	res := add(tka.InstructionAddInput{Text: "  Write LinkedIn comments in English.  "})
	en := res.Instruction
	if en == nil || en.ID != "I-28" || en.Kind != "directive" || en.Label != "14" || en.Text != "Write LinkedIn comments in English." ||
		en.Origin != "added" || en.Signer != "bao" || en.Producer != "claude-desktop" {
		t.Fatalf("add: %+v", res)
	}
	// Close wording: nothing written, similar[] back, until confirmed.
	res = add(tka.InstructionAddInput{Text: "Write LinkedIn comments in English, always."})
	if res.Instruction != nil || len(res.Similar) != 1 || res.Similar[0].ID != "I-28" || res.Similar[0].Label != "14" || res.Similar[0].Reason != "contains" {
		t.Fatalf("close wording: %+v", res)
	}
	if res = add(tka.InstructionAddInput{Text: "Write LinkedIn comments in English, always.", ConfirmNew: true}); res.Instruction == nil || res.Instruction.Label != "15" {
		t.Fatalf("confirmed: %+v", res)
	}
	// The same words return the one that has them.
	if res = add(tka.InstructionAddInput{Text: "write linkedin comments   in English"}); !res.Existing || res.Instruction.ID != "I-28" {
		t.Fatalf("same words: %+v", res)
	}
	// Meaning is not judged: one word swapped is under the claims' 0.90.
	if res = add(tka.InstructionAddInput{Text: "Write LinkedIn comments in Vietnamese.", ConfirmNew: false}); res.Instruction == nil || res.Instruction.Label != "16" {
		t.Fatalf("a contradiction in other words is written: %+v", res)
	}
	// A default restated with a clause dropped is caught.
	if res = add(tka.InstructionAddInput{Text: "When recalling a recorded T/A in chat, quote the claim verbatim."}); res.Instruction != nil ||
		len(res.Similar) == 0 || res.Similar[0].Label != "6" {
		t.Fatalf("a default restated: %+v", res)
	}

	// Meta and definition are named by kind; a definition needs term and why,
	// and a second definition of a term is caught whatever its words.
	if res = add(tka.InstructionAddInput{Kind: "meta", Title: "Who may add", Text: "Any agent holding the owner's grant may add an instruction once the owner approves it in chat."}); res.Instruction == nil || res.Instruction.Label != "M3" {
		t.Fatalf("meta: %+v", res)
	}
	def := tka.InstructionAddInput{Kind: "definition", Term: "delta", Text: "The set of claims a chat hands to the store at the end of a session.",
		Why: "a handoff written as claims can be checked, a chat log cannot.", RationaleRef: "docs/delta"}
	if res = add(def); res.Instruction == nil || res.Instruction.Label != "def:delta" || res.Instruction.Why == nil {
		t.Fatalf("definition: %+v", res)
	}
	def.Text, def.Why = "What moves from one conversation into the ledger, as claims.", "it keeps the store current."
	if res = add(def); res.Instruction != nil || len(res.Similar) != 1 || res.Similar[0].Reason != "same_term" {
		t.Fatalf("a term defined twice: %+v", res)
	}
	def.Supersedes = res.Similar[0].ID
	if res = add(def); res.Instruction == nil || res.Instruction.Supersedes == nil {
		t.Fatalf("a definition replaced: %+v", res)
	}

	// Supersede: the replaced one is not compared, other close ones are.
	in := tka.InstructionAddInput{Text: "Write LinkedIn comments in the language of the post.", Supersedes: "i-28"}
	if res = add(in); res.Instruction != nil || len(res.Similar) != 1 || res.Similar[0].Label != "16" || res.Similar[0].Reason != "restated" {
		t.Fatalf("supersede, I-28 left out: %+v", res)
	}
	in.ConfirmNew = true
	next := add(in).Instruction
	if next == nil || next.Label != "17" || next.Supersedes == nil || *next.Supersedes != "I-28" {
		t.Fatalf("supersede: %+v", next)
	}

	if _, err := r.RetireInstruction(ctx, a, next.ID, "  "); code(t, err) != "EMPTY_REASON" {
		t.Fatalf("no reason: %v", err)
	}
	ret, err := r.RetireInstruction(ctx, a, "I-14", "this store keeps no outreach")
	if err != nil || ret.Label != "12" || ret.Status != "retired" || ret.Retired == nil || ret.Retired.Reason != "this store keeps no outreach" {
		t.Fatalf("retire a default: %+v %v", ret, err)
	}
	// Retired words come back as similar, with the status, not written.
	if res = add(tka.InstructionAddInput{Text: "Write LinkedIn comments in English."}); res.Instruction != nil || len(res.Similar) == 0 ||
		res.Similar[0].ID != "I-28" || res.Similar[0].Reason != "same" || res.Similar[0].Status != "retired" {
		t.Fatalf("retired wording: %+v", res)
	}

	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"retire twice", func() error { _, e := r.RetireInstruction(ctx, a, "I-14", "again"); return e }(), "ALREADY_RETIRED"},
		{"supersede a retired one", func() error {
			_, e := r.AddInstruction(ctx, a, tka.InstructionAddInput{Text: "x y z", Supersedes: "I-28"})
			return e
		}(), "ALREADY_RETIRED"},
		{"unknown", func() error { _, e := r.RetireInstruction(ctx, a, "I-999", "gone"); return e }(), "INSTRUCTION_UNKNOWN"},
		{"not an instruction id", func() error { _, e := r.RetireInstruction(ctx, a, "T-1", "gone"); return e }(), "INSTRUCTION_UNKNOWN"},
		{"empty", func() error { _, e := r.AddInstruction(ctx, a, tka.InstructionAddInput{Text: " "}); return e }(), "EMPTY_INSTRUCTION"},
		{"too long", func() error {
			_, e := r.AddInstruction(ctx, a, tka.InstructionAddInput{Text: strings.Repeat("word ", 500)})
			return e
		}(), "INSTRUCTION_TOO_LONG"},
		{"unknown kind", func() error {
			_, e := r.AddInstruction(ctx, a, tka.InstructionAddInput{Kind: "rule", Text: "x"})
			return e
		}(), "INVALID_INSTRUCTION"},
		{"definition without why", func() error {
			_, e := r.AddInstruction(ctx, a, tka.InstructionAddInput{Kind: "definition", Term: "x", Text: "x"})
			return e
		}(), "INVALID_INSTRUCTION"},
		{"directive with a term", func() error {
			_, e := r.AddInstruction(ctx, a, tka.InstructionAddInput{Term: "x", Text: "x"})
			return e
		}(), "INVALID_INSTRUCTION"},
		{"signer is an agent", func() error {
			r.Agents = map[string]bool{"claude": true}
			defer func() { r.Agents = nil }()
			_, e := r.AddInstruction(ctx, tka.Actor{UserID: "claude", ActorID: "claude", GrantID: "g"}, tka.InstructionAddInput{Text: "x"})
			return e
		}(), "SIGNER_IS_AGENT"},
	} {
		if got := code(t, c.err); got != c.want {
			t.Fatalf("%s: want %s, got %v", c.name, c.want, c.err)
		}
	}

	active, _ := r.Instructions(ctx, "", false)
	all, _ := r.Instructions(ctx, "", true)
	if len(active) != 27-1+5 || len(all) != 27+7 {
		// added 14 15 16 M3 def:delta def:delta' 17; retired 14 (by 17), def:delta, 12
		t.Fatalf("active %d, all %d", len(active), len(all))
	}
	old := byLabel(t, all, "14")
	if old.Status != "retired" || old.Retired == nil || old.Retired.Reason != "superseded by "+next.ID ||
		old.Retired.SupersededBy == nil || *old.Retired.SupersededBy != next.ID || old.Retired.At.Before(old.CreatedAt) {
		t.Fatalf("14 in history: %+v %+v", old, old.Retired)
	}

	if _, err := r.Pool.Exec(ctx, `UPDATE instruction SET text = 'x' WHERE id = 'I-1'`); err == nil {
		t.Fatal("instruction must be append-only")
	}
	if _, err := r.Pool.Exec(ctx, `DELETE FROM instruction_retired`); err == nil {
		t.Fatal("instruction_retired must be append-only")
	}
}

// Each store has its own list. Seeding is the store's own act, not a write
// against a trial store's cap; adding one is, and a caught duplicate is not.
func TestInstructions_PerStoreAndWrites(t *testing.T) {
	ctx := context.Background()
	r, base := newRepo(t)
	if _, err := r.CreateTenant(ctx, tka.Tenant{ID: "capped", Tier: "trial"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetWriteCap(ctx, "capped", 100); err != nil {
		t.Fatal(err)
	}
	a := base
	a.TenantID = "capped"

	before, _ := r.ReadTenant(ctx, "capped")
	if list, err := r.Instructions(ctx, "capped", false); err != nil || len(list) != 27 {
		t.Fatalf("capped store seeded: %d %v", len(list), err)
	}
	mid, _ := r.ReadTenant(ctx, "capped")
	if mid.Writes != before.Writes {
		t.Fatalf("seeding is not a write: %d → %d", before.Writes, mid.Writes)
	}
	// Retiring before ever reading seeds first, so the default exists to retire.
	if _, err := r.RetireInstruction(ctx, base, "I-2", "default store only"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddInstruction(ctx, a, tka.InstructionAddInput{Text: "Cite the post URL in every outreach claim."}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Cite the post URL in every outreach claim.", "Cite the post URL in every outreach claim, without exception."} {
		if res, err := r.AddInstruction(ctx, a, tka.InstructionAddInput{Text: text}); err != nil || (!res.Existing && len(res.Similar) == 0) {
			t.Fatalf("%q: %+v %v", text, res, err)
		}
	}
	after, _ := r.ReadTenant(ctx, "capped")
	if after.Writes != mid.Writes+1 {
		t.Fatalf("one write for the one added: %d → %d", mid.Writes, after.Writes)
	}
	capped, _ := r.Instructions(ctx, "capped", false)
	def, _ := r.Instructions(ctx, "", false)
	if len(capped) != 28 || capped[1].ID != "I-2" || capped[27].Label != "14" || len(def) != 26 {
		t.Fatalf("stores apart: capped %d, default %d", len(capped), len(def))
	}
}

// Tags are set by label or id, the latest row wins, the old ones stay, and
// the table takes no UPDATE or DELETE (owner 13/10/2026). A store that tags
// a line it never wrote has the defaults seeded first, as for retire.
func TestInstructions_TagLatestWinsAppendOnly(t *testing.T) {
	ctx := context.Background()
	r, a := newRepo(t)
	got, err := r.TagInstruction(ctx, a, tka.InstructionTagInput{ID: "12", InstructionTags: tka.InstructionTags{
		What: []string{"document"}, When: []string{"before-write"}, Why: "first try"}})
	if err != nil || got.ID != "I-14" || got.Tags.Source != "tagged" || got.Tags.Why != "first try" {
		t.Fatalf("tag by label: %+v %v", got, err)
	}
	if _, err := r.TagInstruction(ctx, a, tka.InstructionTagInput{ID: "I-14", InstructionTags: tka.InstructionTags{
		What: []string{"document"}, When: []string{"before-write:unbound"}, Why: "second"}}); err != nil {
		t.Fatal(err)
	}
	list, _ := r.Instructions(ctx, "", false)
	if l := byLabel(t, list, "12"); l.Tags.Why != "second" || l.Tags.When[0] != "before-write:unbound" {
		t.Fatalf("latest wins: %+v", l.Tags)
	}
	if l := byLabel(t, list, "13"); l.Tags.Source != "catalog" || l.Tags.When[0] != "on-cost" {
		t.Fatalf("an untouched default keeps its catalog entry's tags: %+v", l.Tags)
	}
	for _, c := range []struct {
		in   tka.InstructionTagInput
		want string
	}{
		{tka.InstructionTagInput{ID: "99", InstructionTags: tka.InstructionTags{When: []string{"before-write"}}}, "INSTRUCTION_UNKNOWN"},
		{tka.InstructionTagInput{ID: "12", InstructionTags: tka.InstructionTags{When: []string{"later"}}}, "INVALID_TAG"},
		{tka.InstructionTagInput{ID: "12"}, "INVALID_TAG"},
	} {
		if _, err := r.TagInstruction(ctx, a, c.in); code(t, err) != c.want {
			t.Fatalf("%+v: want %s, got %v", c.in, c.want, err)
		}
	}
	var rows int
	if err := r.Pool.QueryRow(ctx, `SELECT count(*) FROM instruction_tag WHERE tenant_id <> 'catalog'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("both rows kept: %d %v", rows, err)
	}
	if _, err := r.Pool.Exec(ctx, `UPDATE instruction_tag SET tag_why = 'x'`); err == nil {
		t.Fatal("instruction_tag must be append-only")
	}
	if _, err := r.Pool.Exec(ctx, `DELETE FROM instruction_tag`); err == nil {
		t.Fatal("instruction_tag must be append-only")
	}
}
