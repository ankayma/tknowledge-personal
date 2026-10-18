// Package ledger holds the `ledger` row model and the minimal repo used by
// the other internal packages (eff, flags) and cmd/tk-api. Names match
// the schema exactly: claim_id, mark, basis, refs, signer, producer,
// move, cause_id, plus grant_id and the claim-by-subject columns.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Basis is the `basis` jsonb column: {kind, refs, note, valid_until?}.
type Basis struct {
	Kind       string     `json:"kind"`
	Refs       []int64    `json:"refs,omitempty"`
	Note       string     `json:"note,omitempty"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
}

// Row is one ledger entry. Never updated or deleted — only ever
// read, or inserted as a new row.
type Row struct {
	ID         int64
	ClaimID    int64
	Statement  string
	Mark       string
	Basis      *Basis
	Signer     *string
	Producer   string
	Move       string
	Supersedes *int64
	CauseID    *string
	Reason     *string
	TS         time.Time
	GrantID    *string // grant that authorised the write; NULL outside the API
	Subject    *string // NULL for claims written before subjects
	Owner      *string
	ChatID     *string
	AB         bool
	Scope      *string
}

type Repo struct {
	Pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Repo {
	return &Repo{Pool: pool}
}

const selectColumns = `id, claim_id, statement, mark, basis, signer, producer, move, supersedes, cause_id, reason, ts, grant_id, subject, owner, chat_id, ab, scope`

func scanRow(scan func(dest ...any) error) (Row, error) {
	var row Row
	var basisRaw []byte
	if err := scan(&row.ID, &row.ClaimID, &row.Statement, &row.Mark, &basisRaw,
		&row.Signer, &row.Producer, &row.Move, &row.Supersedes, &row.CauseID, &row.Reason, &row.TS, &row.GrantID,
		&row.Subject, &row.Owner, &row.ChatID, &row.AB, &row.Scope); err != nil {
		return Row{}, err
	}
	if len(basisRaw) > 0 {
		var b Basis
		if err := json.Unmarshal(basisRaw, &b); err != nil {
			return Row{}, err
		}
		row.Basis = &b
	}
	return row, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func queryRows(ctx context.Context, q querier, sql string, args ...any) ([]Row, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		row, err := scanRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func queryOne(ctx context.Context, q querier, sql string, args ...any) (Row, bool, error) {
	row, err := scanRow(q.QueryRow(ctx, sql, args...).Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Row{}, false, nil
		}
		return Row{}, false, err
	}
	return row, true, nil
}

// History returns every row for claimID, oldest first. This is history(c) in
// the predicates.
func (r *Repo) History(ctx context.Context, claimID int64) ([]Row, error) {
	return queryRows(ctx, r.Pool, `SELECT `+selectColumns+` FROM ledger WHERE claim_id = $1 ORDER BY id ASC`, claimID)
}

// Current returns the row with the largest id for claimID — current(claim_id)
// in the predicates. ok is false if the claim doesn't exist.
func (r *Repo) Current(ctx context.Context, claimID int64) (row Row, ok bool, err error) {
	return queryOne(ctx, r.Pool, `SELECT `+selectColumns+` FROM ledger WHERE claim_id = $1 ORDER BY id DESC LIMIT 1`, claimID)
}

// Row returns one row by its id.
func (r *Repo) Row(ctx context.Context, id int64) (Row, bool, error) {
	return queryOne(ctx, r.Pool, `SELECT `+selectColumns+` FROM ledger WHERE id = $1`, id)
}

// BySubject returns every row of every claim under subject, oldest first —
// the version chain of spec history().
func (r *Repo) BySubject(ctx context.Context, subject string) ([]Row, error) {
	return queryRows(ctx, r.Pool, `SELECT `+selectColumns+` FROM ledger WHERE subject = $1 ORDER BY id ASC`, subject)
}

// LatestUnderPrefix is the newest row of every subject starting with prefix
// (e.g. every attribute of person:<slug>.), oldest subject first.
func (r *Repo) LatestUnderPrefix(ctx context.Context, prefix string) ([]Row, error) {
	return queryRows(ctx, r.Pool, `
		SELECT `+selectColumns+`
		FROM (SELECT DISTINCT ON (subject) * FROM ledger
		      WHERE subject IS NOT NULL AND left(subject, length($1)) = $1
		      ORDER BY subject, id DESC) t
		ORDER BY id`, prefix)
}

// LatestBySubject is the newest row under subject. Because moves on a
// superseded claim are refused, its claim is the subject's current claim.
func (r *Repo) LatestBySubject(ctx context.Context, subject string) (Row, bool, error) {
	return latestBySubject(ctx, r.Pool, subject)
}

func latestBySubject(ctx context.Context, q querier, subject string) (Row, bool, error) {
	return queryOne(ctx, q, `SELECT `+selectColumns+` FROM ledger WHERE subject = $1 ORDER BY id DESC LIMIT 1`, subject)
}

// Exists reports whether any row exists for claimID.
func (r *Repo) Exists(ctx context.Context, claimID int64) (bool, error) {
	var exists bool
	err := r.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ledger WHERE claim_id = $1)`, claimID).Scan(&exists)
	return exists, err
}

// RowsSince returns every row with id > sinceID, oldest first — what
// tk-checker treats as "new since last sweep".
func (r *Repo) RowsSince(ctx context.Context, sinceID int64) ([]Row, error) {
	return queryRows(ctx, r.Pool, `SELECT `+selectColumns+` FROM ledger WHERE id > $1 ORDER BY id ASC`, sinceID)
}

// AllCurrent returns current(claim_id) for every claim in the table — used by
// tk-checker to walk the ref graph backwards and to check previously_refuted.
func (r *Repo) AllCurrent(ctx context.Context) ([]Row, error) {
	return queryRows(ctx, r.Pool, `
		SELECT `+selectColumns+`
		FROM (SELECT DISTINCT ON (claim_id) * FROM ledger ORDER BY claim_id, id DESC) t`)
}

// InsertParams is the set of fields a caller supplies; id and ts are server-assigned.
type InsertParams struct {
	ClaimID    int64
	Statement  string
	Mark       string
	Basis      *Basis
	Signer     *string
	Producer   string
	Move       string
	Supersedes *int64
	CauseID    *string
	Reason     *string
	GrantID    *string
	Subject    *string
	Owner      *string
	ChatID     *string
	AB         bool
	Scope      *string
}

func basisJSON(b *Basis) ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	return json.Marshal(b)
}

// Insert appends a new row and returns its id. It never updates or deletes —
// there is no other write method on this repo.
func (r *Repo) Insert(ctx context.Context, p InsertParams) (int64, error) {
	bj, err := basisJSON(p.Basis)
	if err != nil {
		return 0, err
	}
	var id int64
	err = r.Pool.QueryRow(ctx, `
		INSERT INTO ledger (claim_id, statement, mark, basis, signer, producer, move, supersedes, cause_id, reason, grant_id, subject, owner, chat_id, ab, scope)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
		p.ClaimID, p.Statement, p.Mark, bj, p.Signer, p.Producer, p.Move, p.Supersedes, p.CauseID, p.Reason, p.GrantID,
		p.Subject, p.Owner, p.ChatID, p.AB, p.Scope,
	).Scan(&id)
	return id, err
}

// ErrRefNotEarlier is returned by InsertValidated when a basis ref does not
// point at a row already assigned a smaller id — Inv5.
var ErrRefNotEarlier = errors.New("ledger: basis ref does not point at an earlier row (Inv5)")

// InsertValidated is the insert path the REST API uses. Within one
// transaction it reads the id about to be assigned, rejects any basis.refs
// that don't point strictly earlier (Inv5), then inserts using that exact id
// — no wasted sequence value on rejection. If p.ClaimID is 0, this is a new
// claim and its first row's claim_id is set equal to its own id (on a
// claim's first row, claim_id = id).
func (r *Repo) InsertValidated(ctx context.Context, p InsertParams) (int64, error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	id, err := insertValidated(ctx, tx, p)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

func insertValidated(ctx context.Context, tx pgx.Tx, p InsertParams) (int64, error) {
	var nextID int64
	if err := tx.QueryRow(ctx, `SELECT nextval('ledger_id_seq')`).Scan(&nextID); err != nil {
		return 0, err
	}

	if p.Basis != nil {
		for _, ref := range p.Basis.Refs {
			if ref >= nextID {
				return 0, ErrRefNotEarlier
			}
		}
	}

	claimID := p.ClaimID
	if claimID == 0 {
		claimID = nextID
	}

	bj, err := basisJSON(p.Basis)
	if err != nil {
		return 0, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger (id, claim_id, statement, mark, basis, signer, producer, move, supersedes, cause_id, reason, grant_id, subject, owner, chat_id, ab, scope)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		nextID, claimID, p.Statement, p.Mark, bj, p.Signer, p.Producer, p.Move, p.Supersedes, p.CauseID, p.Reason, p.GrantID,
		p.Subject, p.Owner, p.ChatID, p.AB, p.Scope,
	); err != nil {
		return 0, err
	}
	return nextID, nil
}

// SubjectTx is a transaction holding the advisory lock of one subject, so a
// read of the subject's latest row and the insert that depends on it cannot
// interleave with another writer of the same subject (two chats admitting the
// same subject at once would otherwise both see "new").
type SubjectTx struct {
	tx      pgx.Tx
	subject string
}

// WithSubjectLock runs fn under subject's lock and commits if fn returns nil.
func (r *Repo) WithSubjectLock(ctx context.Context, subject string, fn func(*SubjectTx) error) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	stx, err := LockSubject(ctx, tx, subject)
	if err != nil {
		return err
	}
	if err := fn(stx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LockSubject takes subject's lock inside a caller's transaction, so a claim
// can be written atomically with other rows (an event, a document); the lock
// and the rows commit or roll back with tx.
func LockSubject(ctx context.Context, tx pgx.Tx, subject string) (*SubjectTx, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "ledger.subject:"+subject); err != nil {
		return nil, err
	}
	return &SubjectTx{tx: tx, subject: subject}, nil
}

func (t *SubjectTx) Latest(ctx context.Context) (Row, bool, error) {
	return latestBySubject(ctx, t.tx, t.subject)
}

func (t *SubjectTx) Insert(ctx context.Context, p InsertParams) (int64, error) {
	return insertValidated(ctx, t.tx, p)
}
