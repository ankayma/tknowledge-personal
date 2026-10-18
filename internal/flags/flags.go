// Package flags reads and writes the `flags` table. Only
// tk-checker writes — tk-api only reads (grants in migrations/0001_init.sql).
package flags

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Flag struct {
	ID       int64
	ClaimID  int64
	Kind     string
	Evidence []byte
	ToSigner *string
	TS       time.Time
	ClosedBy *int64
}

type Repo struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repo {
	return &Repo{Pool: pool}
}

const selectColumns = `id, claim_id, kind, evidence, to_signer, ts, closed_by`

func scan(rows interface {
	Scan(dest ...any) error
}) (Flag, error) {
	var f Flag
	err := rows.Scan(&f.ID, &f.ClaimID, &f.Kind, &f.Evidence, &f.ToSigner, &f.TS, &f.ClosedBy)
	return f, err
}

// List returns flags, newest first, optionally filtered to open-only
// (closed_by IS NULL) and/or a specific to_signer — GET /flags?open=&to=.
func (r *Repo) List(ctx context.Context, openOnly bool, toSigner string) ([]Flag, error) {
	q := `SELECT ` + selectColumns + ` FROM flags WHERE true`
	var args []any
	if openOnly {
		q += ` AND closed_by IS NULL`
	}
	if toSigner != "" {
		args = append(args, toSigner)
		q += fmt.Sprintf(` AND to_signer = $%d`, len(args))
	}
	q += ` ORDER BY ts DESC`

	rows, err := r.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Flag
	for rows.Next() {
		f, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ForClaim returns the open flags for one claim — used by GET /claims/{id}/audit.
func (r *Repo) ForClaim(ctx context.Context, claimID int64) ([]Flag, error) {
	rows, err := r.Pool.Query(ctx,
		`SELECT `+selectColumns+` FROM flags WHERE claim_id = $1 AND closed_by IS NULL ORDER BY ts DESC`, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Flag
	for rows.Next() {
		f, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// HasOpen reports whether an open flag already exists for (claimID, kind,
// failingID): the sweep's dedup rule. failingID may be nil.
func (r *Repo) HasOpen(ctx context.Context, claimID int64, kind string, failingID *int64) (bool, error) {
	var exists bool
	err := r.Pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM flags
			WHERE claim_id = $1 AND kind = $2 AND closed_by IS NULL
			  AND (evidence->>'failing_id')::bigint IS NOT DISTINCT FROM $3
		)`, claimID, kind, failingID).Scan(&exists)
	return exists, err
}

// Open writes a new flag unless an open one already matches (claim_id, kind,
// failing_id) — checker calls this once per predicate failure per sweep, and
// this is what keeps repeated sweeps from spamming duplicate flags.
func (r *Repo) Open(ctx context.Context, claimID int64, kind string, failingID *int64, reason string, toSigner *string) error {
	already, err := r.HasOpen(ctx, claimID, kind, failingID)
	if err != nil {
		return err
	}
	if already {
		return nil
	}
	evidence, err := json.Marshal(map[string]any{"failing_id": failingID, "reason": reason})
	if err != nil {
		return err
	}
	_, err = r.Pool.Exec(ctx,
		`INSERT INTO flags (claim_id, kind, evidence, to_signer) VALUES ($1,$2,$3,$4)`,
		claimID, kind, evidence, toSigner)
	return err
}

// CloseAll closes every open flag on claimID, stamping closedBy — the row id
// of the reapprove/reject/refute move that answered them.
func (r *Repo) CloseAll(ctx context.Context, claimID int64, closedBy int64) error {
	_, err := r.Pool.Exec(ctx, `UPDATE flags SET closed_by = $1 WHERE claim_id = $2 AND closed_by IS NULL`, closedBy, claimID)
	return err
}
