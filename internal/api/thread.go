package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/conversations"
	"github.com/ankayma/tknowledge-personal/internal/people"
	"github.com/ankayma/tknowledge-personal/internal/subject"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// Threads (owner decision, 1/10/2026): one
// matter a person remembers by its keywords, pointing at claims, assumptions,
// documents, principles, people, conversations and other threads. The store
// side lives in tka; this file adds what only tk-api knows — the turns of a
// conversation and the names of people — and lookup, the one way in from
// what a person types.

// POST /tka/threads — {title, keywords[]} → the thread.
func (s *Server) postThreadOpen(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.ThreadOpenInput](w, r)
	if !ok {
		return
	}
	v, err := s.tka().OpenThread(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	s.writeThreadView(w, r.Context(), v)
}

// POST /tka/threads/tag — {thread, add[], remove[], reason?}.
func (s *Server) postThreadTag(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.ThreadTagInput](w, r)
	if !ok {
		return
	}
	res, err := s.tka().TagThread(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /tka/threads/link — {thread, ids[], note?}.
func (s *Server) postThreadLink(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.LinkInput](w, r)
	if !ok {
		return
	}
	res, err := s.tka().Link(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /tka/threads/unlink — {thread, id, reason}.
func (s *Server) postThreadUnlink(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.UnlinkInput](w, r)
	if !ok {
		return
	}
	v, err := s.tka().Unlink(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	s.writeThreadView(w, r.Context(), v)
}

// POST /tka/threads/close — {thread, reason}.
func (s *Server) postThreadClose(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.ThreadCloseInput](w, r)
	if !ok {
		return
	}
	v, err := s.tka().CloseThread(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	s.writeThreadView(w, r.Context(), v)
}

// POST /tka/threads/summarize — {thread, text, author, as_of?}: the thread's new
// current summary, an A with an author that every sentence ties to the
// thread's ids.
func (s *Server) postThreadSummarize(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.SummarizeInput](w, r)
	if !ok {
		return
	}
	sum, err := s.tka().Summarize(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// GET /tka/threads/{thread}?depth=1&summaries=all — TH-… or a keyword naming
// one thread. summaries=all adds the whole summary chain, oldest first.
func (s *Server) getThread(w http.ResponseWriter, r *http.Request) {
	depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
	all := false
	switch r.URL.Query().Get("summaries") {
	case "":
	case "all":
		all = true
	default:
		writeError(w, http.StatusBadRequest, "bad_request", `summaries is "all" or left out`)
		return
	}
	v, err := s.tka().ReadThread(r.Context(), tenantOf(r), chi.URLParam(r, "thread"), depth, all)
	if err != nil {
		writeStore(w, err)
		return
	}
	s.writeThreadView(w, r.Context(), v)
}

// POST /tka/refute — {id, observed, evidence?}: an open A-p whose not: happened.
func (s *Server) postRefute(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeInto[tka.RefuteInput](w, r)
	if !ok {
		return
	}
	a, err := s.tka().Refute(r.Context(), actorOf(r), in)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// linkedConversation is a conversation a thread points at, with its last turn.
type linkedConversation struct {
	conversationSummary
	LinkedAt string  `json:"linked_at"`
	Note     *string `json:"note,omitempty"`
}

type linkedPerson struct {
	ID       string   `json:"id"`
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Aliases  []string `json:"aliases"`
	LinkedAt string   `json:"linked_at"`
	Note     *string  `json:"note,omitempty"`
}

// writeThreadView fills in the conversations and people a thread points at —
// their last turns and names live in the conversation register — and writes
// the view.
func (s *Server) writeThreadView(w http.ResponseWriter, ctx context.Context, v tka.ThreadView) {
	raw, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	convs := []linkedConversation{}
	persons := []linkedPerson{}
	err = s.inTenant(ctx, func(q pgx.Tx) error {
		for _, l := range v.Conversations {
			id, ok := subject.ParseConversationID(l.ID)
			if !ok {
				continue
			}
			c, found, err := conversations.Get(ctx, q, id)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			sum, err := conversationSummaryOf(ctx, q, c)
			if err != nil {
				return err
			}
			convs = append(convs, linkedConversation{conversationSummary: sum, LinkedAt: l.LinkedAt.UTC().Format("2006-01-02T15:04:05Z"), Note: l.Note})
		}
		if len(v.Persons) == 0 {
			return nil
		}
		all, err := people.All(ctx, q)
		if err != nil {
			return err
		}
		for _, l := range v.Persons {
			slug := strings.TrimPrefix(l.ID, "person:")
			for _, p := range all {
				if p.Slug == slug {
					persons = append(persons, linkedPerson{ID: l.ID, Slug: p.Slug, Name: p.Name, Aliases: p.Aliases,
						LinkedAt: l.LinkedAt.UTC().Format("2006-01-02T15:04:05Z"), Note: l.Note})
				}
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out["conversations"] = convs
	out["persons"] = persons
	writeJSON(w, http.StatusOK, out)
}

// GET /tka/lookup?q=&scope= — the way in from what a person types: threads
// whose keywords or title come close, people whose name or alias does, and
// those people's conversations, documents by their text, then current
// summaries by theirs. Nothing is resolved here; the lists go back to the
// person, who picks. scope=summaries returns the summaries block alone.
//
// Summaries come last and in their own block: a document is evidence that
// backs a claim, a summary is an assumption with an author (owner 5/10/2026).
func (s *Server) getLookup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	scope := r.URL.Query().Get("scope")
	if scope != "" && scope != "summaries" {
		writeError(w, http.StatusBadRequest, "bad_request", `scope is "summaries" or left out`)
		return
	}
	summaries, err := s.tka().LookupSummaries(ctx, tenantOf(r), q)
	if err != nil {
		writeStore(w, err)
		return
	}
	if scope == "summaries" {
		writeJSON(w, http.StatusOK, map[string]any{"q": q, "summaries": summaries})
		return
	}
	threads, err := s.tka().LookupThreads(ctx, tenantOf(r), q)
	if err != nil {
		writeStore(w, err)
		return
	}
	type personHit struct {
		Slug    string   `json:"slug"`
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
	}
	persons := []personHit{}
	convs := []conversationSummary{}
	err = s.inTenant(ctx, func(tx pgx.Tx) error {
		found, err := people.Lookup(ctx, tx, q)
		if err != nil {
			return err
		}
		seen := map[int64]bool{}
		for _, p := range found {
			persons = append(persons, personHit{Slug: p.Slug, Name: p.Name, Aliases: p.Aliases})
			cs, err := conversations.Find(ctx, tx, conversations.FindParams{Person: p.Slug, Limit: findLimit})
			if err != nil {
				return err
			}
			for _, c := range cs {
				if seen[c.ID] {
					continue
				}
				seen[c.ID] = true
				sum, err := conversationSummaryOf(ctx, tx, c)
				if err != nil {
					return err
				}
				convs = append(convs, sum)
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Documents are found by what they say: search runs on their text (spec T–D §3).
	docs, err := s.tka().LookupDocuments(ctx, tenantOf(r), q)
	if err != nil {
		writeStore(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"q": q, "threads": threads, "persons": persons,
		"conversations": convs, "documents": docs, "summaries": summaries})
}
