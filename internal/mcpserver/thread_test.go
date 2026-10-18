package mcpserver

import (
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/ccclient"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// The flow the user approved on 1/10/2026 (spec §6), the way a stranger runs
// it: a demo grant in a store of their own. Mira's email arrives; the
// assistant finds the matter by a word, files the turn, the evidence and the
// claim under it, refutes the assumption the email settled against, and reads
// back where the matter stands.
func TestFlow_ThreadAsAStranger(t *testing.T) {
	env := testapi.Start(t)
	cc := testapi.StartCC(t)
	tok, err := grant.Sign(env.Key, grant.Grant{UserID: "stranger@example.test", ActorID: "claude", NodeID: "test", Aud: testapi.Aud,
		TenantID: "demo-flow", Scope: []string{grant.ScopeRead, grant.ScopeWrite, grant.ScopeDemo}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(tkclient.New(env.Server.URL).WithToken(tok), ccclient.New(cc.URL))
	ok := func(name string, args map[string]any) map[string]any {
		t.Helper()
		r := tool(t, s, name, args)
		if r.IsError {
			t.Fatalf("%s: %s", name, r.Structured)
		}
		return into[map[string]any](t, r)
	}

	th := ok(ToolThreadOpen, map[string]any{"title": "Senior SA interview — Northwind", "keywords": []string{"A42", "Northwind", "mira"}})
	if th["id"] != "TH-1" {
		t.Fatalf("thread_open: %v", th)
	}
	waiting := ok(ToolAssume, map[string]any{"claim": "Northwind sends the stranger the round-3 result before 2026-09-30",
		"type": "A-p", "source": "owner", "if": "an email with the result", "not": "2026-09-30 passes with no email", "thread": "a42"})

	found := ok(ToolLookup, map[string]any{"q": "Northwind 42"})
	if threads, _ := found["threads"].([]any); len(threads) != 1 {
		t.Fatalf("lookup: %v", found)
	}
	turn := ok(ToolInteract, map[string]any{"conversation": map[string]any{"kind": "email", "person": "Mira Patel"},
		"actor": "Mira Patel", "type": "email", "body": "We would like to make you an offer", "at": "2026-10-01T09:00:00+04:00", "thread": "a42"})
	if !strings.HasPrefix(turn["conversation_id"].(string), "conv_") || turn["thread"] == nil {
		t.Fatalf("interact: %v", turn)
	}
	// The old shape — the file, base64 — is refused with the way forward.
	old := tool(t, s, ToolDocumentAdd, map[string]any{"content": "T2ZmZXIgZnJvbSBJbmNlcHRpb240Mg=="})
	if !old.IsError || !strings.Contains(string(old.Structured), "FILE_NOT_KEPT") {
		t.Fatalf("a file is not kept: %s", old.Structured)
	}
	doc := ok(ToolDocumentAdd, map[string]any{"text": "Offer from Northwind", "text_producer": "owner",
		"method": "opened_source", "source_uri": "file:///mail/offer.eml", "filename": "offer.eml", "thread": "TH-1"})
	if doc["bound"] != true {
		t.Fatalf("document_add: %v", doc)
	}
	claim := ok(ToolAdmit, map[string]any{"claim": "Northwind sent the stranger an offer on 2026-10-01", "basis": "fact",
		"where": "the email", "source_doc": doc["id"], "thread": "mira"})
	refuted := ok(ToolRefute, map[string]any{"id": waiting["id"], "observed": "the result came on 2026-10-01, after the date", "evidence": claim["id"]})
	if refuted["status"] != "refuted" {
		t.Fatalf("refute: %v", refuted)
	}

	view := ok(ToolThread, map[string]any{"thread": "TH-1"})
	chain, _ := view["chain"].([]any)
	convs, _ := view["conversations"].([]any)
	docs, _ := view["documents"].([]any)
	open, _ := view["open"].([]any)
	if len(chain) != 1 || len(convs) != 1 || len(docs) != 1 || len(open) != 0 ||
		!strings.Contains(view["counts"].(string), claim["id"].(string)) {
		t.Fatalf("thread: %v", view)
	}

	// A keyword two threads share is refused with the candidates.
	ok(ToolThreadOpen, map[string]any{"title": "Capital.com #068", "keywords": []string{"mira"}})
	amb := tool(t, s, ToolThread, map[string]any{"thread": "mira"})
	if !amb.IsError || !strings.Contains(string(amb.Structured), "THREAD_AMBIGUOUS") || !strings.Contains(string(amb.Structured), `"candidates"`) {
		t.Fatalf("ambiguous: %s", amb.Structured)
	}
	cycle := tool(t, s, ToolLink, map[string]any{"thread": "TH-1", "ids": []string{"TH-1"}})
	if !cycle.IsError || !strings.Contains(string(cycle.Structured), "THREAD_CYCLE") {
		t.Fatalf("cycle: %s", cycle.Structured)
	}

	// Evidence, as a stranger (spec T–D phase 1): a second document, then both
	// taken back — the claim stays, a flag opens, and a person closes it.
	shot := ok(ToolDocumentAdd, map[string]any{"text": "Screenshot: offer letter, Northwind, 2026-10-01",
		"text_producer": "claude", "method": "read_text"})
	if shot["bound"] != false {
		t.Fatalf("a text-only document reads unbound: %v", shot)
	}
	att := ok(ToolAttest, map[string]any{"truth": claim["id"], "document": shot["id"], "where": "letter heading"})
	if att["attesters"] != float64(1) || att["strength"] != "opened_source" {
		t.Fatalf("attest: %v", att)
	}
	ok(ToolWithdrawEvidence, map[string]any{"truth": claim["id"], "document": doc["id"], "reason": "wrong mailbox"})
	last := ok(ToolWithdrawEvidence, map[string]any{"truth": claim["id"], "document": shot["id"], "reason": "not the letter"})
	flag, _ := last["flag"].(map[string]any)
	if flag == nil || flag["kind"] != "evidence_withdrawn" {
		t.Fatalf("withdraw the last: %v", last)
	}
	flags := ok(ToolFlags, map[string]any{})
	if flags["count"] != float64(1) {
		t.Fatalf("flags: %v", flags)
	}
	closed := ok(ToolCloseFlag, map[string]any{"flag": flag["id"], "note": "will re-admit on the signed letter"})
	if closed["status"] != "closed" {
		t.Fatalf("close_flag: %v", closed)
	}
	entry := ok(ToolEntry, map[string]any{"id": claim["id"]})
	if truth, _ := entry["truth"].(map[string]any); truth["status"] != "T" || truth["strength"] != "none" {
		t.Fatalf("the claim stands; nothing attests it: %v", entry)
	}
}
