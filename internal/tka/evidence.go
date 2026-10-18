package tka

import (
	"context"

	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Evidence: the many-to-many between claims and documents (spec T–D §4). A
// claim rests on one or more documents and a document stands behind one or more
// claims. Each row says who attached which document to which claim and where in
// the document to look. Rows are never edited: withdrawing one is an event, and
// the row stays readable.
//
// Phase 1 (owner approved 2/10/2026) is one signer: a person attaches documents
// they added themselves. dispute, the auditor role and marking a document as
// failed wait for the first second person (P.8).

// Evidence is one document attached to one claim.
type Evidence struct {
	Truth           string    `json:"truth"`
	Document        string    `json:"document"`
	Signer          string    `json:"signer"`
	Kind            string    `json:"kind"` // attest
	Method          *string   `json:"method,omitempty"`
	Where           *string   `json:"where,omitempty"`
	Note            *string   `json:"note,omitempty"`
	Bound           bool      `json:"bound"`
	Withdrawn       bool      `json:"withdrawn"`
	WithdrawnReason *string   `json:"withdrawn_reason,omitempty"`
	AddedAt         time.Time `json:"added_at"`
}

// Strength is what a claim's standing evidence is worth, read from the best of
// it (B8): the strongest way someone looked at the evidence, among documents
// that point at an original. A document that is only text reads unbound, below
// every method, whatever its method says.
const (
	StrengthNone    = "none"
	StrengthUnbound = "unbound"
)

func strengthRank(s string) int {
	switch s {
	case "opened_source":
		return 4
	case "rehashed":
		return 3
	case "read_text":
		return 2
	case StrengthUnbound:
		return 1
	}
	return 0
}

// strengthOf reads the strength of a set of evidence rows.
func strengthOf(rows []Evidence) string {
	best := StrengthNone
	for _, e := range rows {
		if e.Withdrawn || e.Kind != "attest" {
			continue
		}
		s := StrengthUnbound
		if e.Bound && e.Method != nil {
			s = *e.Method
		}
		if strengthRank(s) > strengthRank(best) {
			best = s
		}
	}
	return best
}

// attesters counts the people standing behind a claim: several documents from
// one signer are one attester.
func attesters(rows []Evidence) int {
	seen := map[string]bool{}
	for _, e := range rows {
		if !e.Withdrawn && e.Kind == "attest" {
			seen[e.Signer] = true
		}
	}
	return len(seen)
}

// attachEvidence writes one attest row and its attached event.
func attachEvidence(ctx context.Context, q querier, a Actor, truth, document string, where, note *string, how string) error {
	if _, err := q.Exec(ctx, `
		INSERT INTO evidence (tenant_id, truth, document, signer, producer, grant_id, kind, "where", note)
		VALUES ($1,$2,$3,$4,$5,$6,'attest',$7,$8)`,
		a.Tenant(), truth, document, a.UserID, a.ActorID, a.GrantID, where, note); err != nil {
		return err
	}
	return evidenceEvent(ctx, q, a.Tenant(), truth, document, "attached", a.UserID, how)
}

func evidenceEvent(ctx context.Context, q querier, tenant, truth, document, event, actor, note string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO evidence_history (tenant_id, truth, document, event, actor, note) VALUES ($1,$2,$3,$4,$5,$6)`,
		TenantOr(tenant), truth, document, event, actor, nullable(note))
	return err
}

const evidenceSelect = `
	SELECT e.truth, e.document, e.signer, e.kind, d.method, e."where", e.note,
	       (d.source_uri IS NOT NULL OR (d.sha256 IS NOT NULL AND d.bytes IS NULL)),
	       coalesce(h.event = 'withdrawn', false), CASE WHEN h.event = 'withdrawn' THEN h.note END, e.added_at
	FROM evidence e
	JOIN document d ON d.tenant_id = e.tenant_id AND d.id = e.document
	LEFT JOIN LATERAL (
		SELECT event, note FROM evidence_history x
		WHERE x.tenant_id = e.tenant_id AND x.truth = e.truth AND x.document = e.document
		ORDER BY x.id DESC LIMIT 1
	) h ON true`

func scanEvidence(rows pgx.Rows) ([]Evidence, error) {
	out := []Evidence{}
	for rows.Next() {
		var e Evidence
		if err := rows.Scan(&e.Truth, &e.Document, &e.Signer, &e.Kind, &e.Method, &e.Where, &e.Note,
			&e.Bound, &e.Withdrawn, &e.WithdrawnReason, &e.AddedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	return out, rows.Err()
}

// evidenceOf is every evidence row of a claim, oldest first, withdrawn ones too.
func evidenceOf(ctx context.Context, q querier, tenant, truth string) ([]Evidence, error) {
	rows, err := q.Query(ctx, evidenceSelect+` WHERE e.tenant_id = $1 AND e.truth = $2 ORDER BY e.id`,
		TenantOr(tenant), truth)
	if err != nil {
		return nil, err
	}
	return scanEvidence(rows)
}

func evidenceRow(ctx context.Context, q querier, tenant, truth, document string) (Evidence, bool, error) {
	rows, err := q.Query(ctx, evidenceSelect+` WHERE e.tenant_id = $1 AND e.truth = $2 AND e.document = $3`,
		TenantOr(tenant), truth, document)
	if err != nil {
		return Evidence{}, false, err
	}
	list, err := scanEvidence(rows)
	if err != nil || len(list) == 0 {
		return Evidence{}, false, err
	}
	return list[0], true, nil
}

// evidenceFor lists the claims a document stands behind right now.
func evidenceFor(ctx context.Context, q querier, tenant, document string) ([]string, error) {
	rows, err := q.Query(ctx, evidenceSelect+` WHERE e.tenant_id = $1 AND e.document = $2 ORDER BY e.id`,
		TenantOr(tenant), document)
	if err != nil {
		return nil, err
	}
	list, err := scanEvidence(rows)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range list {
		if !e.Withdrawn && e.Kind == "attest" {
			out = append(out, e.Truth)
		}
	}
	return out, nil
}

// ---------- attest ----------

type AttestInput struct {
	Truth    string `json:"truth"`
	Document string `json:"document"`
	Where    string `json:"where"`
	Note     string `json:"note"`
}

type AttestResult struct {
	Evidence Evidence `json:"evidence"`
	// Repeat is true when this document was already attached to this claim: one
	// row per pair, so the answer is the row that was there.
	Repeat bool `json:"repeat"`
	// Warning is set when the claim is not standing: the evidence is recorded
	// all the same, against a claim that has fallen.
	Warning string `json:"warning,omitempty"`
	// Strength and Attesters are the claim's, after this call.
	Strength  string `json:"strength"`
	Attesters int    `json:"attesters"`
}

// Attest attaches a document the caller added to a claim (spec T–D §4).
func (r *Repo) Attest(ctx context.Context, a Actor, in AttestInput) (AttestResult, error) {
	if err := r.checkSigner(a); err != nil {
		return AttestResult{}, err
	}
	where := strings.TrimSpace(in.Where)
	if where == "" {
		return AttestResult{}, fail("EMPTY_WHERE", "say where in the document the claim is read")
	}
	var out AttestResult
	err := r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		t, err := r.ReadTruth(ctx, q, a.Tenant(), in.Truth)
		if err != nil {
			return err
		}
		if t.Basis != "fact" {
			return failNext("NOT_A_FACT",
				"A derivation stands on its premises; attach the document to the fact underneath it — chain(...) shows which.",
				"%s rests on a derivation: evidence belongs to the facts it is derived from", t.ID)
		}
		d, err := readDocument(ctx, q, a.Tenant(), in.Document)
		if err != nil {
			return err
		}
		if d.Signer != a.UserID {
			return fail("DOCUMENT_NOT_YOURS",
				"%s was added by %s: evidence you attach is evidence you looked at yourself", d.ID, d.Signer)
		}
		if t.Effective != "T" {
			out.Warning = fmt.Sprintf("%s is %s: your evidence is recorded against a fallen claim", t.ID, t.Effective)
		}
		existing, found, err := evidenceRow(ctx, q, a.Tenant(), t.ID, d.ID)
		if err != nil {
			return err
		}
		switch {
		case found && !existing.Withdrawn:
			out.Repeat = true
		case found: // withdrawn before; attaching again is a new event on the same row
			if existing.Signer != a.UserID {
				return fail("EVIDENCE_NOT_YOURS", "this evidence was attached by %s", existing.Signer)
			}
			if err := evidenceEvent(ctx, q, a.Tenant(), t.ID, d.ID, "attached", a.UserID, "attest again"); err != nil {
				return err
			}
			out.Repeat = true
		default:
			if err := attachEvidence(ctx, q, a, t.ID, d.ID, &where, optional(in.Note), "attest"); err != nil {
				return err
			}
		}
		if out.Evidence, _, err = evidenceRow(ctx, q, a.Tenant(), t.ID, d.ID); err != nil {
			return err
		}
		all, err := evidenceOf(ctx, q, a.Tenant(), t.ID)
		if err != nil {
			return err
		}
		out.Strength, out.Attesters = strengthOf(all), attesters(all)
		return nil
	})
	return out, err
}

// ---------- withdraw ----------

type WithdrawInput struct {
	Truth    string `json:"truth"`
	Document string `json:"document"`
	Reason   string `json:"reason"`
}

type WithdrawResult struct {
	Evidence  Evidence `json:"evidence"`
	Strength  string   `json:"strength"`
	Attesters int      `json:"attesters"`
	// Flag is opened when nothing else attests the claim any more. The claim
	// itself does not move: only its owner lowers it.
	Flag *Flag `json:"flag,omitempty"`
}

// WithdrawEvidence takes a document back from a claim, with the reason. The row
// stays; the claim does not fall — one person taking back their evidence cannot
// lower a claim (spec T–D §6).
func (r *Repo) WithdrawEvidence(ctx context.Context, a Actor, in WithdrawInput) (WithdrawResult, error) {
	if err := r.checkSigner(a); err != nil {
		return WithdrawResult{}, err
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return WithdrawResult{}, fail("EMPTY_REASON", "withdrawing evidence requires a reason")
	}
	var out WithdrawResult
	err := r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		truth, document := CanonID(in.Truth), CanonID(in.Document)
		e, found, err := evidenceRow(ctx, q, a.Tenant(), truth, document)
		if err != nil {
			return err
		}
		if !found {
			return fail("NOT_ATTACHED", "%s is not attached to %s", document, truth)
		}
		if e.Signer != a.UserID {
			return fail("EVIDENCE_NOT_YOURS", "this evidence was attached by %s; only they can withdraw it", e.Signer)
		}
		if e.Withdrawn {
			return fail("ALREADY_WITHDRAWN", "%s was already withdrawn from %s", document, truth)
		}
		if err := evidenceEvent(ctx, q, a.Tenant(), truth, document, "withdrawn", a.UserID, reason); err != nil {
			return err
		}
		all, err := evidenceOf(ctx, q, a.Tenant(), truth)
		if err != nil {
			return err
		}
		out.Strength, out.Attesters = strengthOf(all), attesters(all)
		if out.Attesters == 0 {
			f, err := openFlag(ctx, q, a, "evidence_withdrawn", truth, fmt.Sprintf(
				"%s withdrawn from %s (%s); nothing else attests it now", document, truth, reason))
			if err != nil {
				return err
			}
			out.Flag = &f
		}
		out.Evidence, _, err = evidenceRow(ctx, q, a.Tenant(), truth, document)
		return err
	})
	return out, err
}

// ---------- flags ----------

// Flag is work the machine set aside for a person (T-ledger §6.0). It changes
// nothing: a claim with an open flag stands until its owner moves.
type Flag struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Subject    string     `json:"subject"`
	Detail     string     `json:"detail"`
	Status     string     `json:"status"` // open | closed
	OpenedBy   string     `json:"opened_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ClosedBy   *string    `json:"closed_by,omitempty"`
	ClosedNote *string    `json:"closed_note,omitempty"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
}

func openFlag(ctx context.Context, q querier, a Actor, kind, subject, detail string) (Flag, error) {
	num, err := nextID(ctx, q, a.Tenant(), "F")
	if err != nil {
		return Flag{}, err
	}
	var id string
	if err := q.QueryRow(ctx, `
		INSERT INTO tka_flag (tenant_id, num, kind, subject, detail) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		a.Tenant(), num, kind, subject, detail).Scan(&id); err != nil {
		return Flag{}, err
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO tka_flag_history (tenant_id, flag, event, actor, note) VALUES ($1,$2,'opened',$3,NULL)`,
		a.Tenant(), id, a.UserID); err != nil {
		return Flag{}, err
	}
	return readFlag(ctx, q, a.Tenant(), id)
}

const flagSelect = `
	SELECT f.id, f.kind, f.subject, f.detail, o.actor, f.created_at,
	       c.actor, c.note, c.at
	FROM tka_flag f
	LEFT JOIN LATERAL (SELECT actor FROM tka_flag_history x
		WHERE x.tenant_id = f.tenant_id AND x.flag = f.id AND x.event = 'opened' ORDER BY x.id LIMIT 1) o ON true
	LEFT JOIN LATERAL (SELECT actor, note, at FROM tka_flag_history x
		WHERE x.tenant_id = f.tenant_id AND x.flag = f.id AND x.event = 'closed' ORDER BY x.id DESC LIMIT 1) c ON true`

func scanFlags(rows pgx.Rows) ([]Flag, error) {
	out := []Flag{}
	for rows.Next() {
		var f Flag
		var opened *string
		if err := rows.Scan(&f.ID, &f.Kind, &f.Subject, &f.Detail, &opened, &f.CreatedAt,
			&f.ClosedBy, &f.ClosedNote, &f.ClosedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if opened != nil {
			f.OpenedBy = *opened
		}
		f.Status = "open"
		if f.ClosedBy != nil {
			f.Status = "closed"
		}
		out = append(out, f)
	}
	rows.Close()
	return out, rows.Err()
}

func readFlag(ctx context.Context, q querier, tenant, id string) (Flag, error) {
	rows, err := q.Query(ctx, flagSelect+` WHERE f.tenant_id = $1 AND f.id = $2`, TenantOr(tenant), id)
	if err != nil {
		return Flag{}, err
	}
	list, err := scanFlags(rows)
	if err != nil {
		return Flag{}, err
	}
	if len(list) == 0 {
		return Flag{}, fail("FLAG_UNKNOWN", "%s is not a flag in this store", id)
	}
	return list[0], nil
}

// openFlagsOn lists the open flags raised on one claim or document.
func openFlagsOn(ctx context.Context, q querier, tenant, subject string) ([]Flag, error) {
	rows, err := q.Query(ctx, flagSelect+` WHERE f.tenant_id = $1 AND f.subject = $2 ORDER BY f.num`,
		TenantOr(tenant), subject)
	if err != nil {
		return nil, err
	}
	list, err := scanFlags(rows)
	if err != nil {
		return nil, err
	}
	out := []Flag{}
	for _, f := range list {
		if f.Status == "open" {
			out = append(out, f)
		}
	}
	return out, nil
}

// Flags lists the store's flags, newest first. status: open (default), closed, all.
func (r *Repo) Flags(ctx context.Context, tenant, status string) ([]Flag, error) {
	want := strings.ToLower(strings.TrimSpace(status))
	if want == "" {
		want = "open"
	}
	if want != "open" && want != "closed" && want != "all" {
		return nil, fail("INVALID_STATUS", "status must be open, closed or all")
	}
	var out []Flag
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, flagSelect+` WHERE f.tenant_id = $1 ORDER BY f.num DESC`, TenantOr(tenant))
		if err != nil {
			return err
		}
		list, err := scanFlags(rows)
		if err != nil {
			return err
		}
		out = []Flag{}
		for _, f := range list {
			if want == "all" || f.Status == want {
				out = append(out, f)
			}
		}
		return nil
	})
	return out, err
}

type CloseFlagInput struct {
	Flag string `json:"flag"`
	Note string `json:"note"`
}

// CloseFlag is the named move that answers a flag: who looked, and what they
// found. The flag stays readable with the note.
func (r *Repo) CloseFlag(ctx context.Context, a Actor, in CloseFlagInput) (Flag, error) {
	if err := r.checkSigner(a); err != nil {
		return Flag{}, err
	}
	note := strings.TrimSpace(in.Note)
	if note == "" {
		return Flag{}, fail("EMPTY_REASON", "closing a flag says what was found")
	}
	id := strings.ToUpper(strings.TrimSpace(in.Flag))
	if n := strings.TrimLeft(strings.TrimPrefix(id, "F-"), "0"); strings.HasPrefix(id, "F-") && n != "" {
		id = "F-" + n
	}
	var out Flag
	err := r.withTenant(ctx, a.Tenant(), func(q querier) error {
		f, err := readFlag(ctx, q, a.Tenant(), id)
		if err != nil {
			return err
		}
		if f.Status == "closed" {
			return failNext("ALREADY_CLOSED", "Nothing to do. flags(status: \"closed\") shows how it was closed.",
				"%s was closed by %s", f.ID, *f.ClosedBy)
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO tka_flag_history (tenant_id, flag, event, actor, note) VALUES ($1,$2,'closed',$3,$4)`,
			a.Tenant(), f.ID, a.UserID, note); err != nil {
			return err
		}
		out, err = readFlag(ctx, q, a.Tenant(), f.ID)
		return err
	})
	return out, err
}
