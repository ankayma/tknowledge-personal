// Package docs is the versioned document store: insert-only,
// one version chain per normalised title. The repo never updates or deletes;
// the table's trigger refuses both anyway.
package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ankayma/tknowledge-personal/internal/subject"
)

// Kinds a document may have. `post` exists only through posted (phase C).
var Kinds = []string{"plan", "spec", "analysis", "assessment", "rule", "post"}

type Doc struct {
	ID         int64
	Title      string
	TitleKey   string
	Kind       string
	Version    int
	Body       string // empty in Find results
	BodySHA256 string
	Refs       []string
	Supersedes *int64
	UserID     string
	ActorID    string
	GrantID    string
	Scope      *string
	TS         time.Time
}

type Repo struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repo { return &Repo{Pool: pool} }

// TitleKey is the version-chain key: "Plan MVP  T-Knowledge" and
// "plan-mvp-t-knowledge" are the same document.
func TitleKey(title string) string { return subject.Slug(title) }

const columns = `id, title, title_key, kind, version, body, body_sha256, refs, supersedes, user_id, actor_id, grant_id, scope, ts`

func scan(row pgx.Row) (Doc, error) {
	var d Doc
	err := row.Scan(&d.ID, &d.Title, &d.TitleKey, &d.Kind, &d.Version, &d.Body, &d.BodySHA256, &d.Refs,
		&d.Supersedes, &d.UserID, &d.ActorID, &d.GrantID, &d.Scope, &d.TS)
	return d, err
}

func (r *Repo) Get(ctx context.Context, id int64) (Doc, bool, error) {
	d, err := scan(r.Pool.QueryRow(ctx, `SELECT `+columns+` FROM docs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Doc{}, false, nil
	}
	return d, err == nil, err
}

func (r *Repo) Exists(ctx context.Context, id int64) (bool, error) {
	var ok bool
	err := r.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM docs WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

type StoreParams struct {
	Title, Kind, Body        string
	Refs                     []string // already normalised, sorted, unique
	UserID, ActorID, GrantID string
	Scope                    string
}

type StoreResult struct {
	Outcome  string // new | supersedes | duplicate
	Doc      Doc
	Previous *Doc
}

// Store writes a new version unless the newest version of the title already
// has the same kind, body and refs (duplicate). Serialised per title.
func (r *Repo) Store(ctx context.Context, p StoreParams) (StoreResult, error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return StoreResult{}, err
	}
	defer tx.Rollback(ctx)
	res, err := StoreTx(ctx, tx, p)
	if err != nil {
		return StoreResult{}, err
	}
	return res, tx.Commit(ctx)
}

// StoreTx is Store inside a caller's transaction (posted writes a document,
// a thread and a claim atomically).
func StoreTx(ctx context.Context, tx pgx.Tx, p StoreParams) (StoreResult, error) {
	key := TitleKey(p.Title)
	sum := sha256.Sum256([]byte(p.Body))
	sha := hex.EncodeToString(sum[:])
	refs := p.Refs
	if refs == nil {
		refs = []string{}
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "docs.title:"+key); err != nil {
		return StoreResult{}, err
	}
	latest, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM docs WHERE title_key = $1 ORDER BY version DESC LIMIT 1`, key))
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return StoreResult{}, err
	}
	if found && latest.BodySHA256 == sha && latest.Kind == p.Kind && slices.Equal(latest.Refs, refs) {
		return StoreResult{Outcome: "duplicate", Doc: latest}, nil
	}

	version := 1
	var supersedes *int64
	if found {
		version = latest.Version + 1
		supersedes = &latest.ID
	}
	d, err := scan(tx.QueryRow(ctx, `
		INSERT INTO docs (title, title_key, kind, version, body, body_sha256, refs, supersedes, user_id, actor_id, grant_id, scope)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+columns,
		strings.TrimSpace(p.Title), key, p.Kind, version, p.Body, sha, refs, supersedes, p.UserID, p.ActorID, p.GrantID, p.Scope))
	if err != nil {
		return StoreResult{}, err
	}
	res := StoreResult{Outcome: "new", Doc: d}
	if found {
		res.Outcome, res.Previous = "supersedes", &latest
	}
	return res, nil
}

type FindParams struct {
	Query      string
	Kind       string
	Since      *time.Time
	LatestOnly bool
	Ref        string // only documents whose refs contain this subject or slug
	Limit      int
}

// Find matches title and body case-insensitively (simple full text, spec
// §5.7 [A]); metadata only — Body is left empty.
func (r *Repo) Find(ctx context.Context, p FindParams) ([]Doc, error) {
	q := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(p.Query))
	var since any
	if p.Since != nil {
		since = *p.Since
	}
	rows, err := r.Pool.Query(ctx, `
		SELECT id, title, title_key, kind, version, '' AS body, body_sha256, refs, supersedes, user_id, actor_id, grant_id, scope, ts
		FROM docs d
		WHERE ($1 = '' OR d.title ILIKE '%' || $1 || '%' OR d.body ILIKE '%' || $1 || '%')
		  AND ($2 = '' OR d.kind = $2)
		  AND ($3::timestamptz IS NULL OR d.ts >= $3::timestamptz)
		  AND (NOT $4 OR d.version = (SELECT max(x.version) FROM docs x WHERE x.title_key = d.title_key))
		  AND ($6 = '' OR $6 = ANY(d.refs))
		ORDER BY d.ts DESC, d.id DESC
		LIMIT $5`, q, p.Kind, since, p.LatestOnly, p.Limit, p.Ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
