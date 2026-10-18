// Package eff implements the three predicates and eff(). It is the single
// source of truth for this logic. Nothing here is cached in the database;
// every call reads the ledger fresh.
package eff

import (
	"context"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/ledger"
)

// Flag kind codes, matching the flags.kind vocabulary.
// tk-checker maps a failing Result straight to one of these when writing flags.
const (
	CodeUnanchored      = "unanchored"        // ¬is_truth: no valid basis, or an unresolvable structure
	CodePremiseDemoted  = "premise_demoted"   // ¬is_truth: a ref this claim depends on is no longer eff=T
	CodeExpiredBasis    = "expired_basis"     // ¬is_truth: fact basis past valid_until
	CodeUnsigned        = "unsigned"          // ¬is_trust: missing signer, or signer is not a person
	CodeSelfSignedAgent = "self_signed_agent" // ¬is_trust: agent producer signed its own row
	CodeBrokenChain     = "broken_chain"      // ¬is_traceable: a ref doesn't exist
	CodeUnjustifiedRow  = "unjustified_row"   // ¬is_traceable: row is in V(w) per CJCM
)

// Result is what each predicate returns: {ok, failing_id, reason},
// plus a Code identifying which flags.kind a failure maps to (empty when OK,
// or when the failure isn't itself flag-worthy — e.g. a recursive "claim not
// found" during ref resolution, which surfaces as broken_chain at the caller).
type Result struct {
	OK        bool
	FailingID *int64
	Code      string
	Reason    string
}

func pass() Result { return Result{OK: true} }

func fail(failingID int64, code, reason string) Result {
	id := failingID
	return Result{OK: false, FailingID: &id, Code: code, Reason: reason}
}

// Evaluator holds what the predicates need beyond the ledger itself: the
// agents list (config.yaml — producer names that are machines, not people).
type Evaluator struct {
	Repo   *ledger.Repo
	Agents map[string]bool
}

func New(repo *ledger.Repo, agents map[string]bool) *Evaluator {
	return &Evaluator{Repo: repo, Agents: agents}
}

// Eff computes eff(c) = is_truth ∧ is_trust ∧ is_traceable. Cuts cycles with a
// visited set (Inv5 should prevent them; this still cuts rather than loops).
func (e *Evaluator) Eff(ctx context.Context, claimID int64) (bool, error) {
	cache := map[int64]bool{}
	visiting := map[int64]bool{}
	return e.effRec(ctx, claimID, cache, visiting)
}

func (e *Evaluator) effRec(ctx context.Context, claimID int64, cache, visiting map[int64]bool) (bool, error) {
	if v, ok := cache[claimID]; ok {
		return v, nil
	}
	if visiting[claimID] {
		return false, nil
	}
	visiting[claimID] = true
	defer delete(visiting, claimID)

	truth, err := e.isTruthRec(ctx, claimID, cache, visiting)
	if err != nil {
		return false, err
	}
	if !truth.OK {
		cache[claimID] = false
		return false, nil
	}

	trust, err := e.IsTrust(ctx, claimID)
	if err != nil {
		return false, err
	}
	if !trust.OK {
		cache[claimID] = false
		return false, nil
	}

	traceable, err := e.IsTraceable(ctx, claimID)
	if err != nil {
		return false, err
	}
	cache[claimID] = traceable.OK
	return traceable.OK, nil
}

// IsTruth evaluates is_truth(current(claimID)).
func (e *Evaluator) IsTruth(ctx context.Context, claimID int64) (Result, error) {
	return e.isTruthRec(ctx, claimID, map[int64]bool{}, map[int64]bool{})
}

func (e *Evaluator) isTruthRec(ctx context.Context, claimID int64, cache, visiting map[int64]bool) (Result, error) {
	cur, found, err := e.Repo.Current(ctx, claimID)
	if err != nil {
		return Result{}, err
	}
	if !found {
		return fail(claimID, CodeBrokenChain, "claim not found"), nil
	}
	if cur.Mark != "T" {
		return fail(claimID, CodeUnanchored, "mark is not T"), nil
	}
	if cur.Basis == nil {
		return fail(claimID, CodeUnanchored, "mark=T without basis"), nil
	}

	switch cur.Basis.Kind {
	case "principle":
		// refs=[] is the principle's own definition row (POST /principles)
		// — axiomatic, self-justifying, nothing further to check here. refs=[p] is
		// an ordinary claim anchored to principle p, which must itself resolve.
		if len(cur.Basis.Refs) == 0 {
			return pass(), nil
		}
		if len(cur.Basis.Refs) != 1 {
			return fail(claimID, CodeUnanchored, "principle basis must have zero refs (a principle itself) or exactly one (anchored to a principle)"), nil
		}
		p := cur.Basis.Refs[0]
		pCur, found, err := e.Repo.Current(ctx, p)
		if err != nil {
			return Result{}, err
		}
		if !found || pCur.Basis == nil || pCur.Basis.Kind != "principle" {
			return fail(p, CodeUnanchored, "referenced claim's basis.kind is not principle"), nil
		}
		effP, err := e.effRec(ctx, p, cache, visiting)
		if err != nil {
			return Result{}, err
		}
		if !effP {
			return fail(p, CodePremiseDemoted, "principle ref is not eff=T"), nil
		}
		return pass(), nil

	case "fact":
		if cur.Basis.Note == "" {
			return fail(claimID, CodeUnanchored, "fact basis missing note"), nil
		}
		if cur.Basis.ValidUntil != nil && cur.Basis.ValidUntil.Before(time.Now()) {
			return fail(claimID, CodeExpiredBasis, "fact basis expired (valid_until passed)"), nil
		}
		return pass(), nil

	case "derivation":
		if len(cur.Basis.Refs) == 0 {
			return fail(claimID, CodeUnanchored, "derivation basis has no refs"), nil
		}
		for _, r := range cur.Basis.Refs {
			effR, err := e.effRec(ctx, r, cache, visiting)
			if err != nil {
				return Result{}, err
			}
			if !effR {
				return fail(r, CodePremiseDemoted, "derivation ref is not eff=T"), nil
			}
		}
		return pass(), nil

	default:
		return fail(claimID, CodeUnanchored, "unknown basis.kind: "+cur.Basis.Kind), nil
	}
}

// IsTrust evaluates is_trust over history(claimID): every non-propose row
// must have a human signer, and an agent producer must not sign its own row.
func (e *Evaluator) IsTrust(ctx context.Context, claimID int64) (Result, error) {
	hist, err := e.Repo.History(ctx, claimID)
	if err != nil {
		return Result{}, err
	}
	for _, row := range hist {
		if row.Move == "propose" {
			continue
		}
		if row.Signer == nil || *row.Signer == "" {
			return fail(row.ID, CodeUnsigned, "row missing signer"), nil
		}
		if e.Agents[row.Producer] && *row.Signer == row.Producer {
			return fail(row.ID, CodeSelfSignedAgent, "agent producer signed its own row"), nil
		}
		if e.Agents[*row.Signer] {
			return fail(row.ID, CodeUnsigned, "signer is an agent, not a person"), nil
		}
	}
	return pass(), nil
}

// IsTraceable evaluates is_traceable: every ref in the current basis (walked
// recursively) must point at a claim that exists, and no non-propose row in
// history(claimID) may be in V(w) — read from violations_seen, which the
// checker fills from the CJCM stream.
func (e *Evaluator) IsTraceable(ctx context.Context, claimID int64) (Result, error) {
	hist, err := e.Repo.History(ctx, claimID)
	if err != nil {
		return Result{}, err
	}
	if len(hist) == 0 {
		return fail(claimID, CodeBrokenChain, "claim not found"), nil
	}

	cur := hist[len(hist)-1]
	if cur.Basis != nil {
		for _, r := range cur.Basis.Refs {
			existsOK, missing, err := e.refsExistRecursive(ctx, r, map[int64]bool{})
			if err != nil {
				return Result{}, err
			}
			if !existsOK {
				return fail(missing, CodeBrokenChain, "ref does not exist"), nil
			}
		}
	}

	for _, row := range hist {
		if row.Move == "propose" {
			continue
		}
		inV, err := e.inViolations(ctx, row.ID)
		if err != nil {
			return Result{}, err
		}
		if inV {
			return fail(row.ID, CodeUnjustifiedRow, "row is in V(w) — unjustified insert, update, or delete"), nil
		}
	}
	return pass(), nil
}

func (e *Evaluator) refsExistRecursive(ctx context.Context, claimID int64, visited map[int64]bool) (ok bool, missing int64, err error) {
	if visited[claimID] {
		return true, 0, nil
	}
	visited[claimID] = true

	cur, found, err := e.Repo.Current(ctx, claimID)
	if err != nil {
		return false, 0, err
	}
	if !found {
		return false, claimID, nil
	}
	if cur.Basis == nil {
		return true, 0, nil
	}
	for _, r := range cur.Basis.Refs {
		exists, missingID, err := e.refsExistRecursive(ctx, r, visited)
		if err != nil {
			return false, 0, err
		}
		if !exists {
			return false, missingID, nil
		}
	}
	return true, 0, nil
}

func (e *Evaluator) inViolations(ctx context.Context, rowID int64) (bool, error) {
	var exists bool
	err := e.Repo.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM violations_seen WHERE row_id = $1)`, rowID).Scan(&exists)
	return exists, err
}
