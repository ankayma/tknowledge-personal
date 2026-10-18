package tka

import (
	"context"
	"errors"
	"strings"
)

type RefuteInput struct {
	ID       string `json:"id"`
	Observed string `json:"observed"`
	Evidence string `json:"evidence"`
}

// Refute closes an A-p whose not: has been observed (owner
// decision, 1/10/2026). It is the opposite of promote and just as narrow:
// only an open A-p carries a not:, so only an open A-p is refuted. A claim in
// the ledger falls with demote; A-r, A-c and A-b close their own ways.
//
// The wording is not carried anywhere — a refuted assumption was wrong as
// written, and a later claim that says what did happen is admitted on its own.
// Evidence, when it names an entry or a document, must be in this store.
func (r *Repo) Refute(ctx context.Context, a Actor, in RefuteInput) (Assumption, error) {
	if err := r.checkSigner(a); err != nil {
		return Assumption{}, err
	}
	observed := strings.TrimSpace(in.Observed)
	if observed == "" {
		return Assumption{}, fail("EMPTY_OBSERVATION", "refuting needs what was observed")
	}
	var out Assumption
	err := r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		id := CanonID(in.ID)
		if strings.HasPrefix(id, "T-") {
			return fail("NOT_REFUTABLE", "%s is a claim in the ledger, not an assumption", id)
		}
		cur, err := r.ReadAssumption(ctx, q, a.Tenant(), id)
		if err != nil {
			return err
		}
		if cur.Type != "A-p" {
			return fail("NOT_REFUTABLE", "%s is %s: it has no not: to observe", cur.ID, cur.Type)
		}
		if cur.Status != "open" {
			return fail("NOT_REFUTABLE", "%s is %s", cur.ID, cur.Status)
		}
		note := "observed: " + observed
		if ev := strings.TrimSpace(in.Evidence); ev != "" {
			if looksLikeID(ev) {
				canon, _, err := resolveTarget(ctx, q, a.Tenant(), ev)
				if err != nil {
					var e *Error
					if errors.As(err, &e) {
						return failNext("NOT_FOUND", "Check the id with entry(...) or documents(); evidence named by id must be in this store.",
							"%s is not in this store", ev)
					}
					return err
				}
				ev = canon
			}
			note += " · evidence: " + ev
		}
		if err := addEvent(ctx, q, a.Tenant(), cur.ID, "refuted", a.UserID, note); err != nil {
			return err
		}
		out, err = r.ReadAssumption(ctx, q, a.Tenant(), cur.ID)
		return err
	})
	return out, err
}

// looksLikeID is whether evidence is written as an id the store would hold
// (T-…, A-…, D-…, TH-…), rather than words or a link.
func looksLikeID(s string) bool {
	if _, ok := CanonThreadID(s); ok {
		return true
	}
	return entryIDPattern.MatchString(strings.ToUpper(strings.TrimSpace(s)))
}
