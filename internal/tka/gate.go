package tka

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The gate of D-07 H.2: a claim only becomes [T] if its basis can be rebuilt
// on the spot. The machine checks the form — a fact has a source, a derivation
// names premises that exist and still stand. Whether the source is right, and
// whether the chain actually holds, is the person's work; no machine replaces it.

// BasisInput is the part of admit and promote that the gate checks.
type BasisInput struct {
	Basis string `json:"basis"`
	// Where is the place in the document the claim is read: "page 2", "row Q3".
	// It lands on the first evidence row (B2, owner approved 2/10). Source is its old
	// name and is still taken.
	Where     string `json:"where"`
	Source    string `json:"source"`
	SourceURL string `json:"source_url"` // refused for a new fact: a link keeps nothing (B1)
	SourceDoc string `json:"source_doc"` // D-… a document the signer added
	// SourceEntry is an entry already in the ledger that carries part of the
	// evidence — the date, the occasion — which the document does not. It is
	// evidence, not a premise: premises are claims, and a document is not one.
	SourceEntry string   `json:"source_entry"`
	SourceDate  string   `json:"source_date"`
	ValidUntil  string   `json:"valid_until"`
	Premises    []string `json:"premises"`
	Derivation  string   `json:"derivation"`
	Supports    []string `json:"supports"`
	ChatID      string   `json:"chat_id"`
}

type checkedBasis struct {
	basis       string
	source      *string
	sourceURL   *string
	sourceDoc   *string
	sourceEntry *string
	sourceDate  *time.Time
	validUntil  *time.Time
	premises    []string
	derivation  *string
}

func parseDay(label, raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	d, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return nil, fail("INVALID_DATE", "%s must be YYYY-MM-DD", label)
	}
	return &d, nil
}

// checkBasis is the gate's form check, shared by admit and promote.
func (r *Repo) checkBasis(ctx context.Context, q querier, a Actor, in BasisInput) (checkedBasis, error) {
	tenant := a.Tenant()
	var out checkedBasis
	switch strings.ToLower(strings.TrimSpace(in.Basis)) {
	case "fact":
		out.basis = "fact"
	case "derivation":
		out.basis = "derivation"
	default:
		return out, fail("INVALID_BASIS", "basis must be fact or derivation — P.9 has no third kind")
	}

	sourceDate, err := parseDay("source_date", in.SourceDate)
	if err != nil {
		return out, err
	}
	validUntil, err := parseDay("valid_until", in.ValidUntil)
	if err != nil {
		return out, err
	}
	out.sourceDate, out.validUntil = sourceDate, validUntil

	if out.basis == "fact" {
		src := strings.TrimSpace(in.Where)
		if src == "" {
			src = strings.TrimSpace(in.Source)
		}
		if src == "" {
			return out, fail("NO_SOURCE", "a fact says where in its evidence the claim is read; without that the claim belongs in assume(...)")
		}
		if strings.TrimSpace(in.Derivation) != "" || len(in.Premises) > 0 {
			return out, failNext("INVALID_BASIS",
				"If an entry already in the ledger carries part of the evidence — the date, the occasion — name it as source_entry, not as a premise: it is evidence, not a step of reasoning.",
				"a fact carries no premises and no derivation chain")
		}
		if validUntil != nil && validUntil.Before(time.Now().UTC().Truncate(24*time.Hour)) {
			return out, fail("EXPIRED_VALID_UNTIL", "valid_until %s is already past", validUntil.Format("2006-01-02"))
		}
		// The citation says where to look; this says the looking is possible for
		// someone who is not the person writing (owner decision, 28/9).
		doc, entry, err := r.checkEvidence(ctx, q, a, in)
		if err != nil {
			return out, err
		}
		out.source, out.sourceDoc, out.sourceEntry = &src, doc, entry
		out.premises = []string{}
		return out, nil
	}

	chain := strings.TrimSpace(in.Derivation)
	if chain == "" {
		return out, fail("EMPTY_DERIVATION", "a derivation must show premises ⟹ conclusion, rebuildable without looking elsewhere")
	}
	// Two premises at least (owner decision, 28/9). From one premise nothing new
	// follows: what comes out is that premise read again, and the store already
	// has a home for a reading — assume(type: "A-r"), which is never raised.
	// Requiring the second premise is what drags the bridge out of the prose: a
	// chain that says "P.1 ⟹ therefore X" is leaning on an unnamed claim, and
	// that claim is the one nobody has audited.
	if len(in.Premises) < minPremises {
		return out, failNext("SINGLE_PREMISE",
			"Name the second premise: the step your chain makes is itself a claim, so add it as a principle "+
				"or admit it first. If it really is only another way of reading that one premise, "+
				"write it with assume(type: \"A-r\") — a reading aid is deliberately never raised.",
			"a derivation stands on at least %d premises; from one, nothing new follows", minPremises)
	}
	if validUntil != nil {
		return out, fail("INVALID_BASIS", "valid_until belongs to a fact, not a derivation")
	}
	if strings.TrimSpace(in.SourceURL) != "" || strings.TrimSpace(in.SourceDoc) != "" ||
		strings.TrimSpace(in.SourceEntry) != "" {
		return out, fail("INVALID_BASIS", "a derivation stands on its premises; evidence belongs to the facts underneath it")
	}
	premises := make([]string, 0, len(in.Premises))
	for _, raw := range in.Premises {
		ref := strings.TrimSpace(raw)
		if code, ok := NormalizeCode(ref); ok || strings.HasPrefix(strings.ToUpper(ref), "P.") {
			if !ok {
				return out, fail("PRINCIPLE_UNKNOWN", "%q is not a principle code", ref)
			}
			p, err := readPrinciple(ctx, q, tenant, code)
			if err != nil {
				return out, err
			}
			if p.Status != "active" {
				return out, fail("PRINCIPLE_RETIRED", "%s has been retired; a claim cannot rest on it", code)
			}
			premises = append(premises, code)
			continue
		}
		t, err := r.ReadTruth(ctx, q, tenant, ref)
		if err != nil {
			if e, ok := err.(*Error); ok && e.Code == "NOT_FOUND" {
				return out, fail("PREMISE_NOT_FOUND", "premise %s is not in the truth ledger", ref)
			}
			return out, err
		}
		if t.Effective != "T" {
			return out, fail("PREMISE_NOT_T", "premise %s is %s, not T — a claim cannot stand on it", ref, t.Effective)
		}
		premises = append(premises, t.ID)
	}
	out.premises = premises
	out.derivation = &chain
	return out, nil
}

// ---------- admit ----------

type AdmitInput struct {
	Claim string `json:"claim"`
	BasisInput
	Supersedes string `json:"supersedes"`
	ConfirmNew bool   `json:"confirm_new"`
	// Thread links the entry to a thread (TH-… or one of its keywords) in the
	// same transaction. A thread that does not resolve refuses the write.
	Thread string `json:"thread"`
}

type AdmitResult struct {
	Entry   *Truth
	Similar []Similar
	Thread  *ThreadLinked
}

// Admit takes a new [T] through the gate.
func (r *Repo) Admit(ctx context.Context, a Actor, in AdmitInput) (AdmitResult, error) {
	if err := r.checkSigner(a); err != nil {
		return AdmitResult{}, err
	}
	claim := CleanClaim(in.Claim)
	if claim == "" {
		return AdmitResult{}, fail("EMPTY_CLAIM", "claim is required")
	}
	// T/A applies to claims, so this comes before anything else — before the
	// store is opened at all (owner decision, 28/9). It needs no database: whether a
	// sentence is a claim depends on the sentence. Asking the store first would
	// answer the wrong question — a full trial store would say TENANT_FULL to
	// something that was never a claim to begin with.
	if check := CheckClaim(claim, in.Basis); !check.Admissible {
		r.step(ctx, a.Tenant(), StepRefusal)
		return AdmitResult{}, failClaim(check)
	}
	key := ClaimKey(claim)

	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return AdmitResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return AdmitResult{}, err
	}
	if err := r.admits(ctx, tx, a, claim); err != nil {
		return AdmitResult{}, err
	}
	// What this store already holds comes first: it is the more specific answer,
	// and telling someone to fetch evidence for a claim already written wastes
	// the trip.
	if id, _, err := exact(ctx, tx, a.Tenant(), "truth_ledger", key); err != nil {
		return AdmitResult{}, err
	} else if id != "" {
		entry, err := r.ReadTruth(ctx, tx, a.Tenant(), id)
		if err != nil {
			return AdmitResult{}, err
		}
		// Duplicate: no claim written. Linking it to the thread named is still
		// what the caller asked for.
		linked, err := r.linkWritten(ctx, tx, a, in.Thread, id, "admit")
		if err != nil {
			return AdmitResult{}, err
		}
		if linked != nil {
			if err := tx.Commit(ctx); err != nil {
				return AdmitResult{}, err
			}
		}
		return AdmitResult{Entry: &entry, Thread: linked}, nil
	}
	if id, _, err := exact(ctx, tx, a.Tenant(), "assumption", key); err != nil {
		return AdmitResult{}, err
	} else if id != "" {
		r.step(ctx, a.Tenant(), StepRefusal)
		return AdmitResult{}, failNext("USE_PROMOTE", "Call promote("+id+") with the source that settles it.",
			"%s already holds this claim as an open assumption", id)
	}

	basis, err := r.checkBasis(ctx, tx, a, in.BasisInput)
	if err != nil {
		r.step(ctx, a.Tenant(), StepRefusal)
		return AdmitResult{}, err
	}
	if err := r.checkSupersedes(ctx, tx, a.Tenant(), in.Supersedes); err != nil {
		return AdmitResult{}, err
	}
	if err := checkThreadRef(ctx, tx, a.Tenant(), in.Thread); err != nil {
		return AdmitResult{}, err
	}
	if !in.ConfirmNew {
		similar, err := r.Similar(ctx, tx, a.Tenant(), key, "")
		if err != nil {
			return AdmitResult{}, err
		}
		if len(similar) > 0 {
			r.step(ctx, a.Tenant(), StepRefusal)
			return AdmitResult{Similar: similar}, nil // nothing written
		}
	}

	id, err := r.insertTruth(ctx, tx, a, claim, key, basis, in.Supports, in.ChatID, nullable(in.Supersedes), nil)
	if err != nil {
		return AdmitResult{}, err
	}
	note := "gate passed"
	if in.ConfirmNew {
		note += " · confirm_new"
	}
	if err := addEvent(ctx, tx, a.Tenant(), id, "created", a.UserID, note); err != nil {
		return AdmitResult{}, err
	}
	if s := strings.TrimSpace(in.Supersedes); s != "" {
		if err := addEvent(ctx, tx, a.Tenant(), s, "superseded", a.UserID, "→ "+id); err != nil {
			return AdmitResult{}, err
		}
	}
	linked, err := r.linkWritten(ctx, tx, a, in.Thread, id, "admit")
	if err != nil {
		return AdmitResult{}, err
	}
	entry, err := r.ReadTruth(ctx, tx, a.Tenant(), id)
	if err != nil {
		return AdmitResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AdmitResult{}, err
	}
	r.step(ctx, a.Tenant(), StepWrite)
	return AdmitResult{Entry: &entry, Thread: linked}, nil
}

func (r *Repo) checkSupersedes(ctx context.Context, q querier, tenant, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	_, err := r.ReadTruth(ctx, q, tenant, strings.TrimSpace(id))
	return err
}

func (r *Repo) insertTruth(ctx context.Context, q querier, a Actor, claim, key string, b checkedBasis,
	supports []string, chatID string, supersedes, promotedFrom *string) (string, error) {
	num, err := nextID(ctx, q, a.Tenant(), "T")
	if err != nil {
		return "", err
	}
	var id string
	err = q.QueryRow(ctx, `
		INSERT INTO truth_ledger (tenant_id, num, claim, claim_key, basis, source, source_url, source_doc,
			source_entry, source_date, verified_at, valid_until,
			premises, derivation, supports, promoted_from, supersedes, signer, producer, grant_id, chat_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) RETURNING id`,
		a.Tenant(), num, claim, key, b.basis, b.source, b.sourceURL, b.sourceDoc,
		b.sourceEntry, b.sourceDate, time.Now().UTC(), b.validUntil,
		supportsOf(b.premises), b.derivation, supportsOf(supports), promotedFrom, supersedes,
		a.UserID, a.ActorID, a.GrantID, nullable(chatID)).Scan(&id)
	if err != nil {
		return "", err
	}
	// The document a fact is admitted on is its first evidence row, written in
	// the same transaction: the signer of the claim is the owner of that document
	// (spec T–D §4).
	if b.sourceDoc != nil {
		if err := attachEvidence(ctx, q, a, id, *b.sourceDoc, b.source, nil, "admit"); err != nil {
			return "", err
		}
	}
	return id, nil
}

// ---------- promote ----------

type PromoteInput struct {
	AssumptionID string `json:"assumption_id"`
	BasisInput
}

type PromoteResult struct {
	Entry      Truth
	Assumption Assumption
}

// Promote raises an A-p whose if: has been observed. The claim keeps its
// wording — a different sentence is a different claim, admitted on its own.
func (r *Repo) Promote(ctx context.Context, a Actor, in PromoteInput) (PromoteResult, error) {
	if err := r.checkSigner(a); err != nil {
		return PromoteResult{}, err
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return PromoteResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return PromoteResult{}, err
	}

	if err := r.writable(ctx, tx, a); err != nil {
		return PromoteResult{}, err
	}

	src, err := r.ReadAssumption(ctx, tx, a.Tenant(), strings.TrimSpace(in.AssumptionID))
	if err != nil {
		return PromoteResult{}, err
	}
	switch {
	case src.Status != "open":
		return PromoteResult{}, fail("NOT_OPEN", "%s is %s", src.ID, src.Status)
	case src.Type == "A-r":
		return PromoteResult{}, fail("NEVER_RAISED", "%s is a reading aid: it is deliberately never raised to T", src.ID)
	case src.Type == "A-c":
		return PromoteResult{}, fail("RESOLVE_BY_AMENDMENT", "%s is a chosen bet: it closes by amendment, not by observation", src.ID)
	case src.Type == "A-b":
		via := ""
		if src.Via != nil {
			via = *src.Via
		}
		return PromoteResult{}, failNext("CHANNEL_BLOCKED",
			"Build the channel first ("+via+"), then update_assumption("+src.ID+", type: \"A-p\"), then promote.",
			"%s is blocked: waiting longer resolves nothing, the channel is what is missing", src.ID)
	}
	// The wording carries over exactly, so it has to be admissible as a [T] now.
	// The fix is update_assumption while it is still open — which is why this is
	// a result with reasons, not a dead end.
	if check := CheckClaim(src.Claim, in.Basis); !check.Admissible {
		return PromoteResult{}, failClaim(check)
	}
	basis, err := r.checkBasis(ctx, tx, a, in.BasisInput)
	if err != nil {
		return PromoteResult{}, err
	}
	if id, _, err := exact(ctx, tx, a.Tenant(), "truth_ledger", src.ClaimKey); err != nil {
		return PromoteResult{}, err
	} else if id != "" {
		return PromoteResult{}, fail("ALREADY_T", "this claim already stands as %s", id)
	}

	id, err := r.insertTruth(ctx, tx, a, src.Claim, src.ClaimKey, basis, in.Supports, in.ChatID, nil, &src.ID)
	if err != nil {
		return PromoteResult{}, err
	}
	if err := addEvent(ctx, tx, a.Tenant(), id, "created", a.UserID, "promoted from "+src.ID); err != nil {
		return PromoteResult{}, err
	}
	seen := ""
	if src.If != nil {
		seen = " · if: " + *src.If
	}
	if err := addEvent(ctx, tx, a.Tenant(), src.ID, "promoted", a.UserID, "→ "+id+seen); err != nil {
		return PromoteResult{}, err
	}
	entry, err := r.ReadTruth(ctx, tx, a.Tenant(), id)
	if err != nil {
		return PromoteResult{}, err
	}
	closed, err := r.ReadAssumption(ctx, tx, a.Tenant(), src.ID)
	if err != nil {
		return PromoteResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PromoteResult{}, err
	}
	r.step(ctx, a.Tenant(), StepWrite)
	r.step(ctx, a.Tenant(), StepPromote)
	return PromoteResult{Entry: entry, Assumption: closed}, nil
}

// ---------- update_assumption ----------

type UpdateInput struct {
	AssumptionID string   `json:"assumption_id"`
	Claim        *string  `json:"claim"`
	Type         *string  `json:"type"`
	Source       *string  `json:"source"`
	If           *string  `json:"if"`
	Not          *string  `json:"not"`
	Via          *string  `json:"via"`
	Supports     []string `json:"supports"`
	ConfirmNew   bool     `json:"confirm_new"`
}

type UpdateResult struct {
	Entry   *Assumption
	Similar []Similar
}

// UpdateAssumption edits an assumption while it is still open. The old value
// stays in history; nothing is quietly replaced.
func (r *Repo) UpdateAssumption(ctx context.Context, a Actor, in UpdateInput) (UpdateResult, error) {
	if err := r.checkSigner(a); err != nil {
		return UpdateResult{}, err
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return UpdateResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, a.Tenant()); err != nil {
		return UpdateResult{}, err
	}

	if err := r.writable(ctx, tx, a); err != nil {
		return UpdateResult{}, err
	}

	cur, err := r.ReadAssumption(ctx, tx, a.Tenant(), strings.TrimSpace(in.AssumptionID))
	if err != nil {
		return UpdateResult{}, err
	}
	if cur.Status != "open" {
		return UpdateResult{}, fail("NOT_OPEN", "%s is %s; its wording is part of the record now", cur.ID, cur.Status)
	}

	next := cur
	changes := []string{}
	if in.Claim != nil && CleanClaim(*in.Claim) != cur.Claim {
		claim := CleanClaim(*in.Claim)
		if claim == "" {
			return UpdateResult{}, fail("EMPTY_CLAIM", "claim cannot be emptied")
		}
		if check := CheckClaim(claim, ""); !check.Admissible {
			return UpdateResult{}, failClaim(check)
		}
		if err := claimLimits(ctx, tx, a.Tenant(), claim); err != nil {
			return UpdateResult{}, err
		}
		next.Claim, next.ClaimKey = claim, ClaimKey(claim)
		changes = append(changes, fmt.Sprintf("claim: %q", cur.Claim))
	}
	if in.Type != nil {
		typ, ok := normalizeType(*in.Type)
		if !ok {
			return UpdateResult{}, fail("INVALID_TYPE", "type must be one of %s", strings.Join(assumptionTypes, ", "))
		}
		if typ != cur.Type {
			next.Type = typ
			changes = append(changes, "type: "+cur.Type)
		}
	}
	if in.Source != nil {
		src, err := ParseSource(*in.Source)
		if err != nil {
			return UpdateResult{}, err
		}
		if src.String() != cur.Source.String() {
			next.Source = src
			changes = append(changes, "source: "+cur.Source.String())
		}
	}
	// A new sentence needs its way out restated: an observation written for the
	// old wording usually no longer settles the new one (owner decision, 25/9).
	if next.Claim != cur.Claim {
		switch next.Type {
		case "A-p":
			if in.If == nil || in.Not == nil {
				return UpdateResult{}, fail("RESTATE_TAIL",
					"the claim changed: an observation written for the old wording usually no longer raises or refutes the new one")
			}
		case "A-c", "A-b":
			if in.Via == nil {
				return UpdateResult{}, fail("RESTATE_TAIL", "the claim changed: state via: again for the new wording")
			}
		}
	}
	tailOf := func(p *string, cur *string) string {
		if p != nil {
			return *p
		}
		if cur != nil {
			return *cur
		}
		return ""
	}
	ifObs, notObs, via, err := tails(next.Type, tailOf(in.If, cur.If), tailOf(in.Not, cur.Not), tailOf(in.Via, cur.Via))
	if err != nil {
		return UpdateResult{}, err
	}
	for label, pair := range map[string][2]*string{"if": {ifObs, cur.If}, "not": {notObs, cur.Not}, "via": {via, cur.Via}} {
		if derefOr(pair[0]) != derefOr(pair[1]) {
			changes = append(changes, label+": "+derefOr(pair[1]))
		}
	}
	next.If, next.Not, next.Via = ifObs, notObs, via
	if in.Supports != nil {
		next.Supports = in.Supports
		changes = append(changes, "supports")
	}
	if len(changes) == 0 {
		return UpdateResult{Entry: &cur}, nil
	}

	if next.ClaimKey != cur.ClaimKey {
		if id, _, err := exact(ctx, tx, a.Tenant(), "truth_ledger", next.ClaimKey); err != nil {
			return UpdateResult{}, err
		} else if id != "" {
			return UpdateResult{}, fail("ALREADY_T", "that wording already stands as %s", id)
		}
		if id, _, err := exact(ctx, tx, a.Tenant(), "assumption", next.ClaimKey); err != nil {
			return UpdateResult{}, err
		} else if id != "" && id != cur.ID {
			return UpdateResult{}, fail("DUPLICATE", "%s already holds that wording", id)
		}
		if !in.ConfirmNew {
			similar, err := r.Similar(ctx, tx, a.Tenant(), next.ClaimKey, cur.ID)
			if err != nil {
				return UpdateResult{}, err
			}
			if len(similar) > 0 {
				return UpdateResult{Similar: similar}, nil // nothing written
			}
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE assumption SET claim = $2, claim_key = $3, type = $4, source_kind = $5, source_detail = $6,
			if_obs = $7, not_obs = $8, via = $9, supports = $10, revision = revision + 1
		WHERE id = $1 AND tenant_id = $11`,
		cur.ID, next.Claim, next.ClaimKey, next.Type, next.Source.Kind, next.Source.Detail,
		next.If, next.Not, next.Via, supportsOf(next.Supports), a.Tenant()); err != nil {
		return UpdateResult{}, err
	}
	if err := addEvent(ctx, tx, a.Tenant(), cur.ID, "updated", a.UserID, strings.Join(changes, " · ")); err != nil {
		return UpdateResult{}, err
	}
	entry, err := r.ReadAssumption(ctx, tx, a.Tenant(), cur.ID)
	if err != nil {
		return UpdateResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return UpdateResult{}, err
	}
	return UpdateResult{Entry: &entry}, nil
}

func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
