package tka

import (
	"context"
	"strings"
)

// Reading the store back. A [T] is only worth its label while the things under
// it still stand, so every read of a truth entry also reports whether any of
// its premises has fallen. The machine never lowers the entry by itself —
// demoting is a named act with a reason (D-07 H.2 part 3) — but it refuses
// to let a fallen premise pass unseen.

// Entry is whatever the id names: a principle, a truth entry, an assumption.
type Entry struct {
	Kind       string      `json:"kind"` // principle | truth | assumption
	Principle  *Principle  `json:"principle,omitempty"`
	Truth      *Truth      `json:"truth,omitempty"`
	Assumption *Assumption `json:"assumption,omitempty"`
	Document   *Document   `json:"document,omitempty"`
	Summary    *Summary    `json:"summary,omitempty"`
	Case       *Case       `json:"case,omitempty"`
}

// Read loads one entry by id: P.x, T-…, or A-….
func (r *Repo) Read(ctx context.Context, tenant, id string) (Entry, error) {
	id = strings.TrimSpace(id)
	var out Entry
	err := r.withTenant(ctx, tenant, func(q querier) error {
		switch {
		case strings.HasPrefix(strings.ToUpper(id), "T-"):
			t, err := r.ReadTruth(ctx, q, tenant, strings.ToUpper(id))
			if err != nil {
				return err
			}
			out = Entry{Kind: "truth", Truth: &t}
			return nil
		case strings.HasPrefix(strings.ToUpper(id), "A-"):
			a, err := r.ReadAssumption(ctx, q, tenant, strings.ToUpper(id))
			if err != nil {
				return err
			}
			out = Entry{Kind: "assumption", Assumption: &a}
			return nil
		case strings.HasPrefix(strings.ToUpper(id), "D-"):
			d, err := readDocument(ctx, q, tenant, id)
			if err != nil {
				return err
			}
			if d.EvidenceFor, err = evidenceFor(ctx, q, tenant, d.ID); err != nil {
				return err
			}
			if d.Flags, err = openFlagsOn(ctx, q, tenant, d.ID); err != nil {
				return err
			}
			out = Entry{Kind: "document", Document: &d}
			return nil
		case strings.HasPrefix(strings.ToUpper(id), "C-"):
			ref, ok := canonCase(id)
			if !ok {
				return fail("CASE_UNKNOWN", "%s is not a Case id (C-…)", id)
			}
			c, err := readCase(ctx, q, tenant, ref)
			if err != nil {
				return err
			}
			out = Entry{Kind: "case", Case: &c}
			return nil
		case strings.HasPrefix(strings.ToUpper(id), "S-"):
			s, err := r.readSummary(ctx, q, tenant, id)
			if err != nil {
				return err
			}
			out = Entry{Kind: "summary", Summary: &s}
			return nil
		}
		code, ok := NormalizeCode(id)
		if !ok {
			return fail("NOT_FOUND", "%q is not an id in this store (P.x, T-…, A-…, D-…)", id)
		}
		p, err := readPrinciple(ctx, q, tenant, code)
		if err != nil {
			return err
		}
		out = Entry{Kind: "principle", Principle: &p}
		return nil
	})
	return out, err
}

// OpenAssumptions lists what is still waiting, newest first. `kind` filters by
// type (A-p, A-b …) and is optional.
func (r *Repo) OpenAssumptions(ctx context.Context, tenant, kind string) ([]Assumption, error) {
	sql := `SELECT ` + assumptionColumns + ` FROM assumption WHERE tenant_id = $1`
	args := []any{TenantOr(tenant)}
	if k := strings.TrimSpace(kind); k != "" {
		typ, ok := normalizeType(k)
		if !ok {
			return nil, fail("INVALID_TYPE", "type must be one of %s", strings.Join(assumptionTypes, ", "))
		}
		sql += ` AND type = $2`
		args = append(args, typ)
	}
	sql += ` ORDER BY num DESC`
	out := []Assumption{}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		all := []Assumption{}
		for rows.Next() {
			a, err := scanAssumption(rows)
			if err != nil {
				rows.Close()
				return err
			}
			all = append(all, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range all {
			events, err := history(ctx, q, tenant, a.ID)
			if err != nil {
				return err
			}
			if status("assumption", events) != "open" {
				continue
			}
			a.History, a.Status = events, "open"
			out = append(out, a)
		}
		return nil
	})
	return out, err
}

// Truths lists the ledger, newest first. `standing` keeps only entries that
// still hold (status T and no fallen premise).
func (r *Repo) Truths(ctx context.Context, tenant string, standing bool, limit int) ([]Truth, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []Truth{}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+truthColumns+` FROM truth_ledger
			WHERE tenant_id = $1 ORDER BY num DESC LIMIT $2`, TenantOr(tenant), limit)
		if err != nil {
			return err
		}
		all := []Truth{}
		for rows.Next() {
			t, err := scanTruth(rows)
			if err != nil {
				rows.Close()
				return err
			}
			all = append(all, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, t := range all {
			full, err := r.ReadTruth(ctx, q, tenant, t.ID)
			if err != nil {
				return err
			}
			if standing && full.Effective != "T" {
				continue
			}
			out = append(out, full)
		}
		return nil
	})
	return out, err
}

// ChainNode is one link under a claim: what it is, and whether it still stands.
type ChainNode struct {
	ID       string      `json:"id"`
	Kind     string      `json:"kind"` // principle | truth
	Claim    string      `json:"claim"`
	Status   string      `json:"status"`
	Stands   bool        `json:"stands"`
	Premises []ChainNode `json:"premises,omitempty"`
}

const chainDepth = 8

// Chain walks a truth entry down to the principles and facts it rests on. A
// fact or a principle is a leaf: P.9 gives it no chain to audit, so the walk
// stops there and the checking moves outside the system.
func (r *Repo) Chain(ctx context.Context, tenant, id string) (Truth, []ChainNode, error) {
	var t Truth
	var nodes []ChainNode
	err := r.withTenant(ctx, tenant, func(q querier) error {
		var err error
		if t, err = r.ReadTruth(ctx, q, tenant, strings.ToUpper(strings.TrimSpace(id))); err != nil {
			return err
		}
		seen := map[string]bool{t.ID: true}
		nodes, err = r.walk(ctx, q, tenant, t.Premises, seen, 0)
		return err
	})
	return t, nodes, err
}

func (r *Repo) walk(ctx context.Context, q querier, tenant string, ids []string, seen map[string]bool, depth int) ([]ChainNode, error) {
	out := []ChainNode{}
	if depth >= chainDepth {
		return out, nil
	}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if code, ok := NormalizeCode(id); ok {
			p, err := readPrinciple(ctx, q, tenant, code)
			if err != nil {
				out = append(out, ChainNode{ID: id, Kind: "principle", Status: "missing", Stands: false})
				continue
			}
			out = append(out, ChainNode{ID: p.Code, Kind: "principle", Claim: p.Statement,
				Status: p.Status, Stands: p.Status == "active"})
			continue
		}
		t, err := r.ReadTruth(ctx, q, tenant, id)
		if err != nil {
			out = append(out, ChainNode{ID: id, Kind: "truth", Status: "missing", Stands: false})
			continue
		}
		node := ChainNode{ID: t.ID, Kind: "truth", Claim: t.Claim, Status: t.Effective, Stands: t.Effective == "T"}
		if node.Premises, err = r.walk(ctx, q, tenant, t.Premises, seen, depth+1); err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}

// premisesFell names the immediate premises that no longer stand. One level is
// enough at read time: a premise that lost its own premise is already not T,
// so the flag travels up the chain one read at a time.
func (r *Repo) premisesFell(ctx context.Context, q querier, tenant string, premises []string) ([]string, error) {
	fell := []string{}
	for _, id := range premises {
		if code, ok := NormalizeCode(id); ok {
			p, err := readPrinciple(ctx, q, tenant, code)
			if err != nil || p.Status != "active" {
				fell = append(fell, id)
			}
			continue
		}
		var mark string
		events, err := history(ctx, q, tenant, id)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			fell = append(fell, id)
			continue
		}
		mark = status("truth", events)
		if mark != "T" {
			fell = append(fell, id)
		}
	}
	return fell, nil
}
