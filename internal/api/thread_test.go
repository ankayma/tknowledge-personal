package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

func tenantToken(t *testing.T, env *testEnv, user, tenant string) string {
	t.Helper()
	return signGrant(t, env.Key, grant.Grant{UserID: user, ActorID: "claude", NodeID: "test", Aud: testAud, TenantID: tenant})
}

type storeErr struct {
	Error tka.Error `json:"error"`
}

type threadViewOut struct {
	ID            string               `json:"id"`
	Counts        string               `json:"counts"`
	Summary       *tka.Summary         `json:"summary"`
	Chain         []tka.ThreadClaim    `json:"chain"`
	Conversations []linkedConversation `json:"conversations"`
	Persons       []linkedPerson       `json:"persons"`
}

type lookupOut struct {
	Threads []tka.ThreadMatch `json:"threads"`
	Persons []struct {
		Slug string `json:"slug"`
	} `json:"persons"`
	Conversations []conversationSummary `json:"conversations"`
}

// The flow the user approved on 1/10/2026 (spec §6), over REST: a thread, a
// conversation filed into it by interact, a claim filed by admit, a person
// linked — and the thread read back with the conversation's last turn and the
// person's name, and lookup finding all three from one typed word.
func TestThread_InteractLinksConversationAndLookupFindsIt(t *testing.T) {
	env := newTestServer(t)
	tok := env.token(t, "bao", "claude-desktop")

	resp := postJSON(t, env.URL+"/v1/tka/threads", tok, map[string]any{"title": "Senior SA interview — Northwind", "keywords": []string{"A42", "mira"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open: %d %+v", resp.StatusCode, decode[storeErr](t, resp))
	}
	th := decode[threadViewOut](t, resp)
	if th.ID != "TH-1" {
		t.Fatalf("open: %+v", th)
	}

	out := doInteract(t, env, tok, interactReq{Conversation: conversationKey{Kind: "email", Person: "Mira Patel"}, Thread: "a42",
		Events: []eventReq{{Actor: "Mira Patel", Type: "email", Body: "We would like to make you an offer", At: "2026-10-01T09:00:00+04:00"}}})
	if out.Thread == nil || out.Thread["thread"] != "TH-1" || out.Thread["target"] != out.ConversationID || out.Thread["new"] != true {
		t.Fatalf("interact files the conversation: %+v", out)
	}

	resp = postJSON(t, env.URL+"/v1/tka/documents", tok, map[string]any{
		"text": "We would like to make you an offer", "text_producer": "owner", "method": "opened_source",
		"source_uri": "https://example.com/offer"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("record the offer email: %d %+v", resp.StatusCode, decode[storeErr](t, resp))
	}
	offer := decode[map[string]any](t, resp)
	resp = postJSON(t, env.URL+"/v1/tka/admit", tok, map[string]any{
		"claim": "Northwind sent Lan Nguyen an offer on 2026-10-01", "basis": "fact",
		"where": "the email", "source_doc": offer["id"], "thread": "mira-patel-is-not-a-keyword"})
	if e := decode[storeErr](t, resp); resp.StatusCode != http.StatusNotFound || e.Error.Code != "THREAD_UNKNOWN" || e.Error.Next == "" {
		t.Fatalf("a thread that does not resolve: %d %+v", resp.StatusCode, e)
	}
	resp = postJSON(t, env.URL+"/v1/tka/admit", tok, map[string]any{
		"claim": "Northwind sent Lan Nguyen an offer on 2026-10-01", "basis": "fact",
		"where": "the email", "source_doc": offer["id"], "thread": "TH-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admit: %d %+v", resp.StatusCode, decode[storeErr](t, resp))
	}
	admitted := decode[map[string]any](t, resp)
	if thread, _ := admitted["thread"].(map[string]any); thread["thread"] != "TH-1" {
		t.Fatalf("admit reports the thread: %+v", admitted)
	}
	resp = postJSON(t, env.URL+"/v1/tka/threads/link", tok, map[string]any{"thread": "a42", "ids": []string{"person:Mira Patel"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("link person: %d %+v", resp.StatusCode, decode[storeErr](t, resp))
	}

	code, v := getAs[threadViewOut](t, env.URL+"/v1/tka/threads/A-42", tok)
	if code != 200 || len(v.Chain) != 1 || v.Chain[0].ID != admitted["id"] {
		t.Fatalf("thread: %d %+v", code, v)
	}
	if len(v.Conversations) != 1 || v.Conversations[0].ConversationID != out.ConversationID || v.Conversations[0].LastTurn == nil ||
		v.Conversations[0].LastTurn.Actor != "mira-patel" || v.Conversations[0].MyTouches != 0 {
		t.Fatalf("the conversation with its last turn: %+v", v.Conversations)
	}
	if len(v.Persons) != 1 || v.Persons[0].Name != "Mira Patel" || v.Persons[0].ID != "person:mira-patel" {
		t.Fatalf("the person with a name: %+v", v.Persons)
	}

	code, l := getAs[lookupOut](t, env.URL+"/v1/tka/lookup?q=Mira", tok)
	if code != 200 || len(l.Threads) != 1 || l.Threads[0].ID != "TH-1" || l.Threads[0].Matched != "keyword:mira" ||
		len(l.Persons) != 1 || l.Persons[0].Slug != "mira-patel" || len(l.Conversations) != 1 || l.Conversations[0].ConversationID != out.ConversationID {
		t.Fatalf("lookup: %d %+v", code, l)
	}
	if _, typo := getAs[lookupOut](t, env.URL+"/v1/tka/lookup?q=Mira%20Patell", tok); len(typo.Persons) != 1 {
		t.Fatalf("a name typed half-right still finds the person: %+v", typo)
	}

	// A summary over REST: every sentence cites the thread's ids, the view shows
	// it fresh, and a new turn in its conversation makes it stale.
	resp = postJSON(t, env.URL+"/v1/tka/threads/summarize", tok, map[string]any{"thread": "a42", "author": "claude",
		"text": "Northwind made the offer by email (" + admitted["id"].(string) + "). Mira wrote it (" + out.ConversationID + ", person:mira-patel)."})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("summarize: %d %+v", resp.StatusCode, decode[storeErr](t, resp))
	}
	if s := decode[tka.Summary](t, resp); s.ID != "S-1" || s.Status != "current" || s.AsOf == nil {
		t.Fatalf("summary: %+v", s)
	}
	resp = postJSON(t, env.URL+"/v1/tka/threads/summarize", tok, map[string]any{"thread": "TH-1", "author": "claude", "text": "Waiting on the start date."})
	if e := decode[storeErr](t, resp); e.Error.Code != "SUMMARY_UNCITED" || e.Error.Next == "" {
		t.Fatalf("uncited summary: %d %+v", resp.StatusCode, e)
	}
	if _, v = getAs[threadViewOut](t, env.URL+"/v1/tka/threads/TH-1", tok); v.Summary == nil || v.Summary.ID != "S-1" || v.Summary.Stale || v.Counts == "" {
		t.Fatalf("thread with its summary: %+v", v.Summary)
	}
	doInteract(t, env, tok, interactReq{Conversation: conversationKey{Kind: "email", Person: "Mira Patel"},
		Events: []eventReq{{Actor: "me", Type: "email", Body: "Thank you, I accept", At: "2026-10-02T09:00:00+04:00"}}})
	if _, v = getAs[threadViewOut](t, env.URL+"/v1/tka/threads/TH-1", tok); v.Summary == nil || !v.Summary.Stale {
		t.Fatalf("a new turn in its conversation makes the summary stale: %+v", v.Summary)
	}

	// interact naming a thread that does not resolve writes nothing.
	before := count(t, env, "events")
	resp = postJSON(t, env.URL+"/v1/interact", tok, interactReq{Conversation: conversationKey{Kind: "dm", Person: "Dev Kapoor"}, Thread: "nope",
		Events: []eventReq{{Actor: "me", Type: "dm", Body: "hello"}}})
	if e := decode[storeErr](t, resp); resp.StatusCode != http.StatusNotFound || e.Error.Code != "THREAD_UNKNOWN" || count(t, env, "events") != before {
		t.Fatalf("interact into an unknown thread: %d %+v", resp.StatusCode, e)
	}
}

// People and conversations belong to a store since 1/10/2026: two stores may
// each know an "Mira Patel", and neither sees the other's.
func TestPeople_TenantIsolation(t *testing.T) {
	env := newTestServer(t)
	acme, globex := tenantToken(t, env, "bao", "acme"), tenantToken(t, env, "lan", "globex")
	mira := conversationKey{Kind: "email", Person: "Mira Patel"}
	mine := doInteract(t, env, acme, interactReq{Conversation: mira, Events: []eventReq{{Actor: "me", Type: "email", Body: "Hello from acme"}}})
	theirs := doInteract(t, env, globex, interactReq{Conversation: mira, Events: []eventReq{{Actor: "Mira Patel", Type: "email", Body: "Hello globex"}}})
	if mine.ConversationID == theirs.ConversationID || count(t, env, "persons") != 2 {
		t.Fatalf("one person per store: %s %s, persons %d", mine.ConversationID, theirs.ConversationID, count(t, env, "persons"))
	}

	if code, p := getAs[personOut](t, env.URL+"/v1/person?query=mira", acme); code != 200 || p.Person == nil ||
		len(p.Conversations) != 1 || p.Conversations[0].ConversationID != mine.ConversationID {
		t.Fatalf("acme sees its own: %d %+v", code, p)
	}
	if code, _ := getAs[apiErr](t, env.URL+"/v1/conversations/"+theirs.ConversationID, acme); code != http.StatusNotFound {
		t.Fatalf("acme cannot read globex's conversation: %d", code)
	}
	if _, l := getAs[lookupOut](t, env.URL+"/v1/tka/lookup?q=mira", globex); len(l.Conversations) != 1 || l.Conversations[0].ConversationID != theirs.ConversationID {
		t.Fatalf("globex's lookup: %+v", l)
	}

	resp := postJSON(t, env.URL+"/v1/tka/threads", acme, map[string]any{"title": "A42", "keywords": []string{"a42"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open: %d", resp.StatusCode)
	}
	resp = postJSON(t, env.URL+"/v1/tka/threads/link", acme, map[string]any{"thread": "a42", "ids": []string{theirs.ConversationID}})
	if e := decode[storeErr](t, resp); e.Error.Code != "LINK_TARGET_UNKNOWN" {
		t.Fatalf("acme cannot point at globex's conversation: %+v", e)
	}
	// The old subject ledger is the default store's: another store's person
	// page never reads claims from it.
	if !strings.HasPrefix(mine.ConversationID, "conv_") {
		t.Fatalf("ids: %s", mine.ConversationID)
	}
}
