package tka

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Source of an assumption: where the claim came from, none of it verified
// (D-02 §2.1 — everything below Principle and Fact is an assumption).
type Source struct {
	Kind   string  `json:"kind"`
	Detail *string `json:"detail,omitempty"`
}

var sourceKinds = []string{"owner", "ai", "hearsay", "unverified", "unknown"}

// ParseSource reads "owner", "hearsay: Paul", "unverified: blog X".
func ParseSource(raw string) (Source, error) {
	kind, detail, _ := strings.Cut(strings.TrimSpace(raw), ":")
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !slices.Contains(sourceKinds, kind) {
		return Source{}, fail("INVALID_SOURCE", "source must be one of %s", strings.Join(sourceKinds, ", "))
	}
	s := Source{Kind: kind}
	if d := strings.TrimSpace(detail); d != "" {
		s.Detail = &d
	}
	return s, nil
}

func (s Source) String() string {
	if s.Detail == nil {
		return s.Kind
	}
	return s.Kind + ": " + *s.Detail
}

type Assumption struct {
	ID        string    `json:"id"`
	Claim     string    `json:"claim"`
	ClaimKey  string    `json:"claim_key"`
	Type      string    `json:"type"`
	Source    Source    `json:"source"`
	If        *string   `json:"if,omitempty"`
	Not       *string   `json:"not,omitempty"`
	Via       *string   `json:"via,omitempty"`
	Supports  []string  `json:"supports"`
	Revision  int       `json:"revision"`
	Status    string    `json:"status"`
	Signer    string    `json:"signer"`
	Producer  string    `json:"producer"`
	GrantID   string    `json:"grant_id"`
	ChatID    *string   `json:"chat_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	History   []Event   `json:"history"`
}

type Truth struct {
	ID           string     `json:"id"`
	Claim        string     `json:"claim"`
	ClaimKey     string     `json:"claim_key"`
	Basis        string     `json:"basis"`
	Source       *string    `json:"source,omitempty"`
	SourceURL    *string    `json:"source_url,omitempty"`
	SourceDoc    *string    `json:"source_doc,omitempty"`
	SourceEntry  *string    `json:"source_entry,omitempty"`
	SourceDate   *time.Time `json:"source_date,omitempty"`
	VerifiedAt   time.Time  `json:"verified_at"`
	ValidUntil   *time.Time `json:"valid_until,omitempty"`
	Premises     []string   `json:"premises"`
	Derivation   *string    `json:"derivation,omitempty"`
	Supports     []string   `json:"supports"`
	PromotedFrom *string    `json:"promoted_from,omitempty"`
	Supersedes   *string    `json:"supersedes,omitempty"`
	Status       string     `json:"status"`
	// Effective is Status unless a premise has fallen: then it reads
	// needs_review — the entry is not lowered by a machine, it is flagged.
	Effective    string   `json:"effective"`
	PremisesFell []string `json:"premises_fell,omitempty"`
	// Evidence is every document attached to a fact, withdrawn ones included
	// (spec T–D §4). Attesters counts the people standing behind it — several
	// documents from one signer are one — and Strength is the best way any of
	// them looked at the evidence (B8). A derivation has none of these: it stands
	// on its premises.
	Evidence  []Evidence `json:"evidence,omitempty"`
	Attesters *int       `json:"attesters,omitempty"`
	Strength  string     `json:"strength,omitempty"`
	// Flags are the open flags on this claim: work waiting for its owner.
	Flags     []Flag    `json:"flags,omitempty"`
	Signer    string    `json:"signer"`
	Producer  string    `json:"producer"`
	GrantID   string    `json:"grant_id"`
	ChatID    *string   `json:"chat_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	History   []Event   `json:"history"`
}

const assumptionColumns = `id, claim, claim_key, type, source_kind, source_detail, if_obs, not_obs, via,
	supports, revision, signer, producer, grant_id, chat_id, created_at`

const truthColumns = `id, claim, claim_key, basis, source, source_url, source_doc, source_entry,
	source_date, verified_at, valid_until,
	premises, derivation, supports, promoted_from, supersedes, signer, producer, grant_id, chat_id, created_at`

func scanAssumption(row pgx.Row) (Assumption, error) {
	var a Assumption
	err := row.Scan(&a.ID, &a.Claim, &a.ClaimKey, &a.Type, &a.Source.Kind, &a.Source.Detail, &a.If, &a.Not, &a.Via,
		&a.Supports, &a.Revision, &a.Signer, &a.Producer, &a.GrantID, &a.ChatID, &a.CreatedAt)
	return a, err
}

func scanTruth(row pgx.Row) (Truth, error) {
	var t Truth
	err := row.Scan(&t.ID, &t.Claim, &t.ClaimKey, &t.Basis, &t.Source, &t.SourceURL, &t.SourceDoc,
		&t.SourceEntry, &t.SourceDate, &t.VerifiedAt, &t.ValidUntil,
		&t.Premises, &t.Derivation, &t.Supports, &t.PromotedFrom, &t.Supersedes, &t.Signer, &t.Producer, &t.GrantID,
		&t.ChatID, &t.CreatedAt)
	return t, err
}

// ReadAssumption / ReadTruth load one entry with its history and status.
func (r *Repo) ReadAssumption(ctx context.Context, q querier, tenant, id string) (Assumption, error) {
	id = CanonID(id)
	if q == nil {
		q = r.Pool
	}
	a, err := scanAssumption(q.QueryRow(ctx, `SELECT `+assumptionColumns+` FROM assumption
		WHERE tenant_id = $1 AND id = $2`, TenantOr(tenant), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Assumption{}, fail("NOT_FOUND", "%s is not an assumption in this store", id)
	}
	if err != nil {
		return Assumption{}, err
	}
	if a.History, err = history(ctx, q, tenant, a.ID); err != nil {
		return Assumption{}, err
	}
	a.Status = status("assumption", a.History)
	return a, nil
}

func (r *Repo) ReadTruth(ctx context.Context, q querier, tenant, id string) (Truth, error) {
	id = CanonID(id)
	if q == nil {
		q = r.Pool
	}
	t, err := scanTruth(q.QueryRow(ctx, `SELECT `+truthColumns+` FROM truth_ledger
		WHERE tenant_id = $1 AND id = $2`, TenantOr(tenant), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Truth{}, fail("NOT_FOUND", "%s is not an entry in the truth ledger", id)
	}
	if err != nil {
		return Truth{}, err
	}
	if t.History, err = history(ctx, q, tenant, t.ID); err != nil {
		return Truth{}, err
	}
	t.Status = status("truth", t.History)
	// Whatever the claim leans on — a premise it was derived from, or an entry
	// that supplied part of its evidence — falling means this claim has to be
	// read again.
	under := t.Premises
	if t.SourceEntry != nil {
		under = append(append([]string{}, under...), *t.SourceEntry)
	}
	if t.PremisesFell, err = r.premisesFell(ctx, q, tenant, under); err != nil {
		return Truth{}, err
	}
	t.Effective = t.Status
	if t.Status == "T" && len(t.PremisesFell) > 0 {
		t.Effective = "needs_review"
	}
	if t.Basis == "fact" {
		if t.Evidence, err = evidenceOf(ctx, q, tenant, t.ID); err != nil {
			return Truth{}, err
		}
		n := attesters(t.Evidence)
		t.Attesters, t.Strength = &n, strengthOf(t.Evidence)
	}
	if t.Flags, err = openFlagsOn(ctx, q, tenant, t.ID); err != nil {
		return Truth{}, err
	}
	return t, nil
}

// exact finds the entry with this claim key whose status is live: an open
// assumption, or a standing T.
func exact(ctx context.Context, q querier, tenant, table, key string) (string, string, error) {
	rows, err := q.Query(ctx, `SELECT id FROM `+table+`
		WHERE tenant_id = $1 AND claim_key = $2 ORDER BY num`, TenantOr(tenant), key)
	if err != nil {
		return "", "", err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", "", err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	kind := "assumption"
	if table == "truth_ledger" {
		kind = "truth"
	}
	for _, id := range ids {
		events, err := history(ctx, q, tenant, id)
		if err != nil {
			return "", "", err
		}
		st := status(kind, events)
		if st == "open" || st == "T" {
			return id, st, nil
		}
	}
	return "", "", nil
}

// ---------- assume ----------

type AssumeInput struct {
	Claim      string   `json:"claim"`
	Type       string   `json:"type"`
	Source     string   `json:"source"`
	If         string   `json:"if"`
	Not        string   `json:"not"`
	Via        string   `json:"via"`
	Supports   []string `json:"supports"`
	ChatID     string   `json:"chat_id"`
	ConfirmNew bool     `json:"confirm_new"`
	// Thread links the assumption to a thread (TH-… or one of its keywords) in
	// the same transaction.
	Thread string `json:"thread"`
}

// AssumeResult is the record written, or — when a close claim already exists
// and the caller has not confirmed — nothing written and the list to look at.
type AssumeResult struct {
	Entry   *Assumption
	Similar []Similar
	Thread  *ThreadLinked
}

var assumptionTypes = []string{"A-r", "A-c", "A-p", "A-b"}

func normalizeType(raw string) (string, bool) {
	t := strings.ToUpper(strings.TrimSpace(raw))
	t = strings.ReplaceAll(t, "_", "-")
	if len(t) > 1 {
		t = "A-" + strings.ToLower(t[2:])
	}
	return t, slices.Contains(assumptionTypes, t)
}

// tails checks the resolve path each type must declare (D-02 §0.2): an
// assumption with no way out runs as a tautology.
func tails(typ, ifObs, notObs, via string) (*string, *string, *string, error) {
	p := func(s string) *string {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		s = strings.TrimSpace(s)
		return &s
	}
	i, n, v := p(ifObs), p(notObs), p(via)
	switch typ {
	case "A-p":
		if i == nil || n == nil {
			return nil, nil, nil, fail("MISSING_TAIL", "A-p needs if: (what raises it) and not: (what refutes it)")
		}
		if v != nil {
			return nil, nil, nil, fail("INVALID_TAIL", "A-p has no via:")
		}
	case "A-c", "A-b":
		if v == nil {
			return nil, nil, nil, fail("MISSING_TAIL", "%s needs via: (%s)", typ,
				map[string]string{"A-c": "amendment", "A-b": "the channel that must be built first"}[typ])
		}
		if i != nil || n != nil {
			return nil, nil, nil, fail("INVALID_TAIL", "%s has no if:/not: — its truth-maker is not an observation yet", typ)
		}
	case "A-r":
		if i != nil || n != nil || v != nil {
			return nil, nil, nil, fail("INVALID_TAIL", "A-r is a reading aid: it is never raised, so it has no tail")
		}
	}
	return i, n, v, nil
}

// Assume writes one assumption. It refuses to write blind next to a claim that
// is already there: the owner sees the neighbours first.
func (r *Repo) Assume(ctx context.Context, a Actor, in AssumeInput) (AssumeResult, error) {
	if err := r.checkSigner(a); err != nil {
		return AssumeResult{}, err
	}
	claim := CleanClaim(in.Claim)
	if claim == "" {
		return AssumeResult{}, fail("EMPTY_CLAIM", "claim is required")
	}
	// Is it a claim at all? That question comes before which kind of assumption
	// it is, and before the store is touched (owner decision, 28/9). Two conditions
	// only: being about the future is what an A-p *is*, so the third would refuse
	// the very thing this table exists for. The wording still has to stand alone,
	// because promote will carry it into the ledger unchanged.
	if check := CheckClaim(claim, ""); !check.Admissible {
		r.step(ctx, a.Tenant(), StepRefusal)
		return AssumeResult{}, failClaim(check)
	}
	typ, ok := normalizeType(in.Type)
	if !ok {
		return AssumeResult{}, fail("INVALID_TYPE", "type must be one of %s", strings.Join(assumptionTypes, ", "))
	}
	src, err := ParseSource(in.Source)
	if err != nil {
		return AssumeResult{}, err
	}
	ifObs, notObs, via, err := tails(typ, in.If, in.Not, in.Via)
	if err != nil {
		return AssumeResult{}, err
	}
	key := ClaimKey(claim)

	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return AssumeResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return AssumeResult{}, err
	}
	if err := r.admits(ctx, tx, a, claim); err != nil {
		return AssumeResult{}, err
	}

	if id, _, err := exact(ctx, tx, a.Tenant(), "truth_ledger", key); err != nil {
		return AssumeResult{}, err
	} else if id != "" {
		return AssumeResult{}, fail("ALREADY_T", "this claim already stands as %s", id)
	}
	if id, _, err := exact(ctx, tx, a.Tenant(), "assumption", key); err != nil {
		return AssumeResult{}, err
	} else if id != "" {
		open, err := r.ReadAssumption(ctx, tx, a.Tenant(), id)
		if err != nil {
			return AssumeResult{}, err
		}
		if open.Type != typ {
			return AssumeResult{}, failNext("TYPE_CONFLICT",
				"Read "+id+" with entry(...), then either keep its kind or change this wording.",
				"%s already holds this claim as %s; one claim has one way out", id, open.Type)
		}
		// Duplicate: nothing written, but the link the caller asked for is.
		linked, err := r.linkWritten(ctx, tx, a, in.Thread, id, "assume")
		if err != nil {
			return AssumeResult{}, err
		}
		if linked != nil {
			if err := tx.Commit(ctx); err != nil {
				return AssumeResult{}, err
			}
		}
		return AssumeResult{Entry: &open, Thread: linked}, nil
	}
	if err := checkThreadRef(ctx, tx, a.Tenant(), in.Thread); err != nil {
		return AssumeResult{}, err
	}
	if !in.ConfirmNew {
		similar, err := r.Similar(ctx, tx, a.Tenant(), key, "")
		if err != nil {
			return AssumeResult{}, err
		}
		if len(similar) > 0 {
			r.step(ctx, a.Tenant(), StepRefusal)
			return AssumeResult{Similar: similar}, nil // nothing written
		}
	}

	num, err := nextID(ctx, tx, a.Tenant(), "A")
	if err != nil {
		return AssumeResult{}, err
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO assumption (tenant_id, num, claim, claim_key, type, source_kind, source_detail, if_obs, not_obs, via,
			supports, signer, producer, grant_id, chat_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id`,
		a.Tenant(), num, claim, key, typ, src.Kind, src.Detail, ifObs, notObs, via, supportsOf(in.Supports),
		a.UserID, a.ActorID, a.GrantID, nullable(in.ChatID)).Scan(&id); err != nil {
		return AssumeResult{}, err
	}
	note := ""
	if in.ConfirmNew {
		note = "confirm_new"
	}
	if err := addEvent(ctx, tx, a.Tenant(), id, "created", a.UserID, note); err != nil {
		return AssumeResult{}, err
	}
	linked, err := r.linkWritten(ctx, tx, a, in.Thread, id, "assume")
	if err != nil {
		return AssumeResult{}, err
	}
	entry, err := r.ReadAssumption(ctx, tx, a.Tenant(), id)
	if err != nil {
		return AssumeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AssumeResult{}, err
	}
	r.step(ctx, a.Tenant(), StepWrite)
	return AssumeResult{Entry: &entry, Thread: linked}, nil
}

func supportsOf(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nullable(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}
