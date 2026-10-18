package tka

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The instruction catalog (owner 14/10/2026, spec-instruction-catalog-14-10-26.md):
// best practices T-knowledge proposes, curated from what people report, which
// a person chooses to take into their own store. It is a store of its own —
// tenant `catalog` — whose instructions are the entries, so it keeps what every
// store keeps: rows are only added, a new wording supersedes, a withdrawal is
// a retirement with its reason. An entry is named BP-<n> after its number
// there, and it is written in English; a person who takes one keeps that
// English word for word and a translation beside it in their own language.
//
// An entry nobody has approved is proposed: the maintainer reads it, and
// people do not see it. A store is offered only approved entries, and an offer
// is data until the person takes it — help and instructions() show offers,
// they never follow them.

// CatalogTenant is the tenant the catalog lives in.
const CatalogTenant = "catalog"

// Catalog entry statuses.
const (
	CatalogProposed   = "proposed"
	CatalogCurrent    = "current"
	CatalogRejected   = "rejected"
	CatalogSuperseded = "superseded"
	CatalogWithdrawn  = "withdrawn"
)

// CatalogEntry is one entry as it reads now.
type CatalogEntry struct {
	ID           string          `json:"id"` // BP-…
	Kind         string          `json:"kind"`
	Title        *string         `json:"title,omitempty"`
	Term         *string         `json:"term,omitempty"`
	Text         string          `json:"text"`
	Why          *string         `json:"definition_why,omitempty"`
	RationaleRef *string         `json:"rationale_ref,omitempty"`
	Tags         InstructionTags `json:"tags"`
	Evidence     *string         `json:"evidence,omitempty"`
	Starter      bool            `json:"starter"`
	Status       string          `json:"status"`
	Supersedes   *string         `json:"supersedes,omitempty"`    // BP-…
	SupersededBy *string         `json:"superseded_by,omitempty"` // BP-…
	Reason       *string         `json:"reason,omitempty"`        // why it was withdrawn or rejected
	// Source names the store the entry came from without naming the person
	// ("a founder's store, UAE"), and IncidentOn is the day of the incident
	// that produced it — both given when the entry is approved, or carried
	// from the entry it supersedes.
	Source     *string `json:"source,omitempty"`
	IncidentOn *string `json:"incident_on,omitempty"` // YYYY-MM-DD
	// Public: the entry passes the gate (A-157) and the public catalog shows
	// it. A definition is a term, not a practice, and needs no incident; a
	// directive needs its incident, dated, and its source. A starter that
	// does not pass still begins every new store — it is the default list —
	// but is not published.
	Public bool `json:"public"`
	// HeldBack says, for the maintainer, what keeps a current entry out of
	// the public catalog.
	HeldBack string `json:"held_back,omitempty"`
	// ReviewNote is for the maintainer (e.g. "too private to publish as is");
	// the public catalog leaves it out.
	ReviewNote *string    `json:"review_note,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`

	num int64
}

// bp is the public id of the catalog row I-n.
func bp(instructionID string) string {
	n, ok := strings.CutPrefix(instructionID, "I-")
	if !ok {
		return instructionID
	}
	return "BP-" + n
}

// canonBP reads BP-12 (any case, spaces around) into BP-12, or "".
func canonBP(raw string) string {
	id := strings.ToUpper(strings.TrimSpace(raw))
	n, ok := strings.CutPrefix(id, "BP-")
	if !ok {
		return ""
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil || v <= 0 {
		return ""
	}
	return "BP-" + strconv.FormatInt(v, 10)
}

func catalogRowID(bpID string) string { return "I-" + strings.TrimPrefix(bpID, "BP-") }

// catalog is the whole catalog, every status, in number order.
type catalog struct {
	entries []CatalogEntry
	byID    map[string]*CatalogEntry
}

func (c catalog) get(id string) *CatalogEntry {
	if c.byID == nil {
		return nil
	}
	return c.byID[canonBP(id)]
}

// starters are the approved entries a new store begins with.
func (c catalog) starters() []CatalogEntry {
	var out []CatalogEntry
	for _, e := range c.entries {
		if e.Starter && e.Status == CatalogCurrent {
			out = append(out, e)
		}
	}
	return out
}

// linked is the entry a store's line comes from: the one it names, else one
// whose English it repeats word for word (a store seeded before the catalog
// existed, or a line the person wrote that the catalog later took up).
func (c catalog) linked(in Instruction) *CatalogEntry {
	if in.CatalogRef != nil {
		return c.get(*in.CatalogRef)
	}
	for i := range c.entries {
		if c.entries[i].Status != CatalogProposed && c.entries[i].Status != CatalogRejected && c.entries[i].Text == in.Text {
			return &c.entries[i]
		}
	}
	return nil
}

// ensureCatalog writes the catalog's first entries — the default list, every
// line a starter, approved — the first time the catalog is needed. It is the
// deployment's own act, done once; afterwards it does nothing.
func (r *Repo) ensureCatalog(ctx context.Context) error {
	return r.withTenant(ctx, CatalogTenant, func(q querier) error {
		if err := lockInstructions(ctx, q, CatalogTenant); err != nil {
			return err
		}
		var any bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM instruction WHERE tenant_id = $1)`, CatalogTenant).Scan(&any); err != nil {
			return err
		}
		if any {
			return nil
		}
		for i, d := range DefaultInstructions() {
			id := "I-" + strconv.Itoa(i+1)
			if _, err := q.Exec(ctx, `
				INSERT INTO instruction (tenant_id, num, kind, label, text, origin, title, grp, term, why, rationale_ref,
				                         starter, signer, producer, grant_id)
				VALUES ($1, $2, $3, $4, $5, 'default', $6, $7, $8, $9, $10, true, $11, $12, $13)`,
				CatalogTenant, i+1, d.Kind, d.Label, d.Text, nullable(d.Title), nullable(d.Group), nullable(d.Term),
				nullable(d.Why), nullable(d.RationaleRef), seedSigner, seedProducer, seedGrant); err != nil {
				return err
			}
			if t, ok := defaultTagsFor(Instruction{Origin: "default", Label: d.Label, Text: d.Text}); ok {
				if err := insertTags(ctx, q, Actor{UserID: seedSigner, ActorID: seedProducer, GrantID: seedGrant, TenantID: CatalogTenant}, id, t); err != nil {
					return err
				}
			}
			if _, err := q.Exec(ctx, `INSERT INTO catalog_review (instruction_id, decision, note, signer, producer, grant_id)
				VALUES ($1, 'approve', 'the default instruction list, taken into the catalog as its starters', $2, $3, $4)`,
				id, seedSigner, seedProducer, seedGrant); err != nil {
				return err
			}
		}
		return nil
	})
}

// loadCatalog reads the catalog, every status.
func (r *Repo) loadCatalog(ctx context.Context) (catalog, error) {
	if err := r.ensureCatalog(ctx); err != nil {
		return catalog{}, err
	}
	var c catalog
	err := r.withTenant(ctx, CatalogTenant, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+instructionColumns+`, i.num,
			(SELECT decision FROM catalog_review v WHERE v.instruction_id = i.id ORDER BY v.id DESC LIMIT 1),
			(SELECT note FROM catalog_review v WHERE v.instruction_id = i.id ORDER BY v.id DESC LIMIT 1),
			(SELECT min(at) FROM catalog_review v WHERE v.instruction_id = i.id AND decision = 'approve'),
			(SELECT source FROM catalog_review v WHERE v.instruction_id = i.id AND decision = 'approve' ORDER BY v.id DESC LIMIT 1),
			(SELECT to_char(incident_on, 'YYYY-MM-DD') FROM catalog_review v WHERE v.instruction_id = i.id AND decision = 'approve' ORDER BY v.id DESC LIMIT 1)
			`+instructionFrom+` WHERE i.tenant_id = $1 ORDER BY i.num`, CatalogTenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		var list []Instruction
		type extra struct {
			num        int64
			decision   *string
			note       *string
			approvedAt *time.Time
			source     *string
			incidentOn *string
		}
		var extras []extra
		for rows.Next() {
			var x extra
			in, err := scanInstruction(rows, &x.num, &x.decision, &x.note, &x.approvedAt, &x.source, &x.incidentOn)
			if err != nil {
				return err
			}
			list = append(list, in)
			extras = append(extras, x)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := attachOwnTags(ctx, q, CatalogTenant, list); err != nil {
			return err
		}
		for i, in := range list {
			x := extras[i]
			e := CatalogEntry{ID: bp(in.ID), Kind: in.Kind, Title: in.Title, Term: in.Term, Text: in.Text, Why: in.Why,
				RationaleRef: in.RationaleRef, Tags: in.Tags, Evidence: in.Evidence, Starter: in.Starter,
				ReviewNote: in.ReviewNote, CreatedAt: in.CreatedAt, ApprovedAt: x.approvedAt, Source: x.source, IncidentOn: x.incidentOn,
				num: x.num}
			if in.Supersedes != nil {
				s := bp(*in.Supersedes)
				e.Supersedes = &s
			}
			switch {
			case in.Retired != nil && in.Retired.SupersededBy != nil:
				e.Status = CatalogSuperseded
				s := bp(*in.Retired.SupersededBy)
				e.SupersededBy, e.Reason = &s, &in.Retired.Reason
			case in.Retired != nil:
				e.Status, e.Reason = CatalogWithdrawn, &in.Retired.Reason
			case x.decision == nil:
				e.Status = CatalogProposed
			case *x.decision == "approve":
				e.Status = CatalogCurrent
			default:
				e.Status, e.Reason = CatalogRejected, x.note
			}
			e.Tags.Source = ""
			c.entries = append(c.entries, e)
		}
		return nil
	})
	if err != nil {
		return catalog{}, err
	}
	c.byID = make(map[string]*CatalogEntry, len(c.entries))
	for i := range c.entries {
		c.byID[c.entries[i].ID] = &c.entries[i]
	}
	// In number order an ancestor is settled before the entries that
	// supersede it: a new wording carries its incident and source.
	for i := range c.entries {
		e := &c.entries[i]
		if p := c.parent(*e); p != nil {
			if e.Evidence == nil {
				e.Evidence = p.Evidence
			}
			if e.Source == nil && e.IncidentOn == nil {
				e.Source, e.IncidentOn = p.Source, p.IncidentOn
			}
		}
		e.HeldBack = heldBack(*e)
		e.Public = e.Status != CatalogProposed && e.Status != CatalogRejected && e.HeldBack == ""
		if e.Status != CatalogCurrent {
			e.HeldBack = ""
		}
	}
	return c, nil
}

func (c catalog) parent(e CatalogEntry) *CatalogEntry {
	if e.Supersedes == nil {
		return nil
	}
	return c.get(*e.Supersedes)
}

// heldBack is what the gate (A-157) still misses for e, or "".
func heldBack(e CatalogEntry) string {
	if e.Kind == "definition" {
		return ""
	}
	var miss []string
	if e.Evidence == nil {
		miss = append(miss, "no incident")
	}
	if e.IncidentOn == nil {
		miss = append(miss, "no incident date")
	}
	if e.Source == nil {
		miss = append(miss, "no source")
	}
	return strings.Join(miss, ", ")
}

// CatalogFilter is what a catalog read asks for.
type CatalogFilter struct {
	// Statuses to return; empty = current only.
	Statuses []string
	// Situation narrows to the entries help would bring up for it.
	Situation string
	// Maintainer keeps review notes in; the public read leaves them out.
	Maintainer bool
}

// Catalog is the catalog as a reader sees it: the public read holds only
// the entries that pass the gate.
func (r *Repo) Catalog(ctx context.Context, f CatalogFilter) ([]CatalogEntry, error) {
	c, err := r.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	statuses := f.Statuses
	if len(statuses) == 0 {
		statuses = []string{CatalogCurrent}
	}
	var facets map[string][]string
	var named []string
	if strings.TrimSpace(f.Situation) != "" {
		var terms []string
		for _, e := range c.entries {
			if e.Term != nil {
				terms = append(terms, *e.Term)
			}
		}
		facets, _, named = readSituation(f.Situation, terms)
	}
	out := []CatalogEntry{}
	for _, e := range c.entries {
		if !slices.Contains(statuses, e.Status) {
			continue
		}
		if facets != nil && !offerFits(e, facets, named) {
			continue
		}
		if !f.Maintainer {
			if !e.Public {
				continue
			}
			e.ReviewNote, e.HeldBack = nil, ""
		}
		out = append(out, e)
	}
	return out, nil
}

// offerFits is help's rule for one catalog entry.
func offerFits(e CatalogEntry, facets map[string][]string, named []string) bool {
	asksMeaning := slices.Contains(facets["what"], "term")
	if e.Kind == "meta" {
		return false
	}
	if e.Kind == "definition" {
		return asksMeaning && (len(named) == 0 || (e.Term != nil && slices.Contains(named, *e.Term)))
	}
	if asksMeaning && len(named) > 0 && len(facets["when"]) == 0 {
		return false
	}
	situational := len(facets["when"]) > 0 || len(facets["where"]) > 0 || len(facets["what"]) > 0
	return situational && !e.Tags.empty() && fits(e.Tags, facets)
}

// --- the maintainer -----------------------------------------------------

// CatalogAddInput proposes one entry. Nobody sees it until it is approved.
type CatalogAddInput struct {
	Kind         string          `json:"kind"`
	Text         string          `json:"text"`
	Title        string          `json:"title"`
	Term         string          `json:"term"`
	Why          string          `json:"why"`
	RationaleRef string          `json:"rationale_ref"`
	Tags         InstructionTags `json:"tags"`
	Evidence     string          `json:"evidence"`
	Starter      bool            `json:"starter"`
	// Supersedes is the BP-… this entry would replace once approved.
	Supersedes string `json:"supersedes"`
	ReviewNote string `json:"review_note"`
}

// CatalogAdd proposes an entry.
func (r *Repo) CatalogAdd(ctx context.Context, a Actor, in CatalogAddInput) (CatalogEntry, error) {
	if err := r.checkSigner(a); err != nil {
		return CatalogEntry{}, err
	}
	text := CleanClaim(in.Text)
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = "directive"
	}
	switch {
	case text == "":
		return CatalogEntry{}, failNext("EMPTY_INSTRUCTION", "Write the entry in English.", "a catalog entry needs text")
	case kind != "directive" && kind != "meta" && kind != "definition":
		return CatalogEntry{}, failNext("INVALID_INSTRUCTION", "kind is directive (the default), meta or definition.", "unknown kind %q", in.Kind)
	case kind == "definition" && (strings.TrimSpace(in.Term) == "" || strings.TrimSpace(in.Why) == ""):
		return CatalogEntry{}, failNext("INVALID_INSTRUCTION", "Give the term and why in one sentence.", "a definition needs a term and a why")
	case kind != "definition" && (strings.TrimSpace(in.Term) != "" || strings.TrimSpace(in.Why) != ""):
		return CatalogEntry{}, failNext("INVALID_INSTRUCTION", "Leave term and why out, or make it a definition.", "term and why belong to a definition")
	case in.Tags.empty():
		return CatalogEntry{}, failNext("INVALID_TAG", "Tag the entry so help can offer it: who, what, when, where, why, how.", "a catalog entry needs tags")
	}
	if err := validTags(in.Tags); err != nil {
		return CatalogEntry{}, err
	}
	c, err := r.loadCatalog(ctx)
	if err != nil {
		return CatalogEntry{}, err
	}
	var sup *string
	if strings.TrimSpace(in.Supersedes) != "" {
		old := c.get(in.Supersedes)
		if old == nil {
			return CatalogEntry{}, failNext("CATALOG_UNKNOWN", "tk catalog list --status all shows every entry.", "%s is not in the catalog", in.Supersedes)
		}
		if old.Status != CatalogCurrent {
			return CatalogEntry{}, failNext("CATALOG_NOT_CURRENT", "Only a current entry can be replaced.", "%s is %s", old.ID, old.Status)
		}
		s := catalogRowID(old.ID)
		sup = &s
	}
	for _, e := range c.entries {
		if e.Text == text && e.Status != CatalogRejected {
			return CatalogEntry{}, failNext("CATALOG_DUPLICATE", "The catalog already has these words.", "%s says this already (%s)", e.ID, e.Status)
		}
	}
	var id string
	err = r.withTenant(ctx, CatalogTenant, func(q querier) error {
		if err := lockInstructions(ctx, q, CatalogTenant); err != nil {
			return err
		}
		var num int64
		if err := q.QueryRow(ctx, `SELECT coalesce(max(num), 0) + 1 FROM instruction WHERE tenant_id = $1`, CatalogTenant).Scan(&num); err != nil {
			return err
		}
		label, err := nextLabel(ctx, q, CatalogTenant, kind, strings.TrimSpace(in.Term))
		if err != nil {
			return err
		}
		if err := q.QueryRow(ctx, `
			INSERT INTO instruction (tenant_id, num, kind, label, text, origin, title, term, why, rationale_ref, supersedes,
			                         starter, evidence, review_note, signer, producer, grant_id)
			VALUES ($1, $2, $3, $4, $5, 'added', $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING id`,
			CatalogTenant, num, kind, label, text, nilIfEmpty(strings.TrimSpace(in.Title)), nilIfEmpty(strings.TrimSpace(in.Term)),
			nilIfEmpty(strings.TrimSpace(in.Why)), nilIfEmpty(strings.TrimSpace(in.RationaleRef)), sup, in.Starter,
			nilIfEmpty(strings.TrimSpace(in.Evidence)), nilIfEmpty(strings.TrimSpace(in.ReviewNote)),
			a.UserID, a.ActorID, a.GrantID).Scan(&id); err != nil {
			return err
		}
		in.Tags.Source = ""
		return insertTags(ctx, q, Actor{UserID: a.UserID, ActorID: a.ActorID, GrantID: a.GrantID, TenantID: CatalogTenant}, id, in.Tags)
	})
	if err != nil {
		return CatalogEntry{}, err
	}
	return r.catalogEntry(ctx, bp(id))
}

func (r *Repo) catalogEntry(ctx context.Context, id string) (CatalogEntry, error) {
	c, err := r.loadCatalog(ctx)
	if err != nil {
		return CatalogEntry{}, err
	}
	e := c.get(id)
	if e == nil {
		return CatalogEntry{}, failNext("CATALOG_UNKNOWN", "tk catalog list --status all shows every entry.", "%s is not in the catalog", id)
	}
	return *e, nil
}

// CatalogReviewInput is the maintainer's decision on a proposed entry.
type CatalogReviewInput struct {
	ID       string `json:"id"`
	Decision string `json:"decision"` // approve | reject
	Note     string `json:"note"`
	// Source and IncidentOn (YYYY-MM-DD) are given with an approval: the
	// store the entry came from, named without the person, and the day of
	// its incident. Approving says the entry ran active in that store, that
	// its signer agreed and that it holds nothing company-specific (A-157).
	Source     string `json:"source"`
	IncidentOn string `json:"incident_on"`
}

// CatalogReview approves or rejects a proposed entry. Approving one that
// supersedes another retires the old one in the same write, pointing at it.
// An entry that is not a starter is approved only through the gate: a
// directive with its incident, dated, and its source — given now or carried
// from the entry it supersedes; a definition needs neither.
func (r *Repo) CatalogReview(ctx context.Context, a Actor, in CatalogReviewInput) (CatalogEntry, error) {
	if err := r.checkSigner(a); err != nil {
		return CatalogEntry{}, err
	}
	decision, note := in.Decision, in.Note
	source, on := strings.TrimSpace(in.Source), strings.TrimSpace(in.IncidentOn)
	if decision != "approve" && decision != "reject" {
		return CatalogEntry{}, failNext("INVALID_DECISION", "approve or reject.", "unknown decision %q", decision)
	}
	if decision == "reject" && strings.TrimSpace(note) == "" {
		return CatalogEntry{}, fail("EMPTY_REASON", "a rejection says why")
	}
	if decision == "reject" && (source != "" || on != "") {
		return CatalogEntry{}, failNext("INVALID_DECISION", "Leave source and incident date out of a rejection.", "a rejection carries only its reason")
	}
	if on != "" {
		d, err := time.Parse("2006-01-02", on)
		if err != nil || d.After(time.Now()) {
			return CatalogEntry{}, failNext("INVALID_DATE", "The incident date is YYYY-MM-DD, not in the future.", "incident date %q", on)
		}
	}
	c, err := r.loadCatalog(ctx)
	if err != nil {
		return CatalogEntry{}, err
	}
	e := c.get(in.ID)
	if e == nil {
		return CatalogEntry{}, failNext("CATALOG_UNKNOWN", "tk catalog list --status all shows every entry.", "%s is not in the catalog", in.ID)
	}
	if e.Status != CatalogProposed && e.Status != CatalogRejected {
		return CatalogEntry{}, failNext("CATALOG_NOT_PROPOSED", "A current entry leaves the catalog with withdraw, or a new entry supersedes it.",
			"%s is %s", e.ID, e.Status)
	}
	if decision == "approve" && !e.Starter && e.Kind != "definition" {
		inherited := source == "" && on == "" && e.Source != nil && e.IncidentOn != nil
		switch {
		case e.Evidence == nil:
			return CatalogEntry{}, failNext("CATALOG_NO_INCIDENT", "Reject it, or propose it again with the incident that produced it.",
				"%s carries no incident", e.ID)
		case !inherited && (source == "" || on == ""):
			return CatalogEntry{}, failNext("CATALOG_GATE",
				`Give the store it came from without naming the person and the incident's date: --source "a founder's store, UAE" --on 2026-10-13.`,
				"%s needs its source and incident date to be approved", e.ID)
		}
	}
	err = r.withTenant(ctx, CatalogTenant, func(q querier) error {
		if _, err := q.Exec(ctx, `INSERT INTO catalog_review (instruction_id, decision, note, source, incident_on, signer, producer, grant_id)
			VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8)`, catalogRowID(e.ID), decision, nilIfEmpty(strings.TrimSpace(note)),
			nilIfEmpty(source), nilIfEmpty(on), a.UserID, a.ActorID, a.GrantID); err != nil {
			return err
		}
		if decision == "approve" && e.Supersedes != nil {
			by := catalogRowID(e.ID)
			return retireInstruction(ctx, q, Actor{UserID: a.UserID, ActorID: a.ActorID, GrantID: a.GrantID, TenantID: CatalogTenant},
				catalogRowID(*e.Supersedes), "superseded by "+e.ID, &by)
		}
		return nil
	})
	if err != nil {
		return CatalogEntry{}, err
	}
	return r.catalogEntry(ctx, e.ID)
}

// CatalogWithdraw takes a current entry out of the catalog. Stores that took
// it keep their line and are told.
func (r *Repo) CatalogWithdraw(ctx context.Context, a Actor, id, reason string) (CatalogEntry, error) {
	if err := r.checkSigner(a); err != nil {
		return CatalogEntry{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return CatalogEntry{}, fail("EMPTY_REASON", "a withdrawal says why")
	}
	e, err := r.catalogEntry(ctx, id)
	if err != nil {
		return CatalogEntry{}, err
	}
	if e.Status != CatalogCurrent {
		return CatalogEntry{}, failNext("CATALOG_NOT_CURRENT", "Only a current entry is withdrawn.", "%s is %s", e.ID, e.Status)
	}
	err = r.withTenant(ctx, CatalogTenant, func(q querier) error {
		return retireInstruction(ctx, q, Actor{UserID: a.UserID, ActorID: a.ActorID, GrantID: a.GrantID, TenantID: CatalogTenant},
			catalogRowID(e.ID), strings.TrimSpace(reason), nil)
	})
	if err != nil {
		return CatalogEntry{}, err
	}
	return r.catalogEntry(ctx, e.ID)
}

// --- a store and the catalog -------------------------------------------

// CatalogOffers is what the catalog has for one store: entries it has not
// taken or declined, newer wordings of entries it holds, and entries it holds
// that the catalog withdrew. Offers, not instructions: the person decides.
type CatalogOffers struct {
	New       []CatalogEntry  `json:"new"`
	Updates   []CatalogUpdate `json:"updates"`
	Withdrawn []CatalogNotice `json:"withdrawn"`
	Next      string          `json:"next,omitempty"`
}

// CatalogUpdate is a newer wording of an entry the store holds.
type CatalogUpdate struct {
	Line  string       `json:"line"` // the store's I-…
	Was   string       `json:"was"`  // BP-… it holds
	Entry CatalogEntry `json:"entry"`
}

// CatalogNotice is a store line whose entry the catalog withdrew.
type CatalogNotice struct {
	Line   string `json:"line"`
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

// present is every entry a store has met — taken, declined, or held under an
// older id — and the store line holding each live one.
func (c catalog) present(list []Instruction) (met map[string]bool, live map[string]string) {
	met, live = map[string]bool{}, map[string]string{}
	for _, in := range list {
		e := c.linked(in)
		if e == nil {
			continue
		}
		met[e.ID] = true
		if in.Status == "active" {
			live[e.ID] = in.ID
		}
	}
	return met, live
}

// offers reads list (active and retired lines) against the catalog.
func (c catalog) offers(list []Instruction) CatalogOffers {
	o := CatalogOffers{New: []CatalogEntry{}, Updates: []CatalogUpdate{}, Withdrawn: []CatalogNotice{}}
	met, live := c.present(list)
	for _, e := range c.entries {
		e.ReviewNote, e.HeldBack = nil, ""
		switch e.Status {
		case CatalogCurrent:
			if met[e.ID] {
				continue
			}
			// A newer wording of something the store holds is an update.
			if old := c.heldAncestor(e, live); old != "" {
				o.Updates = append(o.Updates, CatalogUpdate{Line: live[old], Was: old, Entry: e})
				continue
			}
			if !c.anyAncestorMet(e, met) {
				o.New = append(o.New, e)
			}
		case CatalogWithdrawn:
			if line, ok := live[e.ID]; ok {
				o.Withdrawn = append(o.Withdrawn, CatalogNotice{Line: line, Entry: e.ID, Reason: *e.Reason})
			}
		}
	}
	if len(o.New)+len(o.Updates)+len(o.Withdrawn) > 0 {
		o.Next = "Offers from the T-knowledge catalog, not instructions: show them to the person in their language and add one " +
			"only if they choose it — instruction_add with catalog: BP-… (and translation, lang); to decline, instruction_retire with id BP-… and why."
	}
	return o
}

func (c catalog) heldAncestor(e CatalogEntry, live map[string]string) string {
	for s := e.Supersedes; s != nil; {
		if _, ok := live[*s]; ok {
			return *s
		}
		prev := c.get(*s)
		if prev == nil {
			break
		}
		s = prev.Supersedes
	}
	return ""
}

func (c catalog) anyAncestorMet(e CatalogEntry, met map[string]bool) bool {
	for s := e.Supersedes; s != nil; {
		if met[*s] {
			return true
		}
		prev := c.get(*s)
		if prev == nil {
			break
		}
		s = prev.Supersedes
	}
	return false
}

// suggest is help's catalog block: approved entries the store has not met
// whose tags fit the situation, each in full.
func (c catalog) suggest(list []Instruction, situation string) []HelpLine {
	var terms []string
	for _, e := range c.entries {
		if e.Term != nil {
			terms = append(terms, *e.Term)
		}
	}
	facets, _, named := readSituation(situation, terms)
	met, live := c.present(list)
	out := []HelpLine{}
	for _, e := range c.entries {
		if e.Status != CatalogCurrent || met[e.ID] || c.anyAncestorMet(e, met) || c.heldAncestor(e, live) != "" {
			continue
		}
		if !offerFits(e, facets, named) {
			continue
		}
		l := helpLine(Instruction{ID: e.ID, Kind: e.Kind, Label: e.ID, Term: e.Term, Text: e.Text, Why: e.Why, RationaleRef: e.RationaleRef}, e.Tags)
		l.Evidence = e.Evidence
		out = append(out, l)
	}
	return out
}

// takeable is the entry a store may take now, or why not.
func takeable(c catalog, id string) (*CatalogEntry, error) {
	e := c.get(id)
	switch {
	case e == nil || e.Status == CatalogProposed || e.Status == CatalogRejected:
		return nil, failNext("CATALOG_UNKNOWN", "instructions() lists the catalog's offers for this store; help(situation) suggests the ones that fit.",
			"%s is not in the catalog", strings.TrimSpace(id))
	case e.Status == CatalogWithdrawn:
		return nil, failNext("CATALOG_WITHDRAWN", "The catalog withdrew it: "+*e.Reason, "%s was withdrawn", e.ID)
	case e.Status == CatalogSuperseded:
		return nil, failNext("CATALOG_SUPERSEDED", "Take "+*e.SupersededBy+", its current wording.", "%s was replaced by %s", e.ID, *e.SupersededBy)
	}
	return e, nil
}
