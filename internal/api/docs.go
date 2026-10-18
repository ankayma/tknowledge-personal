package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/conversations"
	"github.com/ankayma/tknowledge-personal/internal/docs"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/people"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/subject"
)

// SPEC §15 — versioned documents: the ledger side of the MCP spec's store /
// find / read, and the existence check behind a doc_… / evt_… basis.

const findLimit = 200

func (s *Server) docs() *docs.Repo { return docs.New(s.Repo.Pool) }

func parseSince(v string) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, v); err != nil {
			return nil, errors.New("since must be YYYY-MM-DD or RFC 3339")
		}
	}
	return &t, nil
}

// basisSourceExists: a fact basis naming doc_<n> or evt_<n> must name a
// stored document or event (spec §4.1: MCP checks the basis exists, not its
// content). Other sources are checked for shape only.
func (s *Server) basisSourceExists(ctx context.Context, b *ledger.Basis) (bool, string, error) {
	if b == nil || b.Kind != "fact" {
		return true, "", nil // principle / derivation are resolved in admitOne
	}
	ref := subject.BasisRef(b)
	if id, ok := subject.ParseDocRef(ref); ok {
		exists, err := s.docs().Exists(ctx, id)
		return exists, ref, err
	}
	if id, ok := subject.ParseEventRef(ref); ok {
		var exists bool
		err := s.inTenant(ctx, func(q pgx.Tx) error {
			return q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE id = $1)`, id).Scan(&exists)
		})
		return exists, ref, err
	}
	if id, version, ok := setup.ParseBasis(ref); ok {
		t, found := s.templates().Get(id)
		return found && t.Version == version, ref, nil
	}
	return true, "", nil
}

// normalizeRefs: a ref with ':' is a subject (namespace must exist); anything
// else is a person slug. Returns a problem message instead of refs when one
// is not acceptable.
func (s *Server) normalizeRefs(ctx context.Context, refs []string) ([]string, string, error) {
	out := []string{}
	for _, raw := range refs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var ref string
		if strings.Contains(raw, ":") {
			subj, err := subject.Parse(raw)
			if err != nil {
				return nil, "ref " + raw + ": " + err.Error(), nil
			}
			known, err := s.acceptNamespace(ctx, subj.Namespace, nil)
			if err != nil {
				return nil, "", err
			}
			if !known {
				return nil, "ref " + raw + ": namespace " + subj.Namespace + " is not in the catalogue", nil
			}
			ref = subj.String()
		} else {
			ref = people.Key(raw)
			if ref == "" || strings.ContainsAny(raw, "#/@") {
				return nil, "ref " + raw + ": neither a subject nor a person slug", nil
			}
		}
		if !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	slices.Sort(out)
	return out, "", nil
}

type storeReq struct {
	Title string   `json:"title"`
	Kind  string   `json:"kind"`
	Body  string   `json:"body"`
	Refs  []string `json:"refs"`
}

func storeRejected(w http.ResponseWriter, code, msg string) {
	writeJSON(w, http.StatusOK, map[string]any{"outcome": "rejected", "error": itemError{Code: code, Message: msg}})
}

// POST /docs — {title, kind, body, refs?} → {outcome, doc_id, version, previous_id?}.
func (s *Server) postDoc(w http.ResponseWriter, r *http.Request) {
	var req storeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	switch {
	case docs.TitleKey(req.Title) == "":
		storeRejected(w, "EMPTY_TITLE", "title is required")
		return
	case req.Kind == "post":
		storeRejected(w, "USE_POSTED", "a post is stored through posted")
		return
	case !slices.Contains(docs.Kinds, req.Kind):
		storeRejected(w, "INVALID_KIND", "kind must be plan, spec, analysis, assessment or rule")
		return
	case strings.TrimSpace(req.Body) == "":
		storeRejected(w, "EMPTY_BODY", "body is required")
		return
	}
	refs, problem, err := s.normalizeRefs(r.Context(), req.Refs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if problem != "" {
		storeRejected(w, "INVALID_REF", problem)
		return
	}
	g := grantFrom(r.Context())
	producer, err := producerFor(g, "")
	if err != nil {
		writeIdentityError(w, err)
		return
	}
	res, err := s.docs().Store(r.Context(), docs.StoreParams{
		Title: req.Title, Kind: req.Kind, Body: req.Body, Refs: refs,
		UserID: g.UserID, ActorID: producer, GrantID: g.GrantID, Scope: "user:" + g.UserID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := map[string]any{"outcome": res.Outcome, "doc_id": subject.FormatDocID(res.Doc.ID), "version": res.Doc.Version}
	if res.Previous != nil {
		out["previous_id"] = subject.FormatDocID(res.Previous.ID)
	}
	writeJSON(w, http.StatusOK, out)
}

type docMeta struct {
	DocID     string    `json:"doc_id"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	Version   int       `json:"version"`
	Refs      []string  `json:"refs"`
	CreatedAt time.Time `json:"created_at"`
}

// GET /find?query=&kind=&person=&since=&latest_only=true — documents (metadata,
// no body) and conversations. kind=conversation → conversations only; a
// document kind → documents only. person resolves a name: several matches →
// 409 ambiguous_person.
func (s *Server) getFind(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	kind := strings.TrimSpace(q.Get("kind"))
	if kind != "" && kind != "conversation" && !slices.Contains(docs.Kinds, kind) {
		writeError(w, http.StatusBadRequest, "bad_kind", "kind must be one of "+strings.Join(docs.Kinds, ", ")+" or conversation")
		return
	}
	since, err := parseSince(q.Get("since"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_since", err.Error())
		return
	}
	truncated := false
	metas := []docMeta{}
	sums := []conversationSummary{}
	err = s.inTenant(ctx, func(tx pgx.Tx) error {
		slug := ""
		if person := strings.TrimSpace(q.Get("person")); person != "" {
			matches, err := people.Resolve(ctx, tx, person)
			if err != nil {
				return err
			}
			switch len(matches) {
			case 0:
				return nil // a lookup never creates a person
			case 1:
				slug = matches[0].Slug
			default:
				return ambiguous(ctx, s, tx, person, matches)
			}
		}
		if kind != "conversation" {
			list, err := s.docs().Find(ctx, docs.FindParams{
				Query: q.Get("query"), Kind: kind, Since: since, LatestOnly: q.Get("latest_only") == "true", Ref: slug, Limit: findLimit + 1,
			})
			if err != nil {
				return err
			}
			if len(list) > findLimit {
				truncated, list = true, list[:findLimit]
			}
			for _, d := range list {
				metas = append(metas, docMeta{DocID: subject.FormatDocID(d.ID), Title: d.Title, Kind: d.Kind, Version: d.Version, Refs: d.Refs, CreatedAt: d.TS})
			}
		}
		if kind == "" || kind == "conversation" {
			list, err := conversations.Find(ctx, tx, conversations.FindParams{Query: q.Get("query"), Person: slug, Since: since, Limit: findLimit + 1})
			if err != nil {
				return err
			}
			if len(list) > findLimit {
				truncated, list = true, list[:findLimit]
			}
			for _, c := range list {
				sum, err := conversationSummaryOf(ctx, tx, c)
				if err != nil {
					return err
				}
				sums = append(sums, sum)
			}
		}
		return nil
	})
	if err != nil {
		writeAny(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"docs": metas, "conversations": sums, "count": len(metas) + len(sums), "truncated": truncated})
}

// GET /docs/{doc_id} — the full document.
func (s *Server) getDoc(w http.ResponseWriter, r *http.Request) {
	id, ok := subject.ParseDocID(chi.URLParam(r, "doc_id"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_doc_id", "doc_id must be doc_<n>")
		return
	}
	d, found, err := s.docs().Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no document "+subject.FormatDocID(id))
		return
	}
	var supersedes *string
	if d.Supersedes != nil {
		prev := subject.FormatDocID(*d.Supersedes)
		supersedes = &prev
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"doc_id": subject.FormatDocID(d.ID), "title": d.Title, "kind": d.Kind, "version": d.Version,
		"body": d.Body, "refs": d.Refs, "created_at": d.TS, "supersedes": supersedes,
	})
}
