// Package conversations is the interaction register: conversations keyed
// by normalised URL (post_thread) or by (kind, person), and insert-only
// events (turns) with dedup. Participants, last
// turn and touches are computed from events, never stored.
//
// A conversation is one exchange with one person on one channel, or the
// comments under one post. It was called a thread until 1/10/2026; a thread is
// now the meta-knowledge that groups conversations, claims and documents
// (internal/tka/thread.go, spec-thread-conversation-01-10-26.md).
//
// Every call runs inside a transaction that carries the store (app.tenant_id):
// row-level security keeps each store's people and conversations to itself.
package conversations

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/people"
	"github.com/ankayma/tknowledge-personal/internal/subject"
)

const PostThread = "post_thread"

var (
	Kinds = []string{PostThread, "dm", "email", "connect"}
	Types = []string{"comment", "reply", "react", "note", "dm", "email", "accept", "view"}
	// NoBody are the event types that carry no body.
	NoBody = []string{"react", "accept", "view"}
)

var ErrURL = errors.New("url must be an absolute http(s) URL")

// NormalizeURL is the post_thread key: lowercase scheme and host, no user
// info, query or fragment, no trailing slash.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrURL
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	u.Path, u.RawPath = strings.TrimRight(u.Path, "/"), ""
	return u.String(), nil
}

type Conversation struct {
	ID         int64
	Kind       string
	URL        *string
	PersonSlug *string
	Owner      *string
	CreatedAt  time.Time
}

type Event struct {
	ID             int64
	ConversationID int64
	Actor          string
	Type           string
	Body           *string
	Verbatim       bool
	At             time.Time
	InReplyTo      *int64
	ChatID         *string
	CreatedAt      time.Time
}

const conversationCols = `id, kind, url, person_slug, owner_of_conversation, created_at`
const eventCols = `id, conversation_id, actor, type, body, verbatim, at, in_reply_to, chat_id, created_at`

func scanConversation(row pgx.Row) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.Kind, &c.URL, &c.PersonSlug, &c.Owner, &c.CreatedAt)
	return c, err
}

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.ConversationID, &e.Actor, &e.Type, &e.Body, &e.Verbatim, &e.At, &e.InReplyTo, &e.ChatID, &e.CreatedAt)
	return e, err
}

func one[T any](scan func(pgx.Row) (T, error), row pgx.Row) (T, bool, error) {
	v, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		var zero T
		return zero, false, nil
	}
	return v, err == nil, err
}

func Get(ctx context.Context, q people.Querier, id int64) (Conversation, bool, error) {
	return one(scanConversation, q.QueryRow(ctx, `SELECT `+conversationCols+` FROM conversations WHERE id = $1`, id))
}

// ByKey finds a conversation by its key: url for post_thread, person for the others.
func ByKey(ctx context.Context, q people.Querier, kind, key string) (Conversation, bool, error) {
	if kind == PostThread {
		return one(scanConversation, q.QueryRow(ctx, `SELECT `+conversationCols+` FROM conversations WHERE kind = $1 AND url = $2`, kind, key))
	}
	return one(scanConversation, q.QueryRow(ctx, `SELECT `+conversationCols+` FROM conversations WHERE kind = $1 AND person_slug = $2`, kind, key))
}

// Ensure returns the conversation for (kind, key), opening it if needed, and
// holds its lock for the rest of tx.
func Ensure(ctx context.Context, tx pgx.Tx, kind, key string, owner *string, st people.Stamp) (Conversation, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"conversations:"+st.Tenant()+":"+kind+":"+key); err != nil {
		return Conversation{}, err
	}
	c, found, err := ByKey(ctx, tx, kind, key)
	if err != nil || found {
		return c, err
	}
	var urlCol, personCol *string
	if kind == PostThread {
		urlCol = &key
	} else {
		personCol = &key
	}
	return scanConversation(tx.QueryRow(ctx, `
		INSERT INTO conversations (tenant_id, kind, url, person_slug, owner_of_conversation, user_id, actor_id, grant_id, scope)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+conversationCols,
		st.Tenant(), kind, urlCol, personCol, owner, st.UserID, st.ActorID, st.GrantID, st.Scope))
}

func EventByID(ctx context.Context, q people.Querier, id int64) (Event, bool, error) {
	return one(scanEvent, q.QueryRow(ctx, `SELECT `+eventCols+` FROM events WHERE id = $1`, id))
}

// Events of a conversation, by time.
func Events(ctx context.Context, q people.Querier, conversationID int64) ([]Event, error) {
	rows, err := q.Query(ctx, `SELECT `+eventCols+` FROM events WHERE conversation_id = $1 ORDER BY at, id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func normBody(b *string) string {
	if b == nil {
		return ""
	}
	return subject.NormalizeStatement(*b)
}

// InsertEvent writes e unless the conversation already has the same actor,
// type and normalised body less than a minute apart — then that event
// is returned with duplicate = true. Call with the conversation's lock held.
func InsertEvent(ctx context.Context, tx pgx.Tx, e Event, st people.Stamp) (Event, bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+eventCols+` FROM events
		WHERE conversation_id = $1 AND actor = $2 AND type = $3
		  AND at > $4::timestamptz - interval '1 minute' AND at < $4::timestamptz + interval '1 minute'
		ORDER BY id`, e.ConversationID, e.Actor, e.Type, e.At)
	if err != nil {
		return Event{}, false, err
	}
	var near []Event
	for rows.Next() {
		x, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return Event{}, false, err
		}
		near = append(near, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Event{}, false, err
	}
	for _, x := range near {
		if normBody(x.Body) == normBody(e.Body) {
			return x, true, nil
		}
	}
	inserted, err := scanEvent(tx.QueryRow(ctx, `
		INSERT INTO events (tenant_id, conversation_id, actor, type, body, verbatim, at, in_reply_to, chat_id, user_id, actor_id, grant_id, scope)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+eventCols,
		st.Tenant(), e.ConversationID, e.Actor, e.Type, e.Body, e.Verbatim, e.At, e.InReplyTo, e.ChatID, st.UserID, st.ActorID, st.GrantID, st.Scope))
	return inserted, false, err
}

// MyTouches counts "me" events after the newest event of anyone else (spec
// §5.9, the two-touch ceiling). events must be in time order.
func MyTouches(events []Event) int {
	n := 0
	for _, e := range events {
		if e.Actor == people.Me {
			n++
		} else {
			n = 0
		}
	}
	return n
}

// Participants: the conversation's owner and person, then every actor in
// order of first appearance.
func Participants(c Conversation, events []Event) []string {
	var out []string
	add := func(s *string) {
		if s != nil && *s != "" && !slices.Contains(out, *s) {
			out = append(out, *s)
		}
	}
	add(c.Owner)
	add(c.PersonSlug)
	for i := range events {
		add(&events[i].Actor)
	}
	return out
}

type FindParams struct {
	Query  string
	Person string // slug
	Since  *time.Time
	Limit  int
}

// Find lists conversations whose events mention query and/or that involve
// person, newest activity first.
func Find(ctx context.Context, q people.Querier, p FindParams) ([]Conversation, error) {
	query := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(p.Query))
	var since any
	if p.Since != nil {
		since = *p.Since
	}
	rows, err := q.Query(ctx, `
		SELECT `+conversationCols+` FROM (
			SELECT c.*, (SELECT max(e.at) FROM events e WHERE e.conversation_id = c.id) AS last_at FROM conversations c
		) c
		WHERE ($1 = '' OR EXISTS (SELECT 1 FROM events e WHERE e.conversation_id = c.id AND e.body ILIKE '%' || $1 || '%'))
		  AND ($2 = '' OR c.person_slug = $2 OR c.owner_of_conversation = $2
		       OR EXISTS (SELECT 1 FROM events e WHERE e.conversation_id = c.id AND e.actor = $2))
		  AND ($3::timestamptz IS NULL OR c.last_at >= $3::timestamptz)
		ORDER BY c.last_at DESC NULLS LAST, c.id DESC
		LIMIT $4`, query, p.Person, since, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
