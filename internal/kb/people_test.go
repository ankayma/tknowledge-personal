package kb

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// One turn inline, then their answer: the conversation counts my touches since
// they last spoke, and — since 1/10/2026 — nothing is written as a claim. When
// the last turn happened is read from the turns.
func TestInteract_RecordsTurnsWritesNoClaim(t *testing.T) {
	svc, tk, env, cc := setup(t)
	ctx := context.Background()

	out, err := svc.Interact(ctx, tk, InteractInput{
		Conversation: tkclient.ConversationKey{Kind: "dm", Person: "Marco Rossi"},
		EventInput:   EventInput{Actor: "me", Type: "dm", Body: "Hi Marco", Verbatim: true, At: "2026-09-13T08:00:00Z"},
	})
	if err != nil || !strings.HasPrefix(out.ConversationID, "conv_") || !strings.HasPrefix(out.EvtID, "evt_") || out.MyTouches != 1 {
		t.Fatalf("interact: %+v %v", out, err)
	}

	// Their answer, pasted as events, addressed by first name.
	them, err := svc.Interact(ctx, tk, InteractInput{
		Conversation: tkclient.ConversationKey{Kind: "dm", Person: "Marco"},
		Events:       []EventInput{{Actor: "Marco", Type: "dm", Body: "Hi Bao", At: "2026-09-13T09:00:00Z"}},
	})
	if err != nil || them.ConversationID != out.ConversationID || them.MyTouches != 0 {
		t.Fatalf("their reply: %+v %v", them, err)
	}
	var rows int
	if err := env.Admin.QueryRow(ctx, `SELECT count(*) FROM ledger`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || len(cc.Causes()) != 0 {
		t.Fatalf("no claim, no cause: ledger %d, causes %d", rows, len(cc.Causes()))
	}

	raw, err := svc.Conversation(ctx, tk, ConversationInput{Kind: "dm", Person: "marco rossi"})
	if err != nil || !strings.Contains(string(raw), `"my_touches_since_their_last":0`) {
		t.Fatalf("conversation: %s %v", raw, err)
	}
	_, err = svc.Conversation(ctx, tk, ConversationInput{ConversationID: "conv_999999"})
	wantCode(t, "missing conversation", err, "NOT_FOUND")
	_, err = svc.Interact(ctx, tk, InteractInput{Conversation: tkclient.ConversationKey{Kind: "dm"}, EventInput: EventInput{Actor: "me", Type: "dm", Body: "x"}})
	wantCode(t, "no person", err, "CONVERSATION_KEY_MISSING")
}

func TestPosted_PromotesPublished(t *testing.T) {
	svc, tk, env, cc := setup(t)
	ctx := context.Background()
	in := PostedInput{Title: "Prediction market record", Body: "Record so far", URL: "https://linkedin.com/posts/bao_1"}

	p, err := svc.Posted(ctx, tk, in)
	if err != nil || p.Error != nil || p.Subject != "post:prediction-market-record.published" || !strings.HasPrefix(p.DocID, "doc_") ||
		!strings.HasPrefix(p.ConversationID, "conv_") {
		t.Fatalf("posted: %+v %v", p, err)
	}
	var mark, note string
	if err := env.Admin.QueryRow(ctx, `SELECT mark, basis->>'note' FROM ledger WHERE id = $1`, mustID(t, p.ClaimID)).Scan(&mark, &note); err != nil {
		t.Fatal(err)
	}
	if mark != "T" || !strings.HasPrefix(note, p.DocID+" @") {
		t.Fatalf("published claim: %s %s", mark, note)
	}
	again, err := svc.Posted(ctx, tk, in)
	if err != nil || again.ClaimID != p.ClaimID || again.ConversationID != p.ConversationID || len(cc.Causes()) != 1 {
		t.Fatalf("posted twice: %+v %v causes=%d", again, err, len(cc.Causes()))
	}
}

func TestPerson_AmbiguityCarriesCandidates(t *testing.T) {
	svc, tk, _, _ := setup(t)
	ctx := context.Background()
	for _, name := range []string{"Paul Okafor", "Paul Ivanov"} {
		if _, err := svc.Interact(ctx, tk, InteractInput{Conversation: tkclient.ConversationKey{Kind: "dm", Person: name}, EventInput: EventInput{Actor: "me", Type: "dm", Body: "hello " + name}}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := svc.Conversation(ctx, tk, ConversationInput{Kind: "dm", Person: "Paul"})
	wantCode(t, "ambiguous conversation", err, "AMBIGUOUS_PERSON")
	var e *Error
	_ = asError(err, &e)
	var cands []map[string]any
	if json.Unmarshal(e.Candidates, &cands) != nil || len(cands) != 2 {
		t.Fatalf("candidates: %s", e.Candidates)
	}
	raw, err := svc.Person(ctx, tk, PersonInput{Query: "Paul"})
	if err != nil || !strings.Contains(string(raw), `"ambiguous":true`) {
		t.Fatalf("person: %s %v", raw, err)
	}
	_, err = svc.Interact(ctx, tk, InteractInput{Conversation: tkclient.ConversationKey{Kind: "dm", Person: "Paul"}, EventInput: EventInput{Actor: "me", Type: "dm", Body: "which one"}})
	wantCode(t, "ambiguous interact", err, "AMBIGUOUS_PERSON")
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	*target = e
	return ok
}
