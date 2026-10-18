package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/ledger"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/subject"
)

// The Principle List is its own trust class (P.9): only it and sourced facts
// are trusted without audit. So a principle is not a document — it is a claim
// series of its own, subject principle:<code>.statement for the canonical
// sentence (T, basis kind principle) and principle:<code>.gloss for the `[A]`
// reading note. A claim resting on a principle carries basis principle:<code>,
// which this file resolves to the principle's own row: eff §4 then requires
// that row to be eff=T, so demoting a principle reaches everything under it.

type principleView struct {
	Code      string  `json:"code"`
	Statement string  `json:"statement"`
	Note      string  `json:"note,omitempty"`
	ID        string  `json:"id"`
	ClaimID   int64   `json:"claim_id"`
	Signer    *string `json:"signer"`
	Label     string  `json:"label"`
}

// principleRow is the claim_id of the current canonical row of one code, when
// it stands (T, not concluded).
func (s *Server) principleRow(ctx context.Context, code string) (int64, bool, error) {
	var claimID int64
	var mark, move string
	err := s.Repo.Pool.QueryRow(ctx, `
		SELECT claim_id, mark, move FROM ledger WHERE subject = $1 ORDER BY id DESC LIMIT 1`,
		principleSubject(code, "statement")).Scan(&claimID, &mark, &move)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return claimID, mark == "T" && !subject.Concluded(move), nil
}

func principleSubject(code, attr string) string {
	return "principle:" + setup.CodeKey(code) + "." + attr
}

// currentPrinciples reads the newest row of every principle subject, in code
// order (P.10 after P.9).
func (s *Server) currentPrinciples(ctx context.Context) ([]principleView, error) {
	rows, err := s.Repo.Pool.Query(ctx, `
		SELECT subject, id, claim_id, statement, mark, ab, move, signer FROM (
			SELECT DISTINCT ON (subject) subject, id, claim_id, statement, mark, ab, move, signer
			FROM ledger WHERE subject LIKE 'principle:%' ORDER BY subject, id DESC) p
		WHERE move NOT IN ('resolve', 'drop', 'reject', 'refute')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byCode := map[string]*principleView{}
	for rows.Next() {
		var subj, statement, mark, move string
		var id, claimID int64
		var ab bool
		var signer *string
		if err := rows.Scan(&subj, &id, &claimID, &statement, &mark, &ab, &move, &signer); err != nil {
			return nil, err
		}
		parsed, err := subject.Parse(subj)
		if err != nil {
			continue
		}
		code := setup.CodeFromKey(parsed.Entity)
		p := byCode[code]
		if p == nil {
			p = &principleView{Code: code}
			byCode[code] = p
		}
		switch parsed.Attribute {
		case "statement":
			p.Statement, p.ID, p.ClaimID, p.Signer = statement, subject.FormatID(id), claimID, signer
			p.Label = subject.Label(mark, ab)
		case "gloss":
			p.Note = statement
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]principleView, 0, len(byCode))
	for _, p := range byCode {
		if strings.TrimSpace(p.Statement) != "" {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return setup.CodeLess(out[i].Code, out[j].Code) })
	return out, nil
}

// anchorPrinciple fills basis.refs for a basis principle:<code>. The
// principle's own canonical row is the axiom itself (refs stays empty, eff §4);
// any other claim is anchored to the row, and an unknown code is refused.
// A non-nil result is the refusal to return.
func (s *Server) anchorPrinciple(ctx context.Context, subj subject.Subject, basis *ledger.Basis) (*admitResult, error) {
	raw, _ := subject.PrincipleCode(subject.BasisRef(basis))
	code, ok := setup.NormalizeCode(raw)
	if !ok {
		res := rejected(subj.String(), "INVALID_BASIS", "principle basis must be principle:P.<n>")
		return &res, nil
	}
	basis.Refs = []int64{}
	if subj.Namespace == "principle" && setup.CodeKey(subj.Entity) == setup.CodeKey(code) {
		return nil, nil // the principle standing on itself
	}
	id, stands, err := s.principleRow(ctx, code)
	if err != nil {
		return nil, err
	}
	if !stands {
		res := rejected(subj.String(), "BASIS_NOT_FOUND", "basis principle:"+code+" names no principle that stands")
		return &res, nil
	}
	basis.Refs = []int64{id}
	return nil, nil
}

// anchorDerivation fills basis.refs for derivation:clm_…,clm_… — the premises
// this claim follows from. Each must be a claim that stands; is_truth then
// requires every one of them to be eff=T, so demoting a premise reaches here.
func (s *Server) anchorDerivation(ctx context.Context, subj subject.Subject, basis *ledger.Basis) (*admitResult, error) {
	ref := subject.BasisRef(basis)
	ids, ok := subject.DerivationRefs(ref)
	if !ok {
		res := rejected(subj.String(), "INVALID_BASIS", "derivation basis must be derivation:clm_<n>[,clm_<n>…]")
		return &res, nil
	}
	// A caller cites the row it was given (clm_<row id>, e.g. the T row of a
	// promote); the basis records the claim it belongs to, as refs always do.
	refs := make([]int64, 0, len(ids))
	for _, id := range ids {
		var claimID int64
		var move string
		err := s.Repo.Pool.QueryRow(ctx, `
			SELECT claim_id, (SELECT move FROM ledger l2 WHERE l2.claim_id = l.claim_id ORDER BY id DESC LIMIT 1)
			FROM ledger l WHERE id = $1`, id).Scan(&claimID, &move)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && subject.Concluded(move)) {
			res := rejected(subj.String(), "BASIS_NOT_FOUND", "basis "+subject.FormatID(id)+" names no claim that stands")
			return &res, nil
		}
		if err != nil {
			return nil, err
		}
		refs = append(refs, claimID)
	}
	basis.Refs = refs
	return nil, nil
}

// GET /principles — the Principle List as it stands (tk:read).
func (s *Server) getPrinciples(w http.ResponseWriter, r *http.Request) {
	ps, err := s.currentPrinciples(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principles": ps, "count": len(ps)})
}
