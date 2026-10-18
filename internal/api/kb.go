package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/people"
	"github.com/ankayma/tknowledge-personal/internal/subject"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

// SPEC §14 — claims by subject, the ledger side of the MCP spec's admit / open
// / history. Decisions (dedup, supersedes, refusals) are made here, under the
// subject's lock; causes are not: tk-api calls no one, so a T claim is written
// as its propose row and the caller follows with cause + promote.

// openLimit is spec §5.4's ceiling; the window for A/T is org config
// open.window_days (§17).
const openLimit = 200

type admitItem struct {
	Subject   string `json:"subject"`
	Statement string `json:"statement"`
	Label     string `json:"label"`
	Basis     string `json:"basis"`
	Owner     string `json:"owner"`
	ChatID    string `json:"chat_id"`
}

type candidate struct {
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Company string `json:"company,omitempty"`
}

type itemError struct {
	Code       string      `json:"code"`
	Message    string      `json:"message"`
	Candidates []candidate `json:"candidates,omitempty"`
}

type admitResult struct {
	Subject           string        `json:"subject"`
	Outcome           string        `json:"outcome"` // new | duplicate | supersedes | rejected
	ID                string        `json:"id,omitempty"`
	ClaimID           int64         `json:"claim_id,omitempty"`
	Label             string        `json:"label,omitempty"`
	PreviousID        string        `json:"previous_id,omitempty"`
	PreviousStatement string        `json:"previous_statement,omitempty"`
	NeedsPromote      bool          `json:"needs_promote,omitempty"`
	Basis             *ledger.Basis `json:"basis,omitempty"`
	Error             *itemError    `json:"error,omitempty"`
}

func rejected(subj, code, msg string) admitResult {
	return admitResult{Subject: subj, Outcome: "rejected", Error: &itemError{Code: code, Message: msg}}
}

func nullable(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

func stampOf(g grant.Grant) people.Stamp {
	return people.Stamp{UserID: g.UserID, ActorID: g.ActorID, GrantID: g.GrantID, Scope: "user:" + g.UserID,
		TenantID: tka.TenantOr(g.TenantID)}
}

// POST /admit — {claims: [...]} → {results: [...]}, one result per claim, in
// order. A refused claim does not stop the others.
func (s *Server) postAdmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Claims []admitItem `json:"claims"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Claims) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "claims: [{subject, statement, label, basis?, owner?, chat_id?}] required")
		return
	}
	g := grantFrom(r.Context())
	now := time.Now()
	out := make([]admitResult, 0, len(req.Claims))
	for _, it := range req.Claims {
		res, err := s.admitOne(r.Context(), g, it, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		out = append(out, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}

func (s *Server) admitOne(ctx context.Context, g grant.Grant, it admitItem, now time.Time) (admitResult, error) {
	subj, err := subject.Parse(it.Subject)
	if err != nil {
		return rejected(it.Subject, "INVALID_SUBJECT", err.Error()), nil
	}
	if ok, err := s.acceptNamespace(ctx, subj.Namespace, &g); err != nil {
		return admitResult{}, err
	} else if !ok {
		return rejected(subj.String(), "UNKNOWN_NAMESPACE", "namespace "+subj.Namespace+" is not in the catalogue"), nil
	}
	label, mark, ab, err := subject.ParseLabel(it.Label)
	if err != nil {
		return rejected(subj.String(), "INVALID_LABEL", err.Error()), nil
	}
	if strings.TrimSpace(it.Statement) == "" {
		return rejected(subj.String(), "EMPTY_STATEMENT", "statement is required"), nil
	}
	var basis *ledger.Basis
	if mark == "T" || strings.TrimSpace(it.Basis) != "" {
		b, err := subject.Basis(it.Basis, now)
		switch {
		case err == subject.ErrNoBasis:
			return rejected(subj.String(), "T_WITHOUT_BASIS", err.Error()), nil
		case err != nil:
			return rejected(subj.String(), "INVALID_BASIS", err.Error()), nil
		}
		basis = &b
	}
	if basis != nil && (basis.Kind == "principle" || basis.Kind == "derivation") {
		anchor := s.anchorPrinciple
		if basis.Kind == "derivation" {
			anchor = s.anchorDerivation
		}
		res, err := anchor(ctx, subj, basis)
		if err != nil || res != nil {
			if res != nil {
				return *res, nil
			}
			return admitResult{}, err
		}
	}
	if ok, ref, err := s.basisSourceExists(ctx, basis); err != nil {
		return admitResult{}, err
	} else if !ok {
		return rejected(subj.String(), "BASIS_NOT_FOUND", "basis "+ref+" names nothing stored"), nil
	}

	// person:<entity> names a person: resolve the entity, create the person
	// the first time (spec §4.6), refuse when it matches several.
	if subj.Namespace == "person" {
		slug, cands, err := s.resolvePersonSubject(ctx, g, subj.Entity)
		if errors.Is(err, people.ErrName) {
			return rejected(subj.String(), "INVALID_SUBJECT", err.Error()), nil
		}
		if err != nil {
			return admitResult{}, err
		}
		if cands != nil {
			res := rejected(subj.String(), "AMBIGUOUS_PERSON", "\""+subj.Entity+"\" matches "+strconv.Itoa(len(cands))+" people")
			res.Error.Candidates = cands
			return res, nil
		}
		subj.Entity = slug
	}
	key := subj.String()
	owner := it.Owner
	if strings.TrimSpace(owner) == "" {
		owner = g.UserID
	}

	var res admitResult
	err = s.Repo.WithSubjectLock(ctx, key, func(stx *ledger.SubjectTx) error {
		var inner error
		res, inner = s.proposeInSubject(ctx, stx, g, proposal{
			Subject: key, Label: label, Mark: mark, AB: ab, Statement: it.Statement, Basis: basis, Owner: owner, ChatID: it.ChatID,
		})
		return inner
	})
	return res, err
}

// proposal is one validated claim, decided under its subject's lock.
type proposal struct {
	Subject, Label, Mark, Statement, Owner, ChatID string
	AB                                             bool
	Basis                                          *ledger.Basis
}

// proposeInSubject applies spec §5.1's rules against the subject's newest row
// and writes the propose row when the claim is new or a new version. Used by
// admit and by the claims interact and posted write in their own transaction.
func (s *Server) proposeInSubject(ctx context.Context, stx *ledger.SubjectTx, g grant.Grant, p proposal) (admitResult, error) {
	latest, found, err := stx.Latest(ctx)
	if err != nil {
		return admitResult{}, err
	}
	var prev *ledger.Row
	if found {
		latestLabel := subject.Label(latest.Mark, latest.AB)
		same := subject.NormalizeStatement(latest.Statement) == subject.NormalizeStatement(p.Statement)
		if same && !subject.Concluded(latest.Move) {
			switch {
			case latestLabel == p.Label:
				return admitResult{Subject: p.Subject, Outcome: "duplicate", ID: subject.FormatID(latest.ID), ClaimID: latest.ClaimID, Label: latestLabel}, nil
			case p.Mark == "T":
				res := rejected(p.Subject, "LABEL_UPGRADE_USE_PROMOTE", "same statement already admitted as "+latestLabel+"; promote "+subject.FormatID(latest.ID))
				res.ID = subject.FormatID(latest.ID)
				return res, nil
			case latest.Mark == "T":
				res := rejected(p.Subject, "LABEL_DOWNGRADE_NOT_ADMITTED", "same statement is T; admit cannot lower it")
				res.ID = subject.FormatID(latest.ID)
				return res, nil
			}
			// A ↔ A-b on the same statement: a new version, like any change.
		}
		prev = &latest
	}
	producer, err := producerFor(g, "")
	if err != nil {
		return admitResult{}, err
	}
	scope := "user:" + g.UserID
	subj := p.Subject
	owner := p.Owner
	ins := ledger.InsertParams{
		Statement: p.Statement, Mark: "A", Basis: p.Basis, Producer: producer, Move: "propose",
		Subject: &subj, Owner: &owner, ChatID: nullable(p.ChatID), AB: p.AB, Scope: &scope, GrantID: &g.GrantID,
	}
	if prev != nil {
		ins.Supersedes = &prev.ID
	}
	id, err := stx.Insert(ctx, ins)
	if err != nil {
		return admitResult{}, err
	}
	res := admitResult{Subject: p.Subject, Outcome: "new", ID: subject.FormatID(id), ClaimID: id, Label: p.Label}
	if prev != nil {
		res.Outcome = "supersedes"
		res.PreviousID = subject.FormatID(prev.ID)
		res.PreviousStatement = prev.Statement
	}
	if p.Mark == "T" {
		res.NeedsPromote = true
		res.Basis = p.Basis
	}
	return res, nil
}

// resolvePersonSubject resolves the entity of a person: subject to one slug,
// creating the person when nobody matches.
func (s *Server) resolvePersonSubject(ctx context.Context, g grant.Grant, entity string) (string, []candidate, error) {
	tx, err := s.Repo.Pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback(ctx)
	if err := tka.SetTenant(ctx, tx, g.TenantID); err != nil {
		return "", nil, err
	}
	p, matches, err := people.Ensure(ctx, tx, entity, stampOf(g))
	if err != nil {
		return "", nil, err
	}
	if matches != nil {
		cands, err := s.candidates(ctx, tx, matches)
		return "", cands, err
	}
	return p.Slug, nil, tx.Commit(ctx)
}

// candidates describes the persons a name matched (spec §5.12).
func (s *Server) candidates(ctx context.Context, q people.Querier, matches []people.Person) ([]candidate, error) {
	out := make([]candidate, 0, len(matches))
	for _, p := range matches {
		company, err := currentStatement(ctx, q, "person:"+p.Slug+".company")
		if err != nil {
			return nil, err
		}
		out = append(out, candidate{Slug: p.Slug, Name: p.Name, Company: company})
	}
	return out, nil
}

// currentStatement is the statement of a subject's newest row, or "" when
// there is none or it was concluded (rejected, refuted, closed).
func currentStatement(ctx context.Context, q people.Querier, subj string) (string, error) {
	// The old subject ledger is the default store's; another store has no
	// statements in it, even under a slug that happens to match.
	var statement, move string
	err := q.QueryRow(ctx, `SELECT statement, move::text FROM ledger WHERE subject = $1
		AND coalesce(nullif(current_setting('app.tenant_id', true), ''), 'default') = 'default'
		ORDER BY id DESC LIMIT 1`, subj).Scan(&statement, &move)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if subject.Concluded(move) {
		return "", nil
	}
	return statement, nil
}

type openClaim struct {
	ID        string    `json:"id"`
	ClaimID   int64     `json:"claim_id"`
	Subject   string    `json:"subject"`
	Statement string    `json:"statement"`
	Label     string    `json:"label"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ChatID    *string   `json:"chat_id"`
	Owner     *string   `json:"owner"`
}

// GET /open?scope=<prefix>&since=<date>&labels=A-b,T — the newest row of every
// subject still open. Default: every open A-b, plus A/T from the last 7 days.
func (s *Server) getOpen(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := strings.ToLower(strings.TrimSpace(q.Get("scope")))
	var since any
	if t, err := parseSince(q.Get("since")); err != nil {
		writeError(w, http.StatusBadRequest, "bad_since", err.Error())
		return
	} else if t != nil {
		since = *t
	}
	var labels any
	if v := strings.TrimSpace(q.Get("labels")); v != "" {
		var list []string
		for _, part := range strings.Split(v, ",") {
			l, _, _, err := subject.ParseLabel(part)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_labels", err.Error())
				return
			}
			list = append(list, l)
		}
		labels = list
	}

	window, err := s.openWindowDays(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	rows, err := s.Repo.Pool.Query(r.Context(), `
		WITH latest AS (
			SELECT DISTINCT ON (subject) id, claim_id, subject, statement, mark, ab, move, ts, chat_id, owner
			FROM ledger
			WHERE subject IS NOT NULL AND left(subject, length($1)) = $1
			ORDER BY subject, id DESC
		), labeled AS (
			SELECT *, CASE WHEN mark = 'T' THEN 'T' WHEN ab THEN 'A-b' ELSE 'A' END AS label
			FROM latest
			WHERE move NOT IN ('resolve', 'drop', 'reject', 'refute')
		)
		SELECT id, claim_id, subject, statement, label, ts, chat_id, owner
		FROM labeled
		WHERE ($2::text[] IS NULL OR label = ANY($2::text[]))
		  AND CASE WHEN $3::timestamptz IS NOT NULL THEN ts >= $3::timestamptz
		           ELSE label = 'A-b' OR ts >= now() - make_interval(days => $4) END
		ORDER BY ts DESC, id DESC
		LIMIT $5`, prefix, labels, since, window, openLimit+1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer rows.Close()
	claims := []openClaim{}
	for rows.Next() {
		var c openClaim
		var id int64
		if err := rows.Scan(&id, &c.ClaimID, &c.Subject, &c.Statement, &c.Label, &c.CreatedAt, &c.ChatID, &c.Owner); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		c.ID, c.Status = subject.FormatID(id), "open"
		claims = append(claims, c)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	truncated := len(claims) > openLimit
	if truncated {
		claims = claims[:openLimit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"claims": claims, "count": len(claims), "truncated": truncated})
}

type historyEntry struct {
	ID         string    `json:"id"`
	ClaimID    int64     `json:"claim_id"`
	Statement  string    `json:"statement"`
	Label      string    `json:"label"`
	Basis      string    `json:"basis,omitempty"`
	Move       string    `json:"move"`
	Signer     *string   `json:"signer"`
	Producer   string    `json:"producer"`
	Supersedes string    `json:"supersedes,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// GET /history?subject= — every row under the subject, old → new.
func (s *Server) getHistory(w http.ResponseWriter, r *http.Request) {
	subj, err := subject.Parse(r.URL.Query().Get("subject"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_subject", err.Error())
		return
	}
	rows, err := s.Repo.BySubject(r.Context(), subj.String())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	chain := make([]historyEntry, 0, len(rows))
	for _, row := range rows {
		e := historyEntry{
			ID: subject.FormatID(row.ID), ClaimID: row.ClaimID, Statement: row.Statement,
			Label: subject.Label(row.Mark, row.AB), Basis: subject.BasisRef(row.Basis), Move: row.Move,
			Signer: row.Signer, Producer: row.Producer, CreatedAt: row.TS,
		}
		if row.Supersedes != nil {
			e.Supersedes = subject.FormatID(*row.Supersedes)
		}
		chain = append(chain, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"subject": subj.String(), "chain": chain})
}

// GET /rows/{row_id} — a row, its claim's current row, and whether that claim
// was superseded. Lets clients refuse a promote/close before issuing a cause.
func (s *Server) getRow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "row_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_row_id", "row_id must be an integer")
		return
	}
	row, found, err := s.Repo.Row(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such row")
		return
	}
	cur, _, err := s.Repo.Current(r.Context(), row.ClaimID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	superseded := false
	if cur.Subject != nil {
		latest, ok, err := s.Repo.LatestBySubject(r.Context(), *cur.Subject)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		superseded = ok && latest.ClaimID != cur.ClaimID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": subject.FormatID(row.ID), "row_id": row.ID, "claim_id": row.ClaimID,
		"subject": deref(row.Subject), "statement": row.Statement, "label": subject.Label(row.Mark, row.AB),
		"superseded": superseded,
		"current": map[string]any{
			"id": subject.FormatID(cur.ID), "row_id": cur.ID, "label": subject.Label(cur.Mark, cur.AB),
			"move": cur.Move, "mark": cur.Mark, "status": statusFor(cur.Move),
		},
	})
}
