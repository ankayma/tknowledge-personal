package tka

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------- principle ----------

type Principle struct {
	Code      string    `json:"code"`
	Statement string    `json:"statement"`
	Note      *string   `json:"note,omitempty"`
	Status    string    `json:"status"`
	Signer    string    `json:"signer"`
	Producer  string    `json:"producer"`
	CreatedAt time.Time `json:"created_at"`
}

// AddPrinciple writes one axiom. A code already taken is refused: principles
// are added, never rewritten — rewriting one pulls down every T above it.
func (r *Repo) AddPrinciple(ctx context.Context, a Actor, rawCode, statement, note string) (Principle, error) {
	if err := r.checkSigner(a); err != nil {
		return Principle{}, err
	}
	code, ok := NormalizeCode(rawCode)
	if !ok {
		return Principle{}, fail("INVALID_CODE", "code must be P.<n>, got %q", rawCode)
	}
	if strings.TrimSpace(statement) == "" {
		return Principle{}, fail("EMPTY_STATEMENT", "%s: statement is required", code)
	}
	var n *string
	if strings.TrimSpace(note) != "" {
		n = &note
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return Principle{}, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return Principle{}, err
	}

	if err := r.writable(ctx, tx, a); err != nil {
		return Principle{}, err
	}

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM principle WHERE tenant_id = $1 AND code = $2)`,
		a.Tenant(), code).Scan(&exists); err != nil {
		return Principle{}, err
	}
	if exists {
		return Principle{}, fail("PRINCIPLE_EXISTS", "%s already exists; a principle is never rewritten (retire it and add a new code)", code)
	}
	p := Principle{Code: code, Statement: statement, Note: n, Status: "active", Signer: a.UserID, Producer: a.ActorID}
	if err := tx.QueryRow(ctx, `
		INSERT INTO principle (tenant_id, code, statement, note, signer, producer, grant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		a.Tenant(), code, statement, n, a.UserID, a.ActorID, a.GrantID).Scan(&p.CreatedAt); err != nil {
		return Principle{}, err
	}
	if err := addEvent(ctx, tx, a.Tenant(), code, "created", a.UserID, ""); err != nil {
		return Principle{}, err
	}
	return p, tx.Commit(ctx)
}

// RetirePrinciple drops an axiom and returns every T resting on it — the part
// of the store that now has to be read again. The owner sees the price first.
func (r *Repo) RetirePrinciple(ctx context.Context, a Actor, rawCode, reason string) (Principle, []string, error) {
	if err := r.checkSigner(a); err != nil {
		return Principle{}, nil, err
	}
	code, ok := NormalizeCode(rawCode)
	if !ok {
		return Principle{}, nil, fail("INVALID_CODE", "code must be P.<n>, got %q", rawCode)
	}
	if strings.TrimSpace(reason) == "" {
		return Principle{}, nil, fail("EMPTY_REASON", "retiring a principle requires a reason")
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return Principle{}, nil, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return Principle{}, nil, err
	}

	if err := r.writable(ctx, tx, a); err != nil {
		return Principle{}, nil, err
	}

	p, err := readPrinciple(ctx, tx, a.Tenant(), code)
	if err != nil {
		return Principle{}, nil, err
	}
	if p.Status == "retired" {
		return Principle{}, nil, fail("ALREADY_RETIRED", "%s is already retired", code)
	}
	affected, err := standingOn(ctx, tx, a.Tenant(), code)
	if err != nil {
		return Principle{}, nil, err
	}
	if err := addEvent(ctx, tx, a.Tenant(), code, "retired", a.UserID, reason); err != nil {
		return Principle{}, nil, err
	}
	p.Status = "retired"
	if err := tx.Commit(ctx); err != nil {
		return Principle{}, nil, err
	}
	if len(affected) > 0 {
		r.step(ctx, a.Tenant(), StepAct2) // a principle fell and took claims with it
	}
	return p, affected, nil
}

// Principles lists the axioms, retired ones included when asked.
func (r *Repo) Principles(ctx context.Context, tenant string, includeRetired bool) ([]Principle, error) {
	kept := []Principle{}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT code, statement, note, signer, producer, created_at FROM principle
			WHERE tenant_id = $1 ORDER BY (regexp_replace(code, '^P\.', ''))::int`, TenantOr(tenant))
		if err != nil {
			return err
		}
		out := []Principle{}
		for rows.Next() {
			var p Principle
			if err := rows.Scan(&p.Code, &p.Statement, &p.Note, &p.Signer, &p.Producer, &p.CreatedAt); err != nil {
				rows.Close()
				return err
			}
			out = append(out, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range out {
			events, err := history(ctx, q, tenant, p.Code)
			if err != nil {
				return err
			}
			p.Status = status("principle", events)
			if p.Status == "retired" && !includeRetired {
				continue
			}
			kept = append(kept, p)
		}
		return nil
	})
	return kept, err
}

func readPrinciple(ctx context.Context, q querier, tenant, code string) (Principle, error) {
	var p Principle
	err := q.QueryRow(ctx, `SELECT code, statement, note, signer, producer, created_at FROM principle
		WHERE tenant_id = $1 AND code = $2`, TenantOr(tenant), code).
		Scan(&p.Code, &p.Statement, &p.Note, &p.Signer, &p.Producer, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principle{}, fail("PRINCIPLE_UNKNOWN", "%s is not in the Principle List", code)
	}
	if err != nil {
		return Principle{}, err
	}
	events, err := history(ctx, q, tenant, code)
	if err != nil {
		return Principle{}, err
	}
	p.Status = status("principle", events)
	return p, nil
}

// standingOn lists the T entries that name this id among their premises.
func standingOn(ctx context.Context, q querier, tenant, id string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id FROM truth_ledger
		WHERE tenant_id = $1 AND $2 = ANY(premises) ORDER BY num`, TenantOr(tenant), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
