package tka

import (
	"context"
	"strings"
)

// The three named acts that lower something. None of them deletes: each is a
// line in entry_history, and the entry keeps its id and its wording (D-07
// R3.3). A fact carries no chain to re-derive, so what lowers it comes from
// outside — a better source, a counter-argument (P.9, clarified 19/9).

// Demote turns a [T] back into an assumption's standing: the claim stays, the
// label falls. `contradictedBy` names the outside source when there is one.
func (r *Repo) Demote(ctx context.Context, a Actor, id, reason, contradictedBy string) (Truth, error) {
	if err := r.checkSigner(a); err != nil {
		return Truth{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Truth{}, fail("EMPTY_REASON", "demoting a [T] requires a reason")
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return Truth{}, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return Truth{}, err
	}

	if err := r.writable(ctx, tx, a); err != nil {
		return Truth{}, err
	}

	t, err := r.ReadTruth(ctx, tx, a.Tenant(), strings.ToUpper(strings.TrimSpace(id)))
	if err != nil {
		return Truth{}, err
	}
	if t.Status != "T" {
		return Truth{}, fail("NOT_T", "%s is %s", t.ID, t.Status)
	}
	if src := strings.TrimSpace(contradictedBy); src != "" {
		if err := addEvent(ctx, tx, a.Tenant(), t.ID, "contradicted_by", a.UserID, src); err != nil {
			return Truth{}, err
		}
	}
	if err := addEvent(ctx, tx, a.Tenant(), t.ID, "demoted", a.UserID, reason); err != nil {
		return Truth{}, err
	}
	out, err := r.ReadTruth(ctx, tx, a.Tenant(), t.ID)
	if err != nil {
		return Truth{}, err
	}
	return out, tx.Commit(ctx)
}

// Retire closes an entry for good: it stops being live, and stays readable so
// everything that ever rested on it can still be traced.
func (r *Repo) Retire(ctx context.Context, a Actor, id, reason string) (Entry, []string, error) {
	if err := r.checkSigner(a); err != nil {
		return Entry{}, nil, err
	}
	if strings.TrimSpace(reason) == "" {
		return Entry{}, nil, fail("EMPTY_REASON", "retiring an entry requires a reason")
	}
	entry, err := r.Read(ctx, a.Tenant(), id)
	if err != nil {
		return Entry{}, nil, err
	}
	var entryID, current string
	switch entry.Kind {
	case "truth":
		entryID, current = entry.Truth.ID, entry.Truth.Status
	case "assumption":
		entryID, current = entry.Assumption.ID, entry.Assumption.Status
	default:
		return Entry{}, nil, fail("USE_PRINCIPLE_RETIRE", "a principle is retired with principle_retire")
	}
	if current == "retired" {
		return Entry{}, nil, fail("ALREADY_RETIRED", "%s is already retired", entryID)
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return Entry{}, nil, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return Entry{}, nil, err
	}

	if err := r.writable(ctx, tx, a); err != nil {
		return Entry{}, nil, err
	}

	affected, err := standingOn(ctx, tx, a.Tenant(), entryID)
	if err != nil {
		return Entry{}, nil, err
	}
	if err := addEvent(ctx, tx, a.Tenant(), entryID, "retired", a.UserID, reason); err != nil {
		return Entry{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Entry{}, nil, err
	}
	out, err := r.Read(ctx, a.Tenant(), entryID)
	return out, affected, err
}

// Recheck records that a person went back to the source and it still holds.
// Only a fact has anywhere to go back to; a derivation is re-derived instead,
// which is a reading, not a write.
func (r *Repo) Recheck(ctx context.Context, a Actor, id, note string) (Truth, error) {
	if err := r.checkSigner(a); err != nil {
		return Truth{}, err
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return Truth{}, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return Truth{}, err
	}

	if err := r.writable(ctx, tx, a); err != nil {
		return Truth{}, err
	}

	t, err := r.ReadTruth(ctx, tx, a.Tenant(), strings.ToUpper(strings.TrimSpace(id)))
	if err != nil {
		return Truth{}, err
	}
	if t.Basis != "fact" {
		return Truth{}, fail("NOT_A_FACT", "%s rests on a derivation: re-derive its chain instead of rechecking a source", t.ID)
	}
	if t.Status != "T" {
		return Truth{}, fail("NOT_T", "%s is %s", t.ID, t.Status)
	}
	if err := addEvent(ctx, tx, a.Tenant(), t.ID, "rechecked", a.UserID, strings.TrimSpace(note)); err != nil {
		return Truth{}, err
	}
	out, err := r.ReadTruth(ctx, tx, a.Tenant(), t.ID)
	if err != nil {
		return Truth{}, err
	}
	return out, tx.Commit(ctx)
}
