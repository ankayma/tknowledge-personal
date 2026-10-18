package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/conversations"
	"github.com/ankayma/tknowledge-personal/internal/docs"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/people"
	"github.com/ankayma/tknowledge-personal/internal/subject"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// SPEC §16 — people, conversations, events: the MCP spec's interact /
// conversation / posted / person. A conversation is one exchange with one
// person on one channel, or the comments under one post; it was called a
// thread until 1/10/2026, when that name went to the meta-knowledge thread
// (tka.ThreadView). People and conversations belong to one store: every read
// and write here runs in a transaction that carries the grant's tenant.
//
// interact no longer writes "last touch" / "last response" claims: when the
// last turn happened is read from the turns themselves, not stored as a T.

// requestError is a REST refusal raised inside a transaction.
type requestError struct {
	status       int
	code, detail string
	candidates   []candidate
}

func (e *requestError) Error() string { return e.code + ": " + e.detail }

func badRequest(code, detail string) *requestError {
	return &requestError{status: http.StatusBadRequest, code: code, detail: detail}
}

func writeRequestError(w http.ResponseWriter, err error) bool {
	var re *requestError
	if !errors.As(err, &re) {
		return false
	}
	body := map[string]any{"error": re.code, "detail": re.detail}
	if re.candidates != nil {
		body["candidates"] = re.candidates
	}
	writeJSON(w, re.status, body)
	return true
}

// writeAny renders whatever a handler's transaction returned: a REST refusal,
// a store refusal (a thread that did not resolve), or an internal error.
func writeAny(w http.ResponseWriter, err error) {
	if writeRequestError(w, err) {
		return
	}
	var e *tka.Error
	if errors.As(err, &e) {
		writeStore(w, err)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

// tenantTx opens a transaction that carries the caller's store, so row-level
// security applies to every statement in it. Without it a read of people or
// conversations comes back empty and every insert is refused.
func (s *Server) tenantTx(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.Repo.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := tka.SetTenant(ctx, tx, grantFrom(ctx).TenantID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// inTenant runs a read in the caller's store.
func (s *Server) inTenant(ctx context.Context, fn func(q pgx.Tx) error) error {
	tx, err := s.tenantTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func ambiguous(ctx context.Context, s *Server, q people.Querier, name string, matches []people.Person) error {
	cands, err := s.candidates(ctx, q, matches)
	if err != nil {
		return err
	}
	return &requestError{status: http.StatusConflict, code: "ambiguous_person",
		detail: fmt.Sprintf("%q matches %d people", name, len(matches)), candidates: cands}
}

type eventView struct {
	EvtID          string    `json:"evt_id"`
	ConversationID string    `json:"conversation_id"`
	Actor          string    `json:"actor"`
	Type           string    `json:"type"`
	Body           *string   `json:"body"`
	Verbatim       bool      `json:"verbatim"`
	At             time.Time `json:"at"`
	InReplyTo      *string   `json:"in_reply_to"`
	ChatID         *string   `json:"chat_id"`
}

func toEventView(e conversations.Event) eventView {
	v := eventView{
		EvtID: subject.FormatEventID(e.ID), ConversationID: subject.FormatConversationID(e.ConversationID), Actor: e.Actor,
		Type: e.Type, Body: e.Body, Verbatim: e.Verbatim, At: e.At, ChatID: e.ChatID,
	}
	if e.InReplyTo != nil {
		id := subject.FormatEventID(*e.InReplyTo)
		v.InReplyTo = &id
	}
	return v
}

type conversationView struct {
	ConversationID string     `json:"conversation_id"`
	Kind           string     `json:"kind"`
	URL            *string    `json:"url"`
	Person         *string    `json:"person"`
	Owner          *string    `json:"owner"`
	Participants   []string   `json:"participants"`
	OpenedAt       time.Time  `json:"opened_at"`
	LastEventAt    *time.Time `json:"last_event_at"`
}

func toConversationView(c conversations.Conversation, evs []conversations.Event) conversationView {
	v := conversationView{
		ConversationID: subject.FormatConversationID(c.ID), Kind: c.Kind, URL: c.URL, Person: c.PersonSlug, Owner: c.Owner,
		Participants: conversations.Participants(c, evs), OpenedAt: c.CreatedAt,
	}
	if v.Participants == nil {
		v.Participants = []string{}
	}
	if len(evs) > 0 {
		if evs[0].At.Before(v.OpenedAt) {
			v.OpenedAt = evs[0].At
		}
		last := evs[len(evs)-1].At
		v.LastEventAt = &last
	}
	return v
}

type lastEvent struct {
	Actor string    `json:"actor"`
	Type  string    `json:"type"`
	At    time.Time `json:"at"`
}

// conversationSummary is a conversation in one line: who it is with, its last
// turn, and how many times I have written since they last did.
type conversationSummary struct {
	ConversationID string     `json:"conversation_id"`
	Kind           string     `json:"kind"`
	URL            *string    `json:"url,omitempty"`
	Person         *string    `json:"person,omitempty"`
	LastTurn       *lastEvent `json:"last_turn"`
	MyTouches      int        `json:"my_touches_since_their_last"`
}

func conversationSummaryOf(ctx context.Context, q people.Querier, c conversations.Conversation) (conversationSummary, error) {
	evs, err := conversations.Events(ctx, q, c.ID)
	if err != nil {
		return conversationSummary{}, err
	}
	sum := conversationSummary{ConversationID: subject.FormatConversationID(c.ID), Kind: c.Kind, URL: c.URL,
		Person: c.PersonSlug, MyTouches: conversations.MyTouches(evs)}
	if len(evs) > 0 {
		e := evs[len(evs)-1]
		sum.LastTurn = &lastEvent{Actor: e.Actor, Type: e.Type, At: e.At}
	}
	return sum, nil
}

type conversationKey struct {
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Person string `json:"person"`
	Owner  string `json:"owner"`
}

type eventReq struct {
	Actor     string `json:"actor"`
	Type      string `json:"type"`
	Body      string `json:"body"`
	Verbatim  bool   `json:"verbatim"`
	At        string `json:"at"`
	InReplyTo string `json:"in_reply_to"`
	ChatID    string `json:"chat_id"`
}

type interactReq struct {
	Conversation conversationKey `json:"conversation"`
	Events       []eventReq      `json:"events"`
	// Thread links the conversation to a thread (TH-… or a keyword) in the
	// same transaction.
	Thread string `json:"thread"`
}

type eventResult struct {
	EvtID   string    `json:"evt_id"`
	Outcome string    `json:"outcome"` // new | duplicate
	Actor   string    `json:"actor"`
	Type    string    `json:"type"`
	At      time.Time `json:"at"`
}

type parsedEvent struct {
	idx  int
	req  eventReq
	at   time.Time
	body *string
}

func parseEvents(in []eventReq, now time.Time) ([]parsedEvent, error) {
	out := make([]parsedEvent, 0, len(in))
	for i, e := range in {
		where := fmt.Sprintf("events[%d]", i)
		if strings.TrimSpace(e.Actor) == "" {
			return nil, badRequest("missing_actor", where+": actor is required (me or a name)")
		}
		if !slices.Contains(conversations.Types, e.Type) {
			return nil, badRequest("invalid_event_type", where+": type must be one of "+strings.Join(conversations.Types, ", "))
		}
		noBody := slices.Contains(conversations.NoBody, e.Type)
		var body *string
		switch {
		case noBody && strings.TrimSpace(e.Body) != "":
			return nil, badRequest("body_not_allowed", where+": "+e.Type+" carries no body")
		case !noBody && strings.TrimSpace(e.Body) == "":
			return nil, badRequest("empty_body", where+": "+e.Type+" needs a body")
		case !noBody:
			b := e.Body
			body = &b
		}
		at := now
		if v := strings.TrimSpace(e.At); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, badRequest("bad_at", where+": at must be RFC 3339")
			}
			at = t
		}
		out = append(out, parsedEvent{idx: i, req: e, at: at, body: body})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].at.Before(out[b].at) })
	return out, nil
}

// POST /interact — {conversation, events: [...], thread?} →
// {conversation_id, evt_id, events, my_touches_since_their_last, thread?}.
func (s *Server) postInteract(w http.ResponseWriter, r *http.Request) {
	var req interactReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Events) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "conversation: {kind, url | person, owner?}, events: [{actor, type, body?, verbatim?, at?, in_reply_to?, chat_id?}] required")
		return
	}
	out, err := s.interact(r.Context(), grantFrom(r.Context()), req, time.Now())
	if err != nil {
		writeAny(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) interact(ctx context.Context, g grant.Grant, req interactReq, now time.Time) (map[string]any, error) {
	kind := strings.TrimSpace(req.Conversation.Kind)
	if !slices.Contains(conversations.Kinds, kind) {
		return nil, badRequest("invalid_conversation_kind", "conversation.kind must be post_thread, dm, email or connect")
	}
	var key string
	if kind == conversations.PostThread {
		if strings.TrimSpace(req.Conversation.URL) == "" {
			return nil, badRequest("conversation_key_missing", "a post_thread needs url")
		}
		u, err := conversations.NormalizeURL(req.Conversation.URL)
		if err != nil {
			return nil, badRequest("invalid_url", err.Error())
		}
		key = u
	} else if strings.TrimSpace(req.Conversation.Person) == "" {
		return nil, badRequest("conversation_key_missing", "a "+kind+" conversation needs person")
	}
	evs, err := parseEvents(req.Events, now)
	if err != nil {
		return nil, err
	}
	st := stampOf(g)

	tx, err := s.tenantTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// A turn in a conversation is a write like any other: a closed store refuses
	// it and a trial store counts it, in this transaction (owner 3/10).
	if err := s.tka().Writable(ctx, tx, actorOfGrant(g)); err != nil {
		return nil, err
	}

	resolve := func(name string) (string, error) {
		if strings.EqualFold(strings.TrimSpace(name), people.Me) {
			return people.Me, nil
		}
		p, matches, err := people.Ensure(ctx, tx, name, st)
		if errors.Is(err, people.ErrName) {
			return "", badRequest("invalid_person", fmt.Sprintf("%q: %v", name, err))
		}
		if err != nil {
			return "", err
		}
		if matches != nil {
			return "", ambiguous(ctx, s, tx, name, matches)
		}
		return p.Slug, nil
	}

	var owner *string
	if kind == conversations.PostThread {
		if o := strings.TrimSpace(req.Conversation.Owner); o != "" {
			slug, err := resolve(o)
			if err != nil {
				return nil, err
			}
			owner = &slug
		}
	} else {
		slug, err := resolve(req.Conversation.Person)
		if err != nil {
			return nil, err
		}
		if slug == people.Me {
			return nil, badRequest("invalid_person", "a "+kind+" conversation is with someone else")
		}
		key, owner = slug, &slug
	}
	conv, err := conversations.Ensure(ctx, tx, kind, key, owner, st)
	if err != nil {
		return nil, err
	}

	byIdx := map[int]conversations.Event{}
	results := make([]eventResult, 0, len(evs))
	for _, pe := range evs {
		actor, err := resolve(pe.req.Actor)
		if err != nil {
			return nil, err
		}
		var inReply *int64
		if ref := strings.TrimSpace(pe.req.InReplyTo); ref != "" {
			if k, ok := strings.CutPrefix(ref, "#"); ok {
				n, convErr := strconv.Atoi(k)
				parent, done := byIdx[n]
				if convErr != nil || !done {
					return nil, badRequest("bad_in_reply_to", "in_reply_to "+ref+" names no earlier event of this batch")
				}
				inReply = &parent.ID
			} else {
				id, ok := subject.ParseEventID(ref)
				var parent conversations.Event
				found := false
				if ok {
					if parent, found, err = conversations.EventByID(ctx, tx, id); err != nil {
						return nil, err
					}
				}
				if !found || parent.ConversationID != conv.ID {
					return nil, badRequest("bad_in_reply_to", "in_reply_to "+ref+" is not an event of this conversation")
				}
				inReply = &parent.ID
			}
		}
		e, dup, err := conversations.InsertEvent(ctx, tx, conversations.Event{
			ConversationID: conv.ID, Actor: actor, Type: pe.req.Type, Body: pe.body, Verbatim: pe.req.Verbatim,
			At: pe.at, InReplyTo: inReply, ChatID: nullable(pe.req.ChatID),
		}, st)
		if err != nil {
			return nil, err
		}
		byIdx[pe.idx] = e
		outcome := "duplicate"
		if !dup {
			outcome = "new"
		}
		results = append(results, eventResult{EvtID: subject.FormatEventID(e.ID), Outcome: outcome, Actor: actor, Type: e.Type, At: e.At})
	}

	convID := subject.FormatConversationID(conv.ID)
	linked, err := s.tka().LinkInTx(ctx, tx, actorOfGrant(g), req.Thread, convID, "interact")
	if err != nil {
		return nil, err
	}

	all, err := conversations.Events(ctx, tx, conv.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	out := map[string]any{
		"conversation_id": convID, "evt_id": results[len(results)-1].EvtID,
		"events": results, "my_touches_since_their_last": conversations.MyTouches(all),
	}
	if linked != nil {
		out["thread"] = linked
	}
	return out, nil
}

type postedReq struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	URL      string `json:"url"`
	PostedAt string `json:"posted_at"`
	ChatID   string `json:"chat_id"`
}

// POST /posted — one transaction: document kind=post, post_thread
// conversation owned by me, claim T post:<title>.published with the document
// as basis (spec §5.11).
func (s *Server) postPosted(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req postedReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	now := time.Now()
	subj, err := subject.Parse("post:" + people.Key(req.Title) + ".published")
	switch {
	case err != nil:
		writeError(w, http.StatusBadRequest, "empty_title", "title is required")
		return
	case strings.TrimSpace(req.Body) == "":
		writeError(w, http.StatusBadRequest, "empty_body", "body is required")
		return
	case strings.TrimSpace(req.URL) == "":
		writeError(w, http.StatusBadRequest, "conversation_key_missing", "posted needs the post url")
		return
	}
	url, err := conversations.NormalizeURL(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
		return
	}
	postedAt := now
	if v := strings.TrimSpace(req.PostedAt); v != "" {
		t, err := parseSince(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_posted_at", "posted_at must be YYYY-MM-DD or RFC 3339")
			return
		}
		postedAt = *t
	}
	g := grantFrom(ctx)
	st := stampOf(g)

	out, err := func() (map[string]any, error) {
		tx, err := s.tenantTx(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		doc, err := docs.StoreTx(ctx, tx, docs.StoreParams{
			Title: req.Title, Kind: "post", Body: req.Body, UserID: g.UserID, ActorID: g.ActorID, GrantID: g.GrantID, Scope: st.Scope,
		})
		if err != nil {
			return nil, err
		}
		me := people.Me
		conv, err := conversations.Ensure(ctx, tx, conversations.PostThread, url, &me, st)
		if err != nil {
			return nil, err
		}
		basis, err := subject.Basis(subject.FormatDocID(doc.Doc.ID), now)
		if err != nil {
			return nil, err
		}
		stx, err := ledger.LockSubject(ctx, tx, subj.String())
		if err != nil {
			return nil, err
		}
		claim, err := s.proposeInSubject(ctx, stx, g, proposal{
			Subject: subj.String(), Label: subject.LabelT, Mark: "T",
			Statement: "published " + postedAt.UTC().Format("2006-01-02") + " " + url,
			Owner:     g.UserID, ChatID: req.ChatID, Basis: &basis,
		})
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return map[string]any{
			"doc_id": subject.FormatDocID(doc.Doc.ID), "doc_outcome": doc.Outcome,
			"conversation_id": subject.FormatConversationID(conv.ID), "subject": subj.String(), "claim": claim,
		}, nil
	}()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func conversationBody(ctx context.Context, q people.Querier, c conversations.Conversation) (map[string]any, error) {
	evs, err := conversations.Events(ctx, q, c.ID)
	if err != nil {
		return nil, err
	}
	views := make([]eventView, 0, len(evs))
	for _, e := range evs {
		views = append(views, toEventView(e))
	}
	return map[string]any{
		"conversation": toConversationView(c, evs), "events": views, "my_touches_since_their_last": conversations.MyTouches(evs),
	}, nil
}

// GET /conversations/{conversation_id}
func (s *Server) getConversationByID(w http.ResponseWriter, r *http.Request) {
	id, ok := subject.ParseConversationID(chi.URLParam(r, "conversation_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_conversation_id", "conversation_id must be conv_<n>")
		return
	}
	var body map[string]any
	err := s.inTenant(r.Context(), func(q pgx.Tx) error {
		c, found, err := conversations.Get(r.Context(), q, id)
		if err != nil {
			return err
		}
		if !found {
			return &requestError{status: http.StatusNotFound, code: "not_found", detail: "no conversation " + subject.FormatConversationID(id)}
		}
		body, err = conversationBody(r.Context(), q, c)
		return err
	})
	if err != nil {
		writeAny(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// GET /conversation?url= | ?kind=dm&person=
func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	var body map[string]any
	err := s.inTenant(ctx, func(tx pgx.Tx) error {
		var c conversations.Conversation
		var found bool
		var err error
		switch {
		case strings.TrimSpace(q.Get("url")) != "":
			u, uerr := conversations.NormalizeURL(q.Get("url"))
			if uerr != nil {
				return badRequest("invalid_url", uerr.Error())
			}
			c, found, err = conversations.ByKey(ctx, tx, conversations.PostThread, u)
		case strings.TrimSpace(q.Get("person")) != "":
			kind := q.Get("kind")
			if kind == "" || kind == conversations.PostThread || !slices.Contains(conversations.Kinds, kind) {
				return badRequest("invalid_conversation_kind", "a conversation by person needs kind dm, email or connect")
			}
			matches, rerr := people.Resolve(ctx, tx, q.Get("person"))
			if rerr != nil {
				return rerr
			}
			if len(matches) > 1 {
				return ambiguous(ctx, s, tx, q.Get("person"), matches)
			}
			if len(matches) == 1 {
				c, found, err = conversations.ByKey(ctx, tx, kind, matches[0].Slug)
			}
		default:
			return badRequest("conversation_key_missing", "conversation_id, url, or kind + person required")
		}
		if err != nil {
			return err
		}
		if !found {
			return &requestError{status: http.StatusNotFound, code: "not_found", detail: "no such conversation"}
		}
		body, err = conversationBody(ctx, tx, c)
		return err
	})
	if err != nil {
		writeAny(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

type personClaim struct {
	ID        string    `json:"id"`
	Subject   string    `json:"subject"`
	Statement string    `json:"statement"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// GET /person?query= — everything the store holds about one person (spec
// §5.12). Never creates a person. Claims about people live in the old subject
// ledger, which belongs to the default store only.
func (s *Server) getPerson(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "query is required")
		return
	}
	legacy := tka.TenantOr(grantFrom(ctx).TenantID) == tka.DefaultTenant
	var out map[string]any
	err := s.inTenant(ctx, func(tx pgx.Tx) error {
		matches, err := people.Resolve(ctx, tx, query)
		if err != nil {
			return err
		}
		switch {
		case len(matches) == 0:
			out = map[string]any{"found": false}
			return nil
		case len(matches) > 1:
			cands, err := s.candidates(ctx, tx, matches)
			if err != nil {
				return err
			}
			out = map[string]any{"ambiguous": true, "candidates": cands}
			return nil
		}
		p := matches[0]
		prefix := "person:" + p.Slug + "."

		claims := []personClaim{}
		profile := map[string]*string{"company": nil, "role": nil, "degree": nil, "tier": nil}
		openItems := []personClaim{}
		if legacy {
			rows, err := s.Repo.LatestUnderPrefix(ctx, prefix)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if subject.Concluded(row.Move) {
					continue
				}
				c := personClaim{ID: subject.FormatID(row.ID), Subject: *row.Subject, Statement: row.Statement, Label: subject.Label(row.Mark, row.AB), CreatedAt: row.TS}
				claims = append(claims, c)
				if attr := strings.TrimPrefix(*row.Subject, prefix); slices.Contains([]string{"company", "role", "degree", "tier"}, attr) {
					statement := row.Statement
					profile[attr] = &statement
				}
				if c.Label == subject.LabelAB {
					openItems = append(openItems, c)
				}
			}
		}

		cs, err := conversations.Find(ctx, tx, conversations.FindParams{Person: p.Slug, Limit: findLimit})
		if err != nil {
			return err
		}
		sums := []conversationSummary{}
		for _, c := range cs {
			sum, err := conversationSummaryOf(ctx, tx, c)
			if err != nil {
				return err
			}
			sums = append(sums, sum)
			if !legacy {
				continue
			}
			trows, err := s.Repo.LatestUnderPrefix(ctx, subject.LegacyThreadSubject(c.ID))
			if err != nil {
				return err
			}
			for _, row := range trows {
				if row.Mark == "A" && row.AB && !subject.Concluded(row.Move) {
					openItems = append(openItems, personClaim{ID: subject.FormatID(row.ID), Subject: *row.Subject, Statement: row.Statement, Label: subject.LabelAB, CreatedAt: row.TS})
				}
			}
		}

		docsOut := []map[string]any{}
		if legacy {
			list, err := s.docs().Find(ctx, docs.FindParams{Ref: p.Slug, LatestOnly: true, Limit: findLimit})
			if err != nil {
				return err
			}
			for _, d := range list {
				docsOut = append(docsOut, map[string]any{"doc_id": subject.FormatDocID(d.ID), "title": d.Title, "kind": d.Kind})
			}
		}

		person := map[string]any{"slug": p.Slug, "name": p.Name, "aliases": p.Aliases}
		for k, v := range profile {
			person[k] = v
		}
		out = map[string]any{
			"person": person, "claims": claims, "conversations": sums, "open_items": openItems, "docs": docsOut,
		}
		return nil
	})
	if err != nil {
		writeAny(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
