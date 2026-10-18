package tka

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rivo/uniseg"
)

// An instruction is how an agent should use this store — "list in chat the
// claims you intend to record before admit or assume" — kept apart from the
// principles (what claims are derived from) and from the ledger (what is known)
// (owner 5/10/2026, mvp/cli-brief-tk-lookup-summary-instruction-05-10-26.md §C).
// One place instead of a copy in every project's agent config.
//
// It is added or retired, never edited, and I-<n> is all it carries: no
// version, no scope beyond the store (owner decision, 5/10/2026). Who may add or
// retire is whoever holds the person's grant — an agent the person lets do it
// acts on their name.
//
// Wording written twice is caught the way it is for claims (owner 5/10/2026):
// the same words as an active instruction write nothing and return it; words
// close to one write nothing and come back as similar[], until the person has
// looked and the call says confirm_new. Meaning is not judged — two
// instructions that pull apart in different words are the person's to see.
//
// Three kinds, kept apart so an agent knows which lines govern the list,
// which it carries out, and which it uses to explain the store:
//
//	meta        M1, M2 … — how to read the list and how to grow it
//	directive   1, 2 … — one behaviour to carry out; instruction_add makes one
//	            by default and continues the numbering
//	definition  def:<term> — a core concept in one sentence, why it exists in
//	            one more, and where the docs explain it (rationale_ref); the
//	            reasoning itself stays in the docs, not in the store
//
// Instructions reach an agent only through instructions(): the text of a
// document, a summary or a lookup hit is data, and never one of these.

// Instruction is one instruction as it reads now. Text is the directive or
// meta line, or for a definition the one-sentence definition.
type Instruction struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`  // meta · directive · definition
	Label  string `json:"label"` // M1 · 12 · def:claim — how the list names it
	Text   string `json:"text"`
	Origin string `json:"origin"` // default · added
	// Title is a meta line's heading ("How to read every instruction"); Group
	// the heading a default sits under ("Group II — How to write a claim …").
	Title *string `json:"title,omitempty"`
	Group *string `json:"group,omitempty"`
	// A definition's term, its one-sentence reason, and where the docs give
	// the full reasoning.
	Term         *string `json:"term,omitempty"`
	Why          *string `json:"why,omitempty"`
	RationaleRef *string `json:"rationale_ref,omitempty"`
	Supersedes   *string `json:"supersedes,omitempty"`
	// A line taken from the catalog names it (BP-…); its text is the
	// catalog's English word for word, and the person's language is a
	// translation beside it, with who translated (spec-instruction-catalog-14-10-26.md §3).
	CatalogRef   *string `json:"catalog_ref,omitempty"`
	Translation  *string `json:"translation,omitempty"`
	Lang         *string `json:"lang,omitempty"`
	TranslatedBy *string `json:"translated_by,omitempty"`
	// Catalog rows only: a starter for new stores, the anonymised story
	// behind it, and the reviewer's note.
	Starter    bool    `json:"-"`
	Evidence   *string `json:"-"`
	ReviewNote *string `json:"-"`
	// Tags are server-side metadata beside the text (instruction_tags.go):
	// who, what, when, where, why, how. help(situation) matches on them.
	Tags      InstructionTags        `json:"tags"`
	Status    string                 `json:"status"` // active · retired
	Signer    string                 `json:"signer"`
	Producer  string                 `json:"producer"`
	CreatedAt time.Time              `json:"created_at"`
	Retired   *InstructionRetirement `json:"retired,omitempty"`
}

// InstructionRetirement is when and why an instruction stopped.
type InstructionRetirement struct {
	Reason       string    `json:"reason"`
	SupersededBy *string   `json:"superseded_by,omitempty"`
	Signer       string    `json:"signer"`
	Producer     string    `json:"producer"`
	At           time.Time `json:"at"`
}

// InstructionAddInput is instruction_add: the words, and the one it replaces
// if any. Reason says why the old one goes; left out it is "superseded by I-n".
type InstructionAddInput struct {
	// Kind is directive when left out; meta and definition are named.
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Title heads a meta line; Term, Why and RationaleRef make a definition
	// (term and why required).
	Title        string `json:"title"`
	Term         string `json:"term"`
	Why          string `json:"why"`
	RationaleRef string `json:"rationale_ref"`
	Supersedes   string `json:"supersedes"`
	Reason       string `json:"reason"`
	ChatID       string `json:"chat_id"`
	// ConfirmNew: the person has read similar[] and says this one is different.
	ConfirmNew bool `json:"confirm_new"`
	// Tags file the new line for help(situation), in the same write.
	Tags *InstructionTags `json:"tags"`
	// Catalog takes an entry of the T-knowledge catalog (BP-…): its English
	// is copied word for word (text, if given, must be exactly it), and the
	// person's language goes in Translation, with Lang and who translated.
	Catalog      string `json:"catalog"`
	Translation  string `json:"translation"`
	Lang         string `json:"lang"`
	TranslatedBy string `json:"translated_by"`
}

// InstructionSimilar is one instruction whose wording comes close to the one
// being added; Reason is as for claims (similar · contains · contained_by ·
// restated · same), or same_term for a definition of a term already defined.
type InstructionSimilar struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Text   string  `json:"text"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
	Status string  `json:"status"`
}

// InstructionAddResult is what instruction_add did: Instruction is the new one,
// or with Existing the active one that already says it; Similar alone means
// nothing was written.
type InstructionAddResult struct {
	Instruction *Instruction         `json:"instruction,omitempty"`
	Existing    bool                 `json:"existing,omitempty"`
	Similar     []InstructionSimilar `json:"similar,omitempty"`
}

// maxInstructionLength is a backstop against a paste by mistake, not a style rule.
const maxInstructionLength = 2000

// The store seeds these into a store the first time its instructions are
// touched: the default list every agent gets when nothing else is added.
// internal/tka/defaults/tk-default-instructions.md is a copy of
// mvp/tk-default-instructions.md, kept byte-identical.
//
//go:embed defaults/tk-default-instructions.md
var defaultInstructionsMD string

// Who the seeded rows name: no person wrote them into this store, the store did.
const (
	seedSigner   = "tk"
	seedProducer = "tk-default-instructions"
	seedGrant    = "default"
)

// DefaultInstruction is one line of the seed list.
type DefaultInstruction struct {
	Kind         string
	Label        string
	Title        string
	Group        string
	Text         string
	Term         string
	Why          string
	RationaleRef string
}

var (
	defaultHeading    = regexp.MustCompile(`^##\s+(.*)$`)
	defaultMeta       = regexp.MustCompile(`^\*\*(M\d+)\s+—\s+(.+?)\.?\*\*\s*(.*)$`)
	defaultDirective  = regexp.MustCompile(`^\*\*(\d+)\.\*\*\s*(.*)$`)
	defaultDefinition = regexp.MustCompile(`^\*\*def:(.+?)\*\*\s*—\s*(.*)$`)
	defaultWhy        = regexp.MustCompile(`^(.*?)\s*\*Why:\*\s*(.*?)\s*(?:→\s*` + "`" + `([^` + "`" + `]+)` + "`" + `)?\s*$`)
)

// DefaultInstructions reads the seed list: "**M1 — Title.** text" under
// "## META", "**n.** text" under each "## Group …", and "**def:term** — text
// *Why:* reason → `ref`" under "## Definitions". An item runs to the next blank
// line; text outside an item (the preambles) is not an instruction.
func DefaultInstructions() []DefaultInstruction {
	var out []DefaultInstruction
	var group string
	var cur *DefaultInstruction
	flush := func() {
		if cur == nil {
			return
		}
		cur.Text = strings.TrimSpace(cur.Text)
		if cur.Kind == "definition" {
			if m := defaultWhy.FindStringSubmatch(cur.Text); m != nil {
				cur.Text, cur.Why, cur.RationaleRef = m[1], m[2], m[3]
			}
		}
		out = append(out, *cur)
		cur = nil
	}
	sc := bufio.NewScanner(strings.NewReader(defaultInstructionsMD))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := defaultHeading.FindStringSubmatch(line); m != nil {
			flush()
			group = m[1]
			continue
		}
		if m := defaultMeta.FindStringSubmatch(line); m != nil {
			flush()
			cur = &DefaultInstruction{Kind: "meta", Label: m[1], Title: m[2], Group: group, Text: m[3]}
			continue
		}
		if m := defaultDirective.FindStringSubmatch(line); m != nil {
			flush()
			cur = &DefaultInstruction{Kind: "directive", Label: m[1], Group: group, Text: m[2]}
			continue
		}
		if m := defaultDefinition.FindStringSubmatch(line); m != nil {
			flush()
			cur = &DefaultInstruction{Kind: "definition", Label: "def:" + m[1], Term: m[1], Group: group, Text: m[2]}
			continue
		}
		if line == "" || line == "---" {
			flush()
			continue
		}
		if cur != nil {
			cur.Text += " " + line
		}
	}
	flush()
	return out
}

func lockInstructions(ctx context.Context, q querier, tenant string) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "instruction:"+TenantOr(tenant))
	return err
}

// seedInstructions gives a store that has never had an instruction the
// catalog's starters (the default list, BP-1…), in order, as I-1…, each
// naming its entry; once any row exists it does nothing, so a retired default
// stays retired and an entry the catalog adds later reaches the store only as
// an offer (owner 14/10/2026). It is the store's own write, not the person's:
// it is not counted against a trial store's writes.
func seedInstructions(ctx context.Context, q querier, tenant string, c catalog) error {
	if err := lockInstructions(ctx, q, tenant); err != nil {
		return err
	}
	var any bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM instruction WHERE tenant_id = $1)`, TenantOr(tenant)).Scan(&any); err != nil {
		return err
	}
	if any {
		return nil
	}
	for i, d := range startingLines(c) {
		if _, err := q.Exec(ctx, `
			INSERT INTO instruction (tenant_id, num, kind, label, text, origin, title, grp, term, why, rationale_ref,
			                         catalog_ref, signer, producer, grant_id)
			VALUES ($1, $2, $3, $4, $5, 'default', $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			TenantOr(tenant), i+1, d.Kind, d.Label, d.Text, d.Title, d.Group, d.Term,
			d.Why, d.RationaleRef, d.CatalogRef, seedSigner, seedProducer, seedGrant); err != nil {
			return err
		}
	}
	return nil
}

// startingLines are the lines a new store begins with: the catalog's
// starters, labelled the way the default list labels them (M1, 1, def:claim,
// numbered in order within each kind); the default list itself if the
// catalog has none.
func startingLines(c catalog) []Instruction {
	starters := c.starters()
	if len(starters) == 0 {
		out := []Instruction{}
		for i, d := range DefaultInstructions() {
			out = append(out, Instruction{ID: "I-" + strconv.Itoa(i+1), Kind: d.Kind, Label: d.Label, Text: d.Text,
				Origin: "default", Title: nilIfEmpty(d.Title), Group: nilIfEmpty(d.Group), Term: nilIfEmpty(d.Term),
				Why: nilIfEmpty(d.Why), RationaleRef: nilIfEmpty(d.RationaleRef), Status: "active",
				Signer: seedSigner, Producer: seedProducer})
		}
		return out
	}
	defaults := map[string]DefaultInstruction{}
	for _, d := range DefaultInstructions() {
		defaults[d.Text] = d
	}
	out := []Instruction{}
	metaN, dirN := 0, 0
	for i, e := range starters {
		label := ""
		switch e.Kind {
		case "meta":
			metaN++
			label = "M" + strconv.Itoa(metaN)
		case "directive":
			dirN++
			label = strconv.Itoa(dirN)
		default:
			label = "def:" + *e.Term
		}
		ref := e.ID
		in := Instruction{ID: "I-" + strconv.Itoa(i+1), Kind: e.Kind, Label: label, Text: e.Text, Origin: "default",
			Title: e.Title, Term: e.Term, Why: e.Why, RationaleRef: e.RationaleRef, CatalogRef: &ref, Status: "active",
			Signer: seedSigner, Producer: seedProducer}
		if d, ok := defaults[e.Text]; ok {
			in.Group = nilIfEmpty(d.Group)
		}
		out = append(out, in)
	}
	return out
}

const instructionColumns = `i.id, i.kind, i.label, i.text, i.origin, i.title, i.grp, i.term, i.why, i.rationale_ref,
	i.supersedes, i.signer, i.producer, i.created_at,
	i.catalog_ref, i.translation, i.lang, i.translated_by, i.starter, i.evidence, i.review_note,
	r.reason, r.superseded_by, r.signer, r.producer, r.at`

const instructionFrom = `FROM instruction i LEFT JOIN instruction_retired r
	ON r.tenant_id = i.tenant_id AND r.instruction_id = i.id`

// scanInstruction reads instructionColumns, then any extra columns into extra.
func scanInstruction(row pgx.Row, extra ...any) (Instruction, error) {
	var in Instruction
	var reason, rSigner, rProducer *string
	var by *string
	var at *time.Time
	dest := append([]any{&in.ID, &in.Kind, &in.Label, &in.Text, &in.Origin, &in.Title, &in.Group, &in.Term, &in.Why, &in.RationaleRef,
		&in.Supersedes, &in.Signer, &in.Producer, &in.CreatedAt,
		&in.CatalogRef, &in.Translation, &in.Lang, &in.TranslatedBy, &in.Starter, &in.Evidence, &in.ReviewNote,
		&reason, &by, &rSigner, &rProducer, &at}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Instruction{}, err
	}
	in.Status = "active"
	if reason != nil {
		in.Status = "retired"
		in.Retired = &InstructionRetirement{Reason: *reason, SupersededBy: by, Signer: *rSigner, Producer: *rProducer, At: *at}
	}
	return in, nil
}

// Instructions is instructions(): the active set, defaults first, in the order
// they were added, each with its tags; history adds the retired ones, each
// with when and why. It only reads: a store that has never had an
// instruction is shown the catalog's starters as they will be seeded (I-1…)
// by the first add, retire or tag, so reading the instructions writes nothing.
func (r *Repo) Instructions(ctx context.Context, tenant string, history bool) ([]Instruction, error) {
	list, _, err := r.instructionsAndCatalog(ctx, tenant)
	if err != nil || history {
		return list, err
	}
	return activeOnly(list), nil
}

func activeOnly(list []Instruction) []Instruction {
	out := []Instruction{}
	for _, in := range list {
		if in.Status == "active" {
			out = append(out, in)
		}
	}
	return out
}

// instructionsAndCatalog is every line of the store, retired ones included,
// with tags, and the catalog it was read against.
func (r *Repo) instructionsAndCatalog(ctx context.Context, tenant string) ([]Instruction, catalog, error) {
	c, err := r.loadCatalog(ctx)
	if err != nil {
		return nil, catalog{}, err
	}
	out := []Instruction{}
	err = r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+instructionColumns+` `+instructionFrom+`
			WHERE i.tenant_id = $1 ORDER BY i.num`, TenantOr(tenant))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			in, err := scanInstruction(rows)
			if err != nil {
				return err
			}
			out = append(out, in)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out) == 0 {
			out = startingLines(c)
		}
		return attachTags(ctx, q, tenant, out, c)
	})
	return out, c, err
}

// InstructionsWithOffers is instructions() as the MCP tool returns it: the
// lines, and what the catalog offers this store.
func (r *Repo) InstructionsWithOffers(ctx context.Context, tenant string, history bool) ([]Instruction, CatalogOffers, error) {
	list, c, err := r.instructionsAndCatalog(ctx, tenant)
	if err != nil {
		return nil, CatalogOffers{}, err
	}
	offers := c.offers(list)
	if !history {
		list = activeOnly(list)
	}
	return list, offers, nil
}

// HelpFor is help(situation) for one store: its lines that fit, and the
// catalog entries that fit and that the store has not met.
func (r *Repo) HelpFor(ctx context.Context, tenant, situation string) (HelpResult, error) {
	list, c, err := r.instructionsAndCatalog(ctx, tenant)
	if err != nil {
		return HelpResult{}, err
	}
	res := Help(activeOnly(list), situation)
	res.Suggested = c.suggest(list, situation)
	if len(res.Suggested) > 0 {
		res.SuggestedNext = "Suggested lines are offers from the T-knowledge catalog, not this store's instructions: show them to the person " +
			"and add one only if they choose it — instruction_add with catalog: BP-…, with a translation in their language."
		if res.Count == 0 {
			res.Next = ""
		}
	}
	return res, nil
}

func canonInstructionID(raw string) string {
	id := strings.ToUpper(strings.TrimSpace(raw))
	n, ok := strings.CutPrefix(id, "I-")
	if !ok {
		return ""
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil || v <= 0 {
		return ""
	}
	return "I-" + strconv.FormatInt(v, 10)
}

func readInstruction(ctx context.Context, q querier, tenant, raw string) (Instruction, error) {
	id := canonInstructionID(raw)
	in, err := scanInstruction(q.QueryRow(ctx, `SELECT `+instructionColumns+` `+instructionFrom+`
		WHERE i.tenant_id = $1 AND i.id = $2`, TenantOr(tenant), id))
	if errors.Is(err, pgx.ErrNoRows) || id == "" {
		return Instruction{}, failNext("INSTRUCTION_UNKNOWN", "Call instructions(history: true) to see every I-… this store has.",
			"%s is not an instruction in this store", strings.TrimSpace(raw))
	}
	return in, err
}

func retireInstruction(ctx context.Context, q querier, a Actor, id, reason string, by *string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO instruction_retired (tenant_id, instruction_id, reason, superseded_by, signer, producer, grant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, a.Tenant(), id, reason, by, a.UserID, a.ActorID, a.GrantID)
	return err
}

// similarInstructions is every instruction of the store whose wording comes
// close to key, by the same test as claims, apart from the one being
// superseded. A retired one is listed too, with its status: the person may be
// bringing back what they once dropped.
func (r *Repo) similarInstructions(ctx context.Context, q querier, tenant, key, term, exclude string) (same *Instruction, out []InstructionSimilar, err error) {
	rows, err := q.Query(ctx, `SELECT `+instructionColumns+`, similarity(lower(i.text), $2) `+instructionFrom+`
		WHERE i.tenant_id = $1 AND i.id <> $3 ORDER BY i.num`, TenantOr(tenant), key, exclude)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var score float64
		in, err := scanInstruction(rows, &score)
		if err != nil {
			return nil, nil, err
		}
		other := ClaimKey(in.Text)
		reason := "same"
		if other != key {
			reason = r.closeness(key, other, score)
		} else if in.Status == "active" && same == nil {
			c := in
			same = &c
			continue
		}
		// A term defined twice is two definitions of one thing, whatever the words.
		if reason == "" && term != "" && in.Kind == "definition" && in.Status == "active" && in.Term != nil &&
			strings.EqualFold(strings.TrimSpace(*in.Term), term) {
			reason = "same_term"
		}
		if reason != "" {
			out = append(out, InstructionSimilar{ID: in.ID, Label: in.Label, Text: in.Text, Score: score, Reason: reason, Status: in.Status})
		}
	}
	return same, out, rows.Err()
}

// AddInstruction is instruction_add. With supersedes, the named instruction is
// retired in the same write, pointing at the new one. Wording already here is
// caught as for claims; meaning is not judged.
func (r *Repo) AddInstruction(ctx context.Context, a Actor, in InstructionAddInput) (InstructionAddResult, error) {
	if err := r.checkSigner(a); err != nil {
		return InstructionAddResult{}, err
	}
	c, err := r.loadCatalog(ctx)
	if err != nil {
		return InstructionAddResult{}, err
	}
	var entry *CatalogEntry
	if strings.TrimSpace(in.Catalog) != "" {
		if entry, err = takeable(c, in.Catalog); err != nil {
			return InstructionAddResult{}, err
		}
		if t := strings.TrimSpace(in.Text); t != "" && t != entry.Text {
			return InstructionAddResult{}, failNext("CATALOG_TEXT_CHANGED",
				"Leave text out (the store copies "+entry.ID+") or pass it exactly; the person's language goes in translation.",
				"text differs from %s", entry.ID)
		}
		if strings.TrimSpace(in.Term) != "" || strings.TrimSpace(in.Why) != "" || strings.TrimSpace(in.Title) != "" || strings.TrimSpace(in.Kind) != "" {
			return InstructionAddResult{}, failNext("INVALID_INSTRUCTION", "Leave kind, title, term and why out: they come from "+entry.ID+".",
				"a catalog entry carries its own kind and wording")
		}
		in.Kind = entry.Kind
		in.Term, in.Why, in.RationaleRef, in.Title = deref(entry.Term), deref(entry.Why), deref(entry.RationaleRef), deref(entry.Title)
	}
	translation, lang, translatedBy := strings.TrimSpace(in.Translation), strings.TrimSpace(in.Lang), strings.TrimSpace(in.TranslatedBy)
	switch {
	case translation != "" && entry == nil:
		return InstructionAddResult{}, failNext("TRANSLATION_WITHOUT_CATALOG",
			"Write the instruction in the person's own words as text; a translation sits beside a catalog entry's English.",
			"translation is for a catalog entry")
	case translation != "" && lang == "":
		return InstructionAddResult{}, failNext("TRANSLATION_WITHOUT_LANG", "Name the language, e.g. lang: vi.", "a translation needs its language")
	case translation == "" && (lang != "" || translatedBy != ""):
		lang, translatedBy = "", ""
	case translation != "" && translatedBy == "":
		translatedBy = a.ActorID
	}
	text := CleanClaim(in.Text)
	if entry != nil {
		text = entry.Text
	}
	if text == "" {
		return InstructionAddResult{}, failNext("EMPTY_INSTRUCTION", "Write the instruction as the person said it.", "an instruction needs text")
	}
	if n := uniseg.GraphemeClusterCount(text); n > maxInstructionLength && entry == nil {
		return InstructionAddResult{}, failNext("INSTRUCTION_TOO_LONG", "Split it into instructions that each say one thing.",
			"an instruction is at most %d characters, this one is %d", maxInstructionLength, n)
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = "directive"
	}
	term, why, title := strings.TrimSpace(in.Term), strings.TrimSpace(in.Why), strings.TrimSpace(in.Title)
	switch kind {
	case "directive", "meta":
		if term != "" || why != "" || strings.TrimSpace(in.RationaleRef) != "" {
			return InstructionAddResult{}, failNext("INVALID_INSTRUCTION", "Leave term, why and rationale_ref out, or add it as kind definition.",
				"term, why and rationale_ref belong to a definition, not a %s", kind)
		}
	case "definition":
		if term == "" || why == "" {
			return InstructionAddResult{}, failNext("INVALID_INSTRUCTION", "Give the term, the definition as text, and why in one sentence; rationale_ref points into the docs.",
				"a definition needs a term and a why")
		}
		if title != "" {
			return InstructionAddResult{}, failNext("INVALID_INSTRUCTION", "Leave title out: a definition is headed by its term.", "a definition has no title")
		}
	default:
		return InstructionAddResult{}, failNext("INVALID_INSTRUCTION", "kind is directive (the default), meta or definition.",
			"unknown kind %q", in.Kind)
	}
	if in.Tags != nil {
		if err := validTags(*in.Tags); err != nil {
			return InstructionAddResult{}, err
		}
		in.Tags.Source = ""
	}
	var res InstructionAddResult
	err = r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := seedInstructions(ctx, q, a.Tenant(), c); err != nil {
			return err
		}
		var old *Instruction
		if strings.TrimSpace(in.Supersedes) != "" {
			o, err := readInstruction(ctx, q, a.Tenant(), in.Supersedes)
			if err != nil {
				return err
			}
			if o.Status == "retired" {
				return failNext("ALREADY_RETIRED", "Leave supersedes out, or name the instruction that replaced it.",
					"%s is already retired", o.ID)
			}
			old = &o
		}
		exclude := ""
		if old != nil {
			exclude = old.ID
		}
		// Nothing is written below this unless it is new: a store that only
		// read its instructions has not spent a write.
		same, similar, err := r.similarInstructions(ctx, q, a.Tenant(), ClaimKey(text), term, exclude)
		if err != nil {
			return err
		}
		if same != nil && entry != nil && same.Status == "retired" {
			// Declined or retired before: taking it again is a new line.
			same = nil
		}
		if same != nil {
			res = InstructionAddResult{Instruction: same, Existing: true}
			return nil
		}
		if len(similar) > 0 && !in.ConfirmNew {
			res = InstructionAddResult{Similar: similar}
			return nil
		}
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		var num int64
		if err := q.QueryRow(ctx, `SELECT coalesce(max(num), 0) + 1 FROM instruction WHERE tenant_id = $1`,
			TenantOr(a.Tenant())).Scan(&num); err != nil {
			return err
		}
		var sup *string
		if old != nil {
			sup = &old.ID
		}
		label, err := nextLabel(ctx, q, a.Tenant(), kind, term)
		if err != nil {
			return err
		}
		origin := "added"
		var ref *string
		if entry != nil {
			origin, ref = "catalog", &entry.ID
		}
		out := Instruction{Kind: kind, Label: label, Text: text, Origin: origin, Title: nilIfEmpty(title),
			Term: nilIfEmpty(term), Why: nilIfEmpty(why), RationaleRef: nilIfEmpty(in.RationaleRef),
			Supersedes: sup, Status: "active", Signer: a.UserID, Producer: a.ActorID,
			CatalogRef: ref, Translation: nilIfEmpty(translation), Lang: nilIfEmpty(lang), TranslatedBy: nilIfEmpty(translatedBy)}
		if err := q.QueryRow(ctx, `
			INSERT INTO instruction (tenant_id, num, kind, label, text, origin, title, term, why, rationale_ref,
			                         supersedes, signer, producer, grant_id, chat_id, catalog_ref, translation, lang, translated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19) RETURNING id, created_at`,
			a.Tenant(), num, kind, label, text, origin, out.Title, out.Term, out.Why, out.RationaleRef,
			sup, a.UserID, a.ActorID, a.GrantID, nullable(in.ChatID), ref, out.Translation, out.Lang, out.TranslatedBy).Scan(&out.ID, &out.CreatedAt); err != nil {
			return err
		}
		if entry != nil && (in.Tags == nil || in.Tags.empty()) {
			out.Tags = entry.Tags
			out.Tags.Source = "catalog"
		}
		if in.Tags != nil && !in.Tags.empty() {
			if err := insertTags(ctx, q, a, out.ID, *in.Tags); err != nil {
				return err
			}
			out.Tags = *in.Tags
			out.Tags.Source = "tagged"
		}
		if old != nil {
			reason := strings.TrimSpace(in.Reason)
			if reason == "" {
				reason = "superseded by " + out.ID
			}
			if err := retireInstruction(ctx, q, a, old.ID, reason, &out.ID); err != nil {
				return err
			}
		}
		res = InstructionAddResult{Instruction: &out}
		return nil
	})
	return res, err
}

// RetireInstruction is instruction_retire: the instruction stays readable in
// instructions(history: true) with the reason. Nothing in the store rests on
// an instruction, so there is no list of what fell with it.
func (r *Repo) RetireInstruction(ctx context.Context, a Actor, id, reason string) (Instruction, error) {
	if err := r.checkSigner(a); err != nil {
		return Instruction{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Instruction{}, failNext("EMPTY_REASON", "Say why the instruction no longer applies.", "retiring an instruction requires a reason")
	}
	c, err := r.loadCatalog(ctx)
	if err != nil {
		return Instruction{}, err
	}
	if bpID := canonBP(id); bpID != "" {
		return r.declineCatalog(ctx, a, c, bpID, reason)
	}
	var out Instruction
	err = r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		if err := seedInstructions(ctx, q, a.Tenant(), c); err != nil {
			return err
		}
		in, err := readInstruction(ctx, q, a.Tenant(), id)
		if err != nil {
			return err
		}
		if in.Status == "retired" {
			return failNext("ALREADY_RETIRED", "Nothing to do: it is already out of instructions().", "%s is already retired", in.ID)
		}
		if err := retireInstruction(ctx, q, a, in.ID, reason, nil); err != nil {
			return err
		}
		out, err = readInstruction(ctx, q, a.Tenant(), in.ID)
		return err
	})
	return out, err
}

// declineCatalog is instruction_retire with a catalog id: the store stops
// using the entry, or says no to it before ever taking it. A line the store
// holds is retired; an entry it never took is written and retired in the
// same write, so the refusal and its reason stay in the history and the
// offer does not come back.
func (r *Repo) declineCatalog(ctx context.Context, a Actor, c catalog, id, reason string) (Instruction, error) {
	e := c.get(id)
	if e == nil || e.Status == CatalogProposed || e.Status == CatalogRejected {
		return Instruction{}, failNext("CATALOG_UNKNOWN", "instructions() lists the catalog's offers for this store.", "%s is not in the catalog", id)
	}
	var out Instruction
	err := r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		if err := seedInstructions(ctx, q, a.Tenant(), c); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT `+instructionColumns+` `+instructionFrom+` WHERE i.tenant_id = $1 ORDER BY i.num`, a.Tenant())
		if err != nil {
			return err
		}
		var list []Instruction
		for rows.Next() {
			in, err := scanInstruction(rows)
			if err != nil {
				rows.Close()
				return err
			}
			list = append(list, in)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		met := false
		for _, in := range list {
			if l := c.linked(in); l == nil || l.ID != e.ID {
				continue
			}
			met = true
			if in.Status == "active" {
				if err := retireInstruction(ctx, q, a, in.ID, reason, nil); err != nil {
					return err
				}
				out, err = readInstruction(ctx, q, a.Tenant(), in.ID)
				return err
			}
		}
		if met {
			return failNext("ALREADY_RETIRED", "Nothing to do: this store already set it aside.", "%s is already retired or declined here", e.ID)
		}
		var num int64
		if err := q.QueryRow(ctx, `SELECT coalesce(max(num), 0) + 1 FROM instruction WHERE tenant_id = $1`, a.Tenant()).Scan(&num); err != nil {
			return err
		}
		label, err := nextLabel(ctx, q, a.Tenant(), e.Kind, deref(e.Term))
		if err != nil {
			return err
		}
		var newID string
		if err := q.QueryRow(ctx, `
			INSERT INTO instruction (tenant_id, num, kind, label, text, origin, title, term, why, rationale_ref,
			                         catalog_ref, signer, producer, grant_id)
			VALUES ($1, $2, $3, $4, $5, 'catalog', $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
			a.Tenant(), num, e.Kind, label, e.Text, e.Title, e.Term, e.Why, e.RationaleRef, e.ID,
			a.UserID, a.ActorID, a.GrantID).Scan(&newID); err != nil {
			return err
		}
		if err := retireInstruction(ctx, q, a, newID, "declined: "+reason, nil); err != nil {
			return err
		}
		out, err = readInstruction(ctx, q, a.Tenant(), newID)
		return err
	})
	return out, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nextLabel is how the list names a new instruction: a directive continues the
// numbering after the highest number the store has ever used, a meta line the
// M-numbering, and a definition is named by its term.
func nextLabel(ctx context.Context, q querier, tenant, kind, term string) (string, error) {
	switch kind {
	case "definition":
		return "def:" + term, nil
	case "meta":
		var n int64
		err := q.QueryRow(ctx, `SELECT coalesce(max(substr(label, 2)::bigint), 0) + 1 FROM instruction
			WHERE tenant_id = $1 AND kind = 'meta' AND label ~ '^M[0-9]+$'`, TenantOr(tenant)).Scan(&n)
		return "M" + strconv.FormatInt(n, 10), err
	}
	var n int64
	err := q.QueryRow(ctx, `SELECT coalesce(max(label::bigint), 0) + 1 FROM instruction
		WHERE tenant_id = $1 AND kind = 'directive' AND label ~ '^[0-9]+$'`, TenantOr(tenant)).Scan(&n)
	return strconv.FormatInt(n, 10), err
}
