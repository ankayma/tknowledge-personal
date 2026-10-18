package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type interactOut struct {
	ConversationID string         `json:"conversation_id"`
	EvtID          string         `json:"evt_id"`
	Events         []eventResult  `json:"events"`
	Claims         []admitResult  `json:"claims"`
	MyTouches      int            `json:"my_touches_since_their_last"`
	Thread         map[string]any `json:"thread"`
}

func doInteract(t *testing.T, env *testEnv, token string, req interactReq) interactOut {
	t.Helper()
	resp := postJSON(t, env.URL+"/v1/interact", token, req)
	if resp.StatusCode != http.StatusOK {
		body := decode[apiErr](t, resp)
		t.Fatalf("interact: %d %s %s", resp.StatusCode, body.Error, body.Detail)
	}
	return decode[interactOut](t, resp)
}

func count(t *testing.T, env *testEnv, table string) int {
	t.Helper()
	var n int
	if err := env.Admin.QueryRow(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type conversationOut struct {
	Conversation conversationView `json:"conversation"`
	Events       []eventView      `json:"events"`
	MyTouches    int              `json:"my_touches_since_their_last"`
}

// spec §5.9–5.10: a pasted LinkedIn comment thread — events in time order, URL
// key normalised, replies linked, touches counted, and pasting it again changes
// nothing. Since 1/10/2026 the last touch and last response are read from the
// turns: interact writes no claim at all.
func TestInteract_PastedConversation(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	req := interactReq{
		Conversation: conversationKey{Kind: "post_thread", URL: "https://www.LinkedIn.com/posts/omar_x/?utm_source=share#comments", Owner: "Omar Haddad"},
		Events: []eventReq{
			{Actor: "me", Type: "comment", Body: "Great point on liquidity", Verbatim: true, At: "2026-09-13T08:12:00+04:00"},
			{Actor: "Layla", Type: "reply", Body: "Agree with Bao", Verbatim: true, At: "2026-09-13T08:20:00+04:00", InReplyTo: "#0"},
			{Actor: "ME", Type: "react", At: "2026-09-13T08:31:00+04:00"},
			{Actor: "me", Type: "reply", Body: "Thanks Layla", At: "2026-09-13T08:30:00+04:00", InReplyTo: "#1"},
		},
	}
	out := doInteract(t, env, agent, req)
	if !strings.HasPrefix(out.ConversationID, "conv_") || len(out.Events) != 4 || out.MyTouches != 2 {
		t.Fatalf("interact: %+v", out)
	}
	order := []string{}
	for _, e := range out.Events {
		if e.Outcome != "new" {
			t.Fatalf("first paste: %+v", e)
		}
		order = append(order, e.Actor+"/"+e.Type)
	}
	if strings.Join(order, " ") != "me/comment layla/reply me/reply me/react" || out.EvtID != out.Events[3].EvtID {
		t.Fatalf("events are in time order, evt_id is the last: %v %s", order, out.EvtID)
	}
	if len(out.Claims) != 0 || count(t, env, "ledger") != 0 {
		t.Fatalf("interact writes no claim: %+v, ledger %d", out.Claims, count(t, env, "ledger"))
	}
	if count(t, env, "persons") != 2 || count(t, env, "conversations") != 1 || count(t, env, "events") != 4 {
		t.Fatal("persons omar-haddad + layla, one conversation, four events")
	}

	again := doInteract(t, env, agent, req)
	for _, e := range again.Events {
		if e.Outcome != "duplicate" {
			t.Fatalf("second paste: %+v", e)
		}
	}
	if len(again.Claims) != 0 || again.MyTouches != 2 || again.ConversationID != out.ConversationID || count(t, env, "events") != 4 {
		t.Fatalf("second paste changed something: %+v", again)
	}

	_, th := getJSON[conversationOut](t, env, env.URL+"/v1/conversation?url=https://www.linkedin.com/posts/omar_x/")
	if th.Conversation.ConversationID != out.ConversationID || th.Conversation.URL == nil || *th.Conversation.URL != "https://www.linkedin.com/posts/omar_x" ||
		strings.Join(th.Conversation.Participants, ",") != "omar-haddad,me,layla" || len(th.Events) != 4 || th.MyTouches != 2 {
		t.Fatalf("conversation by url: %+v", th)
	}
	if th.Events[1].InReplyTo == nil || *th.Events[1].InReplyTo != th.Events[0].EvtID ||
		th.Events[2].InReplyTo == nil || *th.Events[2].InReplyTo != th.Events[1].EvtID || th.Events[3].Body != nil {
		t.Fatalf("reply links / react without body: %+v", th.Events)
	}
	if !th.Conversation.OpenedAt.Equal(th.Events[0].At) || th.Conversation.LastEventAt == nil || !th.Conversation.LastEventAt.Equal(th.Events[3].At) {
		t.Fatalf("opened / last event: %+v", th.Conversation)
	}
	if code, byID := getJSON[conversationOut](t, env, env.URL+"/v1/conversations/"+out.ConversationID); code != 200 || len(byID.Events) != 4 {
		t.Fatalf("conversation by id: %d", code)
	}
	// The id it had before the rename still finds it.
	legacy := "thr_" + strings.TrimPrefix(out.ConversationID, "conv_")
	if code, byID := getJSON[conversationOut](t, env, env.URL+"/v1/conversations/"+legacy); code != 200 || len(byID.Events) != 4 {
		t.Fatalf("conversation by legacy id: %d", code)
	}

	// A later reply to an event by id.
	reply := doInteract(t, env, agent, interactReq{Conversation: req.Conversation, Events: []eventReq{
		{Actor: "Omar Haddad", Type: "reply", Body: "Thanks both", At: "2026-09-13T09:00:00+04:00", InReplyTo: out.Events[0].EvtID},
	}})
	if reply.MyTouches != 0 || len(reply.Claims) != 0 || count(t, env, "ledger") != 0 {
		t.Fatalf("their reply resets touches and writes no claim: %+v", reply)
	}
}

func TestInteract_RefusalsWriteNothing(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	post := conversationKey{Kind: "post_thread", URL: "https://linkedin.com/posts/x"}
	one := func(e eventReq) []eventReq { return []eventReq{e} }
	for _, c := range []struct {
		req  interactReq
		code string
	}{
		{interactReq{Conversation: conversationKey{Kind: "dm"}, Events: one(eventReq{Actor: "me", Type: "dm", Body: "hi"})}, "conversation_key_missing"},
		{interactReq{Conversation: conversationKey{Kind: "post_thread"}, Events: one(eventReq{Actor: "me", Type: "comment", Body: "hi"})}, "conversation_key_missing"},
		{interactReq{Conversation: conversationKey{Kind: "chat", Person: "x"}, Events: one(eventReq{Actor: "me", Type: "dm", Body: "hi"})}, "invalid_conversation_kind"},
		{interactReq{Conversation: conversationKey{Kind: "post_thread", URL: "linkedin.com/posts/x"}, Events: one(eventReq{Actor: "me", Type: "comment", Body: "hi"})}, "invalid_url"},
		{interactReq{Conversation: post, Events: one(eventReq{Actor: "me", Type: "react", Body: "👍"})}, "body_not_allowed"},
		{interactReq{Conversation: post, Events: one(eventReq{Actor: "me", Type: "comment", Body: " "})}, "empty_body"},
		{interactReq{Conversation: post, Events: one(eventReq{Actor: "me", Type: "like"})}, "invalid_event_type"},
		{interactReq{Conversation: post, Events: one(eventReq{Actor: "me", Type: "comment", Body: "x", InReplyTo: "#3"})}, "bad_in_reply_to"},
		{interactReq{Conversation: post, Events: one(eventReq{Actor: "me", Type: "comment", Body: "x", InReplyTo: "evt_999999"})}, "bad_in_reply_to"},
		{interactReq{Conversation: post, Events: one(eventReq{Actor: "me", Type: "comment", Body: "x", At: "yesterday"})}, "bad_at"},
		{interactReq{Conversation: post, Events: one(eventReq{Type: "comment", Body: "x"})}, "missing_actor"},
		{interactReq{Conversation: conversationKey{Kind: "dm", Person: "me"}, Events: one(eventReq{Actor: "me", Type: "dm", Body: "hi"})}, "invalid_person"},
		// A valid first event does not survive a bad second one.
		{interactReq{Conversation: conversationKey{Kind: "dm", Person: "Marco"}, Events: []eventReq{
			{Actor: "Marco", Type: "dm", Body: "hi", At: "2026-09-13T08:00:00Z"},
			{Actor: "me", Type: "dm", Body: "yo", At: "2026-09-13T09:00:00Z", InReplyTo: "#7"},
		}}, "bad_in_reply_to"},
	} {
		resp := postJSON(t, env.URL+"/v1/interact", agent, c.req)
		if body := decode[apiErr](t, resp); resp.StatusCode/100 != 4 || body.Error != c.code {
			t.Errorf("%+v: want %s, got %d %s", c.req, c.code, resp.StatusCode, body.Error)
		}
	}
	if count(t, env, "persons")+count(t, env, "conversations")+count(t, env, "events")+count(t, env, "ledger") != 0 {
		t.Fatal("refused interactions wrote rows")
	}
}

type personOut struct {
	Found      *bool       `json:"found"`
	Ambiguous  bool        `json:"ambiguous"`
	Candidates []candidate `json:"candidates"`
	Person     *struct {
		Slug    string   `json:"slug"`
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
		Company *string  `json:"company"`
		Tier    *string  `json:"tier"`
		Role    *string  `json:"role"`
	} `json:"person"`
	Claims        []personClaim         `json:"claims"`
	Conversations []conversationSummary `json:"conversations"`
	OpenItems     []personClaim         `json:"open_items"`
	Docs          []struct {
		DocID string `json:"doc_id"`
		Title string `json:"title"`
	} `json:"docs"`
}

// spec §4.6, §5.12, §6: resolution by name / alias / initials, ambiguity with
// candidates, the profile as a view of claims, and lookups never create.
func TestPerson_ResolutionProfileAndAmbiguity(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")

	doInteract(t, env, agent, interactReq{Conversation: conversationKey{Kind: "dm", Person: "Paul Okafor"}, Events: []eventReq{
		{Actor: "Paul Okafor", Type: "dm", Body: "Hi Bao, liquidity question", At: "2026-09-12T10:00:00Z"},
		{Actor: "me", Type: "dm", Body: "Hi Paul", At: "2026-09-12T11:00:00Z"},
	}})
	doInteract(t, env, agent, interactReq{Conversation: conversationKey{Kind: "dm", Person: "Paul Ivanov"}, Events: []eventReq{
		{Actor: "me", Type: "dm", Body: "Hello", At: "2026-09-12T12:00:00Z"},
	}})
	if _, amb := getJSON[personOut](t, env, env.URL+"/v1/person?query=Paul"); !amb.Ambiguous || len(amb.Candidates) != 2 {
		t.Fatalf("Paul is two people: %+v", amb)
	}

	r := admit(t, env, agent,
		admitItem{Subject: "person:paul-okafor.company", Statement: "Acme Markets", Label: "A"},
		admitItem{Subject: "person:Paul.tier", Statement: "thesis-ally", Label: "A"},
		admitItem{Subject: "person:po.tier", Statement: "thesis-ally", Label: "A"},
		admitItem{Subject: "person:paul-okafor.alias", Statement: "Leo", Label: "A"},
		admitItem{Subject: "person:Okafor.reply-sent", Statement: "Reply Paul gửi chưa", Label: "A-b"},
	)
	if r[1].Error == nil || r[1].Error.Code != "AMBIGUOUS_PERSON" || len(r[1].Error.Candidates) != 2 {
		t.Fatalf("ambiguous entity: %+v", r[1])
	}
	companies := []string{r[1].Error.Candidates[0].Company, r[1].Error.Candidates[1].Company}
	if !slices.Contains(companies, "Acme Markets") {
		t.Fatalf("candidates carry company: %+v", r[1].Error.Candidates)
	}
	if r[2].Subject != "person:paul-okafor.tier" || r[4].Subject != "person:paul-okafor.reply-sent" {
		t.Fatalf("initials / last name resolve to the slug: %s %s", r[2].Subject, r[4].Subject)
	}
	store(t, env, agent, storeReq{Title: "partner-assessment-paul", Kind: "assessment", Body: "strong", Refs: []string{"Paul Okafor"}})

	_, p := getJSON[personOut](t, env, env.URL+"/v1/person?query=leo")
	if p.Person == nil || p.Person.Slug != "paul-okafor" || p.Person.Company == nil || *p.Person.Company != "Acme Markets" ||
		p.Person.Tier == nil || *p.Person.Tier != "thesis-ally" || p.Person.Role != nil {
		t.Fatalf("profile: %+v", p.Person)
	}
	for _, a := range []string{"paul", "okafor", "po", "leo"} {
		if !slices.Contains(p.Person.Aliases, a) {
			t.Fatalf("aliases %v lack %s", p.Person.Aliases, a)
		}
	}
	if len(p.Conversations) != 1 || p.Conversations[0].Kind != "dm" || p.Conversations[0].LastTurn == nil ||
		p.Conversations[0].LastTurn.Actor != "me" || p.Conversations[0].MyTouches != 1 {
		t.Fatalf("conversations: %+v", p.Conversations)
	}
	if len(p.OpenItems) != 1 || p.OpenItems[0].Subject != "person:paul-okafor.reply-sent" || len(p.Docs) != 1 || len(p.Claims) < 4 {
		t.Fatalf("open items / docs / claims: %+v %+v %d", p.OpenItems, p.Docs, len(p.Claims))
	}

	personsBefore := count(t, env, "persons")
	if _, none := getJSON[personOut](t, env, env.URL+"/v1/person?query=Nobody+Here"); none.Found == nil || *none.Found {
		t.Fatalf("unknown: %+v", none)
	}
	if code, e := getJSON[apiErr](t, env, env.URL+"/v1/find?person=Paul"); code != 409 || e.Error != "ambiguous_person" {
		t.Fatalf("find by ambiguous person: %d %s", code, e.Error)
	}
	if _, f := getJSON[findOut](t, env, env.URL+"/v1/find?person=nobody"); f.Count != 0 || count(t, env, "persons") != personsBefore {
		t.Fatalf("lookups never create: %+v", f)
	}
	type findBoth struct {
		Docs          []docMeta             `json:"docs"`
		Conversations []conversationSummary `json:"conversations"`
	}
	if _, f := getJSON[findBoth](t, env, env.URL+"/v1/find?person=Okafor"); len(f.Docs) != 1 || len(f.Conversations) != 1 {
		t.Fatalf("find by person: %+v", f)
	}
	if _, f := getJSON[findBoth](t, env, env.URL+"/v1/find?kind=conversation&query=LIQUIDITY"); len(f.Docs) != 0 || len(f.Conversations) != 1 {
		t.Fatalf("find conversations by event body: %+v", f)
	}
	if r := admit(t, env, agent, admitItem{Subject: "person:new-person.tier", Statement: "cold", Label: "A"})[0]; r.Outcome != "new" || count(t, env, "persons") != personsBefore+1 {
		t.Fatalf("admit creates the person: %+v", r)
	}
}

// spec §5.11: posted = document kind post + conversation owned by me + claim T.
func TestPosted_DocConversationClaim(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	owner := env.token(t, "bao", "bao")
	type postedOut struct {
		DocID          string      `json:"doc_id"`
		DocOutcome     string      `json:"doc_outcome"`
		ConversationID string      `json:"conversation_id"`
		Subject        string      `json:"subject"`
		Claim          admitResult `json:"claim"`
	}
	posted := func(req postedReq) (int, postedOut, apiErr) {
		t.Helper()
		resp := postJSON(t, env.URL+"/v1/posted", agent, req)
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, postedOut{}, decode[apiErr](t, resp)
		}
		return resp.StatusCode, decode[postedOut](t, resp), apiErr{}
	}
	req := postedReq{Title: "Prediction Market Record", Body: "My record so far…", URL: "https://linkedin.com/posts/bao_pm-record?x=1", PostedAt: "2026-09-13T09:00:00+04:00", ChatID: "c7"}

	_, p, _ := posted(req)
	if p.DocOutcome != "new" || p.Subject != "post:prediction-market-record.published" || !p.Claim.NeedsPromote ||
		p.Claim.Basis == nil || !strings.HasPrefix(p.Claim.Basis.Note, p.DocID+" @") || !strings.HasPrefix(p.ConversationID, "conv_") {
		t.Fatalf("posted: %+v", p)
	}
	_, th := getJSON[conversationOut](t, env, env.URL+"/v1/conversations/"+p.ConversationID)
	if th.Conversation.Kind != "post_thread" || th.Conversation.Owner == nil || *th.Conversation.Owner != "me" {
		t.Fatalf("conversation: %+v", th.Conversation)
	}
	if code, e := moveAs(t, env, owner, p.Claim.ClaimID, "promote", p.Claim.Basis); code != 200 {
		t.Fatalf("promote: %d %s", code, e)
	}

	_, again, _ := posted(req)
	if again.DocOutcome != "duplicate" || again.DocID != p.DocID || again.ConversationID != p.ConversationID || again.Claim.Outcome != "duplicate" {
		t.Fatalf("posted twice: %+v", again)
	}

	c := doInteract(t, env, agent, interactReq{Conversation: conversationKey{Kind: "post_thread", URL: "https://LINKEDIN.com/posts/bao_pm-record/"},
		Events: []eventReq{{Actor: "Anna Berg", Type: "comment", Body: "Impressive", At: "2026-09-13T10:00:00+04:00"}}})
	if c.ConversationID != p.ConversationID {
		t.Fatalf("a comment on my post lands in its conversation: %s vs %s", c.ConversationID, p.ConversationID)
	}
	if _, f := getJSON[findOut](t, env, env.URL+"/v1/find?kind=post"); f.Count != 1 || f.Docs[0].DocID != p.DocID {
		t.Fatalf("find posts: %+v", f)
	}
	if code, _, e := posted(postedReq{Title: "x", Body: "y"}); code != 400 || e.Error != "conversation_key_missing" {
		t.Fatalf("no url: %d %s", code, e.Error)
	}
	if code, _, e := posted(postedReq{Title: "x", URL: "https://x.com/p"}); code != 400 || e.Error != "empty_body" {
		t.Fatalf("no body: %d %s", code, e.Error)
	}
}

func TestBasis_EventMustExist(t *testing.T) {
	env := newTestServer(t)
	agent := env.token(t, "bao", "claude-desktop")
	if r := admit(t, env, agent, admitItem{Subject: "tracker:d4.reply-sent", Statement: "Reply đã gửi", Label: "T", Basis: "evt_999999"})[0]; r.Error == nil || r.Error.Code != "BASIS_NOT_FOUND" {
		t.Fatalf("missing event: %+v", r)
	}
	out := doInteract(t, env, agent, interactReq{Conversation: conversationKey{Kind: "email", Person: "Helena"}, Events: []eventReq{{Actor: "me", Type: "email", Body: "Reply"}}})
	if r := admit(t, env, agent, admitItem{Subject: "tracker:d4.reply-sent", Statement: "Reply đã gửi", Label: "T", Basis: out.EvtID})[0]; r.Outcome != "new" {
		t.Fatalf("stored event: %+v", r)
	}
}

func TestPeopleTables_AppendOnly(t *testing.T) {
	env := newTestServer(t)
	doInteract(t, env, env.token(t, "bao", "bao"), interactReq{Conversation: conversationKey{Kind: "dm", Person: "Marco"}, Events: []eventReq{{Actor: "Marco", Type: "dm", Body: "hi"}}})
	for _, sql := range []string{`UPDATE persons SET name = 'x'`, `UPDATE conversations SET owner_of_conversation = 'x'`, `UPDATE events SET body = 'x'`, `DELETE FROM events`} {
		_, err := env.Admin.Exec(context.Background(), sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: want insufficient_privilege, got %v", sql, err)
		}
	}
}
