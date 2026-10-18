package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// Through the real JSON-RPC path and a real tk-api: an agent loads the store's
// instructions with one call — the meta lines in full and an index of the rest
// with their tags (owner 13/10/2026) — the person adds, replaces and retires
// them, and the history is a parameter on the same read (owner 5/10/2026).
func TestInstructions_LoadAddRetire(t *testing.T) {
	s, env, _ := newMCP(t)
	env.API.Contact = "support@tknowledge.org"
	type inst struct {
		ID         string  `json:"id"`
		Kind       string  `json:"kind"`
		Label      string  `json:"label"`
		Text       string  `json:"text"`
		Origin     string  `json:"origin"`
		Status     string  `json:"status"`
		Why        *string `json:"why"`
		Ref        *string `json:"rationale_ref"`
		Supersedes *string `json:"supersedes"`
		Retired    *struct {
			Reason       string  `json:"reason"`
			SupersededBy *string `json:"superseded_by"`
		} `json:"retired"`
	}
	type row struct {
		ID    string  `json:"id"`
		Kind  string  `json:"kind"`
		Label string  `json:"label"`
		Text  *string `json:"text"`
		Tags  *struct {
			When   []string `json:"when"`
			Why    string   `json:"why"`
			Source string   `json:"source"`
		} `json:"tags"`
		Gist    string `json:"gist"`
		Status  string `json:"status"`
		Retired *struct {
			SupersededBy *string `json:"superseded_by"`
		} `json:"retired"`
	}
	type list struct {
		Store struct {
			FeedbackEmail string `json:"feedback_email"`
			StoreVersion  string `json:"store_version"`
		} `json:"store"`
		Meta  []inst `json:"meta"`
		Index []row  `json:"index"`
		Count int    `json:"count"`
		Help  string `json:"help"`
	}
	byLabel := func(rows []row, label string) row {
		t.Helper()
		for _, r := range rows {
			if r.Label == label && r.Status == "active" {
				return r
			}
		}
		for _, r := range rows {
			if r.Label == label {
				return r
			}
		}
		t.Fatalf("no %s in the index", label)
		return row{}
	}

	got := into[list](t, tool(t, s, ToolInstructions, map[string]any{}))
	if got.Count != 27 || len(got.Meta) != 2 || len(got.Index) != 25 ||
		got.Meta[0].Label != "M1" || !strings.Contains(got.Meta[0].Text, "Purpose outranks wording") || !strings.Contains(got.Help, "help(") {
		t.Fatalf("meta in full, the rest as an index: %+v", got)
	}
	for _, r := range got.Index {
		if r.Text != nil || r.Tags == nil || r.Tags.Source != "catalog" || r.Tags.Why == "" {
			t.Fatalf("an index row carries tags, not text: %+v", r)
		}
	}
	if r := byLabel(got.Index, "13"); r.Tags.When[0] != "on-cost" {
		t.Fatalf("the default set ends with the owner's directive 27: %+v", r)
	}
	if got.Store.FeedbackEmail != "support@tknowledge.org" || got.Store.StoreVersion == "" {
		t.Fatalf("store metadata comes with the instructions: %+v", got.Store)
	}

	type addOut struct {
		Instruction *inst `json:"instruction"`
		Existing    bool  `json:"existing"`
		Similar     []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Reason string `json:"reason"`
		} `json:"similar"`
	}
	if r := into[addOut](t, tool(t, s, ToolInstructionAdd, map[string]any{"text": "A claim must be decontextualized: enough names, dates and quantities to read it a year later."})); r.Instruction != nil || len(r.Similar) == 0 || r.Similar[0].Label != "5" {
		t.Fatalf("close to directive 5: %+v", r)
	}
	out := into[addOut](t, tool(t, s, ToolInstructionAdd, map[string]any{"text": "Comment in the language of the post.", "supersedes": "I-14",
		"tags": map[string]any{"who": []string{"agent"}, "what": []string{"message"}, "when": []string{"before-draft"}, "where": []string{"linkedin"},
			"why": "a comment in another language is not read", "how": "match the post's language"}}))
	if out.Instruction == nil || out.Instruction.Label != "14" || out.Instruction.Kind != "directive" || out.Instruction.Supersedes == nil || *out.Instruction.Supersedes != "I-14" {
		t.Fatalf("add: %+v", out)
	}
	def := into[addOut](t, tool(t, s, ToolInstructionAdd, map[string]any{"kind": "definition", "term": "delta",
		"text": "The claims a chat hands to the store at the end of a session.", "why": "a handoff written as claims can be checked.", "rationale_ref": "docs/delta"}))
	if def.Instruction == nil || def.Instruction.Label != "def:delta" {
		t.Fatalf("definition: %+v", def)
	}
	retired := into[inst](t, tool(t, s, ToolInstructionRetire, map[string]any{"id": "I-13", "reason": "this store keeps no summaries"}))
	if retired.Status != "retired" || retired.Label != "11" || retired.Retired == nil || retired.Retired.Reason != "this store keeps no summaries" {
		t.Fatalf("retire: %+v", retired)
	}
	got = into[list](t, tool(t, s, ToolInstructions, map[string]any{}))
	if got.Count != 27 || len(got.Index) != 25 || byLabel(got.Index, "14").Tags == nil || byLabel(got.Index, "14").Tags.Source != "tagged" {
		t.Fatalf("active after: %+v", got)
	}
	if r := byLabel(got.Index, "def:delta"); r.Tags != nil || !strings.HasPrefix(r.Gist, "The claims a chat hands") {
		t.Fatalf("an untagged line shows the start of its text: %+v", r)
	}
	all := into[list](t, tool(t, s, ToolInstructions, map[string]any{"history": true}))
	if all.Count != 29 || byLabel(all.Index, "12").Retired == nil || byLabel(all.Index, "12").Retired.SupersededBy == nil {
		t.Fatalf("history: %+v", all)
	}

	type errOut struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if r := tool(t, s, ToolInstructionRetire, map[string]any{"id": "I-13", "reason": "again"}); !r.IsError || into[errOut](t, r).Error.Code != "ALREADY_RETIRED" {
		t.Fatalf("retire twice: %s", r.Structured)
	}
	if r := tool(t, s, ToolInstructionAdd, map[string]any{"text": "Sign every note.", "tags": map[string]any{"when": []string{"someday"}}}); !r.IsError || into[errOut](t, r).Error.Code != "INVALID_TAG" {
		t.Fatalf("a tag outside the vocabulary: %s", r.Structured)
	}
}

// help through JSON-RPC and tk-api: only the lines that fit the situation, in
// full; a line the person added and tagged comes back with them; a store that
// has never written an instruction answers from the default set without
// writing one.
func TestHelp_BySituation(t *testing.T) {
	s, env, _ := newMCP(t)
	type line struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Text  string `json:"text"`
	}
	type helpOut struct {
		Directives  []line              `json:"directives"`
		Definitions []line              `json:"definitions"`
		ReadAs      map[string][]string `json:"read_as"`
		Count       int                 `json:"count"`
		Next        string              `json:"next"`
	}
	labels := func(ls []line) string {
		out := []string{}
		for _, l := range ls {
			out = append(out, l.Label)
		}
		return strings.Join(out, " ")
	}
	got := into[helpOut](t, tool(t, s, ToolHelp, map[string]any{"situation": "record a decision"}))
	if labels(got.Directives) != "1 2 4 5" || len(got.Definitions) != 0 || !strings.HasPrefix(got.Directives[0].Text, "Before `admit`/`assume`, list in chat") {
		t.Fatalf("record a decision: %+v", got)
	}
	var rows int
	if err := env.Admin.QueryRow(context.Background(), `SELECT count(*) FROM instruction WHERE tenant_id <> 'catalog'`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("help wrote %d instruction rows (%v)", rows, err)
	}
	if got := into[helpOut](t, tool(t, s, ToolHelp, map[string]any{"situation": "what is a T"})); labels(got.Definitions) != "def:truth [T]" || len(got.Directives) != 0 {
		t.Fatalf("what is a T: %+v", got)
	}

	tok := env.Token(t, "bao", "claude-desktop")
	owner := tkclient.New(env.Server.URL).WithToken(tok)
	if _, err := owner.TkaInstructionAdd(context.Background(), map[string]any{"text": "Open every LinkedIn note with the reader's first name.",
		"tags": map[string]any{"what": []string{"message"}, "when": []string{"before-draft"}, "where": []string{"linkedin"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.TkaInstructionAdd(context.Background(), map[string]any{"text": "Quote every price with its currency."}); err != nil {
		t.Fatal(err)
	}
	// Tags set afterwards, by label, the way `tk instructions tag` applies a file.
	if _, err := owner.TkaInstructionTag(context.Background(), map[string]any{"id": "15", "what": []string{"message"},
		"when": []string{"before-draft"}, "where": []string{"email"}}); err != nil {
		t.Fatal(err)
	}
	got = into[helpOut](t, tool(t, s, ToolHelp, map[string]any{"situation": "draft linkedin message"}))
	if labels(got.Directives) != "14" || !strings.HasPrefix(got.Directives[0].Text, "Open every LinkedIn note") {
		t.Fatalf("draft linkedin message: %+v", got)
	}
	if got := into[helpOut](t, tool(t, s, ToolHelp, map[string]any{"situation": "draft an email"})); labels(got.Directives) != "15" {
		t.Fatalf("draft an email: %+v", got)
	}
	if got := into[helpOut](t, tool(t, s, ToolHelp, map[string]any{"situation": "weather in Dubai"})); got.Count != 0 || got.Next == "" {
		t.Fatalf("nothing fits: %+v", got)
	}
	type errOut struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if r := tool(t, s, ToolHelp, map[string]any{"situation": "  "}); !r.IsError || into[errOut](t, r).Error.Code != "EMPTY_SITUATION" {
		t.Fatalf("no situation: %s", r.Structured)
	}
	if _, err := owner.TkaInstructionTag(context.Background(), map[string]any{"id": "99", "when": []string{"before-draft"}}); err == nil {
		t.Fatal("tagging a line that does not exist must be refused")
	}
}

// Store metadata is one record for the deployment: every grant reads it,
// only tk:admin writes it (through org config), and an unset feedback
// address falls back to TK_CONTACT.
func TestStoreInfo_ReadByAllSetByAdmin(t *testing.T) {
	s, env, _ := newMCP(t)
	env.API.Contact = "support@tknowledge.org"
	type info struct {
		MaintainerName string `json:"maintainer_name"`
		FeedbackEmail  string `json:"feedback_email"`
		FeedbackURL    string `json:"feedback_url"`
		StoreVersion   string `json:"store_version"`
	}
	if got := into[info](t, tool(t, s, ToolStoreInfo, map[string]any{})); got.FeedbackEmail != "support@tknowledge.org" || got.MaintainerName != "" || got.StoreVersion == "" {
		t.Fatalf("defaults: %+v", got)
	}

	owner := tkclient.New(env.Server.URL).WithToken(env.Token(t, "bao", "claude-desktop"))
	if _, err := owner.OrgConfigSet(context.Background(), "store.feedback_email", json.RawMessage(`"bao@example.com"`)); err == nil {
		t.Fatal("a grant without tk:admin must not change the store metadata")
	}
	admin := tkclient.New(env.Server.URL).WithToken(env.Token(t, "bao", "tk-cli", grant.ScopeAdmin))
	for k, v := range map[string]string{"store.feedback_email": `"bao@example.com"`, "store.maintainer_name": `"Lan Nguyen"`,
		"store.feedback_url": `"https://operator.example"`} {
		if _, err := admin.OrgConfigSet(context.Background(), k, json.RawMessage(v)); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	if _, err := admin.OrgConfigSet(context.Background(), "store.feedback_email", json.RawMessage(`"Bao <not an address"`)); err == nil {
		t.Fatal("a feedback address must be an email address")
	}
	if _, err := admin.OrgConfigSet(context.Background(), "store.feedback_url", json.RawMessage(`"operator.example"`)); err == nil {
		t.Fatal("a feedback page must be an http(s) URL")
	}
	if got := into[info](t, tool(t, s, ToolStoreInfo, map[string]any{})); got.FeedbackEmail != "bao@example.com" || got.MaintainerName != "Lan Nguyen" || got.FeedbackURL != "https://operator.example" {
		t.Fatalf("after set: %+v", got)
	}
}

// lookup takes scope="summaries" and thread takes summaries="all", end to
// end; anything else for either is refused rather than ignored.
func TestLookupScopeAndThreadSummaries(t *testing.T) {
	s, _, _ := newMCP(t)
	type th struct {
		ID string `json:"id"`
	}
	open := into[th](t, tool(t, s, ToolThreadOpen, map[string]any{"title": "Bao works in small moves on the Acme matter", "keywords": []string{"acme"}}))
	if r := tool(t, s, ToolPrincipleAdd, map[string]any{"code": "P.1", "statement": "Small moves, each one checkable."}); r.IsError {
		t.Fatalf("principle: %s", r.Structured)
	}
	if r := tool(t, s, ToolLink, map[string]any{"thread": open.ID, "ids": []string{"P.1"}}); r.IsError {
		t.Fatalf("link: %s", r.Structured)
	}

	type view struct {
		Summary   *struct{ ID string } `json:"summary"`
		Summaries *[]struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"summaries"`
	}
	if v := into[view](t, tool(t, s, ToolThread, map[string]any{"thread": open.ID, "summaries": "all"})); v.Summaries == nil || len(*v.Summaries) != 0 {
		t.Fatalf("asked with none written: %+v", v)
	}
	for _, text := range []string{"Acme moves in small steps (P.1).", "Acme keeps moving in small, checkable steps (P.1)."} {
		if r := tool(t, s, ToolSummarize, map[string]any{"thread": open.ID, "text": text, "author": "owner"}); r.IsError {
			t.Fatalf("summarize: %s", r.Structured)
		}
	}
	if v := into[view](t, tool(t, s, ToolThread, map[string]any{"thread": open.ID})); v.Summary == nil || v.Summary.ID != "S-2" || v.Summaries != nil {
		t.Fatalf("default view: %+v", v)
	}
	v := into[view](t, tool(t, s, ToolThread, map[string]any{"thread": open.ID, "summaries": "all"}))
	if v.Summaries == nil || len(*v.Summaries) != 2 || (*v.Summaries)[0].Status != "superseded" || (*v.Summaries)[1].ID != "S-2" {
		t.Fatalf("all: %+v", v.Summaries)
	}

	type hit struct {
		ID     string `json:"id"`
		Thread string `json:"thread"`
		Stale  bool   `json:"stale"`
	}
	type found struct {
		Threads   *[]any `json:"threads"`
		Documents *[]any `json:"documents"`
		Summaries []hit  `json:"summaries"`
	}
	full := into[found](t, tool(t, s, ToolLookup, map[string]any{"q": "checkable steps"}))
	if full.Threads == nil || full.Documents == nil || len(full.Summaries) != 1 || full.Summaries[0].ID != "S-2" || full.Summaries[0].Thread != open.ID {
		t.Fatalf("every block, summaries last: %+v", full)
	}
	only := into[found](t, tool(t, s, ToolLookup, map[string]any{"q": "checkable steps", "scope": "summaries"}))
	if only.Threads != nil || only.Documents != nil || len(only.Summaries) != 1 || only.Summaries[0].ID != "S-2" {
		t.Fatalf("scope=summaries: %+v", only)
	}
	if r := tool(t, s, ToolLookup, map[string]any{"q": "acme", "scope": "documents"}); !r.IsError {
		t.Fatalf("an unknown scope is refused: %s", r.Structured)
	}
	if r := tool(t, s, ToolThread, map[string]any{"thread": open.ID, "summaries": "some"}); !r.IsError {
		t.Fatalf("an unknown summaries value is refused: %s", r.Structured)
	}
}
