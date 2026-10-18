package kb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// EventInput is one turn of an interaction.
type EventInput struct {
	Actor     string `json:"actor"`
	Type      string `json:"type"`
	Body      string `json:"body"`
	Verbatim  bool   `json:"verbatim"`
	At        string `json:"at"`
	InReplyTo string `json:"in_reply_to"`
	ChatID    string `json:"chat_id"`
}

// InteractInput takes one turn inline (actor, type, …) or a pasted
// conversation as events. in_reply_to may name evt_<n> or #<i>, the i-th event
// of this call. Thread, when given, links the conversation to that thread
// (TH-… or a keyword) in the same write.
type InteractInput struct {
	Conversation tkclient.ConversationKey `json:"conversation"`
	EventInput
	Events []EventInput `json:"events"`
	Thread string       `json:"thread"`
}

type InteractOutput struct {
	ConversationID string                 `json:"conversation_id"`
	EvtID          string                 `json:"evt_id"`
	MyTouches      int                    `json:"my_touches_since_their_last"`
	Events         []tkclient.EventResult `json:"events"`
	Thread         json.RawMessage        `json:"thread,omitempty"`
}

// Interact records turns in a conversation. When the last turn happened is
// read from the turns; nothing about it is written as a claim.
func (s *Service) Interact(ctx context.Context, tk *tkclient.Client, in InteractInput) (InteractOutput, error) {
	events := in.Events
	if len(events) == 0 {
		if strings.TrimSpace(in.Actor) == "" && strings.TrimSpace(in.Type) == "" {
			return InteractOutput{}, fail("BAD_REQUEST", "actor and type, or events, are required")
		}
		events = []EventInput{in.EventInput}
	}
	reqs := make([]tkclient.EventReq, 0, len(events))
	for _, e := range events {
		reqs = append(reqs, tkclient.EventReq(e))
	}
	res, err := tk.Interact(ctx, in.Conversation, reqs, in.Thread)
	if err != nil {
		return InteractOutput{}, upstream(err)
	}
	return InteractOutput{ConversationID: res.ConversationID, EvtID: res.EvtID, MyTouches: res.MyTouches,
		Events: res.Events, Thread: res.Thread}, nil
}

type PostedInput struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url"`
	PostedAt string `json:"posted_at"`
	ChatID   string `json:"chat_id"`
}

type PostedOutput struct {
	DocID          string `json:"doc_id"`
	ClaimID        string `json:"claim_id,omitempty"`
	Subject        string `json:"subject"`
	ConversationID string `json:"conversation_id"`
	Error          *Error `json:"error,omitempty"`
}

// Posted records one of my posts: document, conversation, and the T claim
// that it was published, promoted with a cause. If the promote
// fails the document and conversation stay, and the error says the claim is
// still A.
func (s *Service) Posted(ctx context.Context, tk *tkclient.Client, in PostedInput) (PostedOutput, error) {
	res, err := tk.Posted(ctx, tkclient.PostedReq(in))
	if err != nil {
		return PostedOutput{}, upstream(err)
	}
	c := s.finish(ctx, tk, res.Claim)
	out := PostedOutput{DocID: res.DocID, Subject: res.Subject, ConversationID: res.ConversationID, ClaimID: c.ID}
	if c.Error != nil {
		out.Error = c.Error
	}
	return out, nil
}

type ConversationInput struct {
	ConversationID string `json:"conversation_id"`
	URL            string `json:"url"`
	Kind           string `json:"kind"`
	Person         string `json:"person"`
}

// Conversation returns a conversation with every turn.
func (s *Service) Conversation(ctx context.Context, tk *tkclient.Client, in ConversationInput) (json.RawMessage, error) {
	raw, err := tk.Conversation(ctx, in.ConversationID, in.URL, in.Kind, in.Person)
	var apiErr *tkclient.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil, fail("NOT_FOUND", "no such conversation")
	}
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}

type PersonInput struct {
	Query string `json:"query"`
}

// Person returns everything known about one person, or the candidates when
// the name is ambiguous, or {found: false}.
func (s *Service) Person(ctx context.Context, tk *tkclient.Client, in PersonInput) (json.RawMessage, error) {
	raw, err := tk.Person(ctx, in.Query)
	if err != nil {
		return nil, upstream(err)
	}
	return raw, nil
}
