package tka

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A Case is a lesson with the price paid, kept as its own entry (owner
// 17/10/2026, D-239 in the owner's store). The lesson is stored as a document
// first; at that moment the agent offers to keep a copy as a Case, and makes it
// only when the person agrees — otherwise the lesson stays in documents. A Case
// is the thing a person later shares to the Community, so its fields are the
// Community Case's fields. It is never evidence for a T: admit takes a D-….
//
// Its kind and tags are for filtering (owner 17/10: the kind is open, not a
// fixed list; later an index engine classifies Cases by them for the public
// layer). They live in case_tag lines — the newest line is the current one —
// so a Case is re-tagged without editing it.

// CaseInput is what case_add signs.
type CaseInput struct {
	Title     string   `json:"title"`
	Text      string   `json:"text"`
	FromDoc   string   `json:"from_doc"`
	Agent     string   `json:"agent"`
	CostUSD   *float64 `json:"cost_usd"`
	CostHours *float64 `json:"cost_hours"`
	CostNote  string   `json:"cost_note"`
	Who       string   `json:"who"`
	What      string   `json:"what"`
	When      string   `json:"when"`
	Where     string   `json:"where"`
	Why       string   `json:"why"`
	How       string   `json:"how"`
	CaseTags
}

// CaseTags is how a Case is found again: an open kind and three sets of tags.
type CaseTags struct {
	Kind        string   `json:"kind"`
	Keywords    []string `json:"keywords"`
	Topics      []string `json:"topics"`
	CaseStudies []string `json:"case_studies"`
}

type Case struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Text        string          `json:"text"`
	FromDoc     string          `json:"from_doc"`
	Agent       *string         `json:"agent,omitempty"`
	CostUSD     *float64        `json:"cost_usd,omitempty"`
	CostHours   *float64        `json:"cost_hours,omitempty"`
	CostNote    *string         `json:"cost_note,omitempty"`
	Who         *string         `json:"who,omitempty"`
	What        *string         `json:"what,omitempty"`
	When        *time.Time      `json:"when,omitempty"`
	Where       *string         `json:"where,omitempty"`
	Why         *string         `json:"why,omitempty"`
	How         *string         `json:"how,omitempty"`
	Kind        *string         `json:"kind,omitempty"`
	Keywords    []string        `json:"keywords"`
	Topics      []string        `json:"topics"`
	CaseStudies []string        `json:"case_studies"`
	Signer      string          `json:"signer"`
	Producer    string          `json:"producer"`
	CreatedAt   time.Time       `json:"created_at"`
	Retired     *CaseRetirement `json:"retired,omitempty"`
	// SHA256 is the hash of what the Case says — every field above except the
	// tags, the record and the retirement — computed on read. A copy shared
	// elsewhere carries it, so the copy can be checked against this one.
	SHA256 string `json:"sha256"`
}

type CaseRetirement struct {
	Reason string    `json:"reason"`
	Signer string    `json:"signer"`
	At     time.Time `json:"at"`
}

var caseIDPattern = regexp.MustCompile(`^C-([0-9]+)$`)

func canonCase(raw string) (string, bool) {
	m := caseIDPattern.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(raw)))
	if m == nil {
		return "", false
	}
	num := strings.TrimLeft(m[1], "0")
	if num == "" {
		num = "0"
	}
	return "C-" + num, true
}

// tagSet trims, lowercases and de-duplicates, keeping the order given.
func tagSet(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.Join(strings.Fields(strings.ToLower(t)), " ")
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

func (t CaseTags) clean() CaseTags {
	return CaseTags{Kind: strings.Join(strings.Fields(strings.ToLower(t.Kind)), " "),
		Keywords: tagSet(t.Keywords), Topics: tagSet(t.Topics), CaseStudies: tagSet(t.CaseStudies)}
}

func insertCaseTags(ctx context.Context, q querier, a Actor, id string, t CaseTags) error {
	_, err := q.Exec(ctx, `
		INSERT INTO case_tag (tenant_id, case_id, kind, keywords, topics, case_studies, signer, producer, grant_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		a.Tenant(), id, nullable(t.Kind), t.Keywords, t.Topics, t.CaseStudies, a.UserID, a.ActorID, a.GrantID)
	return err
}

// AddCase keeps a copy of a lesson, made from a document in this store.
func (r *Repo) AddCase(ctx context.Context, a Actor, in CaseInput) (Case, error) {
	if err := r.checkSigner(a); err != nil {
		return Case{}, err
	}
	title, text := strings.TrimSpace(in.Title), strings.TrimSpace(in.Text)
	if title == "" || text == "" {
		return Case{}, failNext("EMPTY_CASE", "Give the Case a title and the lesson's text.", "a Case needs a title and the lesson's text")
	}
	if (in.CostUSD != nil && *in.CostUSD < 0) || (in.CostHours != nil && *in.CostHours < 0) {
		return Case{}, fail("INVALID_COST", "a cost is not negative")
	}
	when, err := parseDay("when", in.When)
	if err != nil {
		return Case{}, err
	}
	if strings.TrimSpace(in.FromDoc) == "" {
		return Case{}, failNext("NO_SOURCE", "Store the lesson with document_add first, then name its D-… as from_doc.",
			"a Case is a copy of a lesson already stored as a document")
	}
	tags := in.CaseTags.clean()
	var c Case
	err = r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		doc, err := readDocument(ctx, q, a.Tenant(), in.FromDoc)
		if err != nil {
			return err
		}
		num, err := nextID(ctx, q, a.Tenant(), "C")
		if err != nil {
			return err
		}
		var id string
		if err := q.QueryRow(ctx, `
			INSERT INTO case_entry (tenant_id, num, title, text, agent, cost_usd, cost_hours, cost_note,
				who, what, happened_on, "where", why, how, from_doc, signer, producer, grant_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) RETURNING id`,
			a.Tenant(), num, title, text, nullable(in.Agent), in.CostUSD, in.CostHours, nullable(in.CostNote),
			nullable(in.Who), nullable(in.What), when, nullable(in.Where), nullable(in.Why), nullable(in.How),
			doc.ID, a.UserID, a.ActorID, a.GrantID).Scan(&id); err != nil {
			return err
		}
		if err := insertCaseTags(ctx, q, a, id, tags); err != nil {
			return err
		}
		c, err = readCase(ctx, q, a.Tenant(), id)
		return err
	})
	return c, err
}

// TagCase replaces a Case's kind and tags with a new line; the old lines stay.
func (r *Repo) TagCase(ctx context.Context, a Actor, id string, t CaseTags) (Case, error) {
	if err := r.checkSigner(a); err != nil {
		return Case{}, err
	}
	ref, ok := canonCase(id)
	if !ok {
		return Case{}, fail("CASE_UNKNOWN", "%s is not a Case id (C-…)", strings.TrimSpace(id))
	}
	var c Case
	err := r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		if _, err := readCase(ctx, q, a.Tenant(), ref); err != nil {
			return err
		}
		if err := insertCaseTags(ctx, q, a, ref, t.clean()); err != nil {
			return err
		}
		var err error
		c, err = readCase(ctx, q, a.Tenant(), ref)
		return err
	})
	return c, err
}

// RetireCase takes a Case out of cases() with a reason; it stays readable.
func (r *Repo) RetireCase(ctx context.Context, a Actor, id, reason string) (Case, error) {
	if err := r.checkSigner(a); err != nil {
		return Case{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Case{}, failNext("EMPTY_REASON", "Say why the Case no longer applies.", "retiring a Case requires a reason")
	}
	ref, ok := canonCase(id)
	if !ok {
		return Case{}, fail("CASE_UNKNOWN", "%s is not a Case id (C-…)", strings.TrimSpace(id))
	}
	var c Case
	err := r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		cur, err := readCase(ctx, q, a.Tenant(), ref)
		if err != nil {
			return err
		}
		if cur.Retired != nil {
			return failNext("ALREADY_RETIRED", "Nothing to do: it is already out of cases().", "%s is already retired", ref)
		}
		if _, err := q.Exec(ctx, `INSERT INTO case_retired (tenant_id, case_id, reason, signer, producer, grant_id)
			VALUES ($1,$2,$3,$4,$5,$6)`, a.Tenant(), ref, reason, a.UserID, a.ActorID, a.GrantID); err != nil {
			return err
		}
		c, err = readCase(ctx, q, a.Tenant(), ref)
		return err
	})
	return c, err
}

const caseColumns = `c.id, c.title, c.text, c.from_doc, c.agent, c.cost_usd::float8, c.cost_hours::float8, c.cost_note,
	c.who, c.what, c.happened_on, c."where", c.why, c.how, c.signer, c.producer, c.created_at,
	t.kind, coalesce(t.keywords, '{}'), coalesce(t.topics, '{}'), coalesce(t.case_studies, '{}'),
	x.reason, x.signer, x.at`

const caseFrom = ` FROM case_entry c
	LEFT JOIN LATERAL (SELECT kind, keywords, topics, case_studies FROM case_tag g
		WHERE g.tenant_id = c.tenant_id AND g.case_id = c.id ORDER BY g.id DESC LIMIT 1) t ON true
	LEFT JOIN case_retired x ON x.tenant_id = c.tenant_id AND x.case_id = c.id`

func scanCase(row scanner) (Case, error) {
	var c Case
	var xReason, xSigner *string
	var xAt *time.Time
	if err := row.Scan(&c.ID, &c.Title, &c.Text, &c.FromDoc, &c.Agent, &c.CostUSD, &c.CostHours, &c.CostNote,
		&c.Who, &c.What, &c.When, &c.Where, &c.Why, &c.How, &c.Signer, &c.Producer, &c.CreatedAt,
		&c.Kind, &c.Keywords, &c.Topics, &c.CaseStudies, &xReason, &xSigner, &xAt); err != nil {
		return Case{}, err
	}
	if xReason != nil {
		c.Retired = &CaseRetirement{Reason: *xReason, Signer: deref(xSigner), At: *xAt}
	}
	c.SHA256 = caseDigest(c)
	return c, nil
}

// caseDigest hashes what the Case says, in a fixed order and shape.
func caseDigest(c Case) string {
	day := ""
	if c.When != nil {
		day = c.When.Format("2006-01-02")
	}
	body, _ := json.Marshal([]any{c.Title, c.Text, c.Agent, c.CostUSD, c.CostHours, c.CostNote,
		c.Who, c.What, day, c.Where, c.Why, c.How})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func readCase(ctx context.Context, q querier, tenant, id string) (Case, error) {
	c, err := scanCase(q.QueryRow(ctx, `SELECT `+caseColumns+caseFrom+` WHERE c.tenant_id = $1 AND c.id = $2`,
		TenantOr(tenant), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Case{}, fail("CASE_UNKNOWN", "%s is not a Case in this store", id)
	}
	return c, err
}

// CaseFilter narrows cases(): words of Q in what the Case says, and an exact
// kind or tag. Retired ones come only when asked.
type CaseFilter struct {
	Q         string
	Kind      string
	Keyword   string
	Topic     string
	CaseStudy string
	Retired   bool
}

// CaseFacets counts each kind and tag over the Cases still in use, so the
// person sees what there is to filter by.
type CaseFacets struct {
	Kinds       map[string]int `json:"kinds"`
	Keywords    map[string]int `json:"keywords"`
	Topics      map[string]int `json:"topics"`
	CaseStudies map[string]int `json:"case_studies"`
}

// Cases lists this store's Cases, newest first, with the facets to filter by.
func (r *Repo) Cases(ctx context.Context, tenant string, f CaseFilter) ([]Case, CaseFacets, error) {
	out := []Case{}
	facets := CaseFacets{Kinds: map[string]int{}, Keywords: map[string]int{}, Topics: map[string]int{}, CaseStudies: map[string]int{}}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, `SELECT `+caseColumns+caseFrom+` WHERE c.tenant_id = $1 ORDER BY c.num DESC`, TenantOr(tenant))
		if err != nil {
			return err
		}
		defer rows.Close()
		words := strings.Fields(strings.ToLower(f.Q))
		want := CaseTags{Kind: f.Kind, Keywords: []string{f.Keyword}, Topics: []string{f.Topic}, CaseStudies: []string{f.CaseStudy}}.clean()
		for rows.Next() {
			c, err := scanCase(rows)
			if err != nil {
				return err
			}
			if c.Retired == nil {
				if c.Kind != nil {
					facets.Kinds[*c.Kind]++
				}
				count(facets.Keywords, c.Keywords)
				count(facets.Topics, c.Topics)
				count(facets.CaseStudies, c.CaseStudies)
			}
			if (c.Retired != nil && !f.Retired) || !c.matches(words, want) {
				continue
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, facets, err
}

func count(m map[string]int, tags []string) {
	for _, t := range tags {
		m[t]++
	}
}

func (c Case) matches(words []string, want CaseTags) bool {
	if want.Kind != "" && (c.Kind == nil || *c.Kind != want.Kind) {
		return false
	}
	for _, pair := range [][2][]string{{want.Keywords, c.Keywords}, {want.Topics, c.Topics}, {want.CaseStudies, c.CaseStudies}} {
		if len(pair[0]) > 0 && !slices.Contains(pair[1], pair[0][0]) {
			return false
		}
	}
	hay := strings.ToLower(strings.Join([]string{c.Title, c.Text, deref(c.What), deref(c.Agent), deref(c.Why), deref(c.How),
		strings.Join(c.Keywords, " "), strings.Join(c.Topics, " "), strings.Join(c.CaseStudies, " ")}, " "))
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// ReadCase reads one Case by id.
func (r *Repo) ReadCase(ctx context.Context, tenant, id string) (Case, error) {
	ref, ok := canonCase(id)
	if !ok {
		return Case{}, fail("CASE_UNKNOWN", "%s is not a Case id (C-…)", strings.TrimSpace(id))
	}
	var c Case
	err := r.withTenant(ctx, tenant, func(q querier) error {
		var err error
		c, err = readCase(ctx, q, tenant, ref)
		return err
	})
	return c, err
}
