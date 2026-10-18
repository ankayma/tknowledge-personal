package tka

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/rivo/uniseg"
)

// A tenant is one store — one organisation. A customer's store is a tenant with
// no expiry and no ceiling; a stranger trying the thing gets the same store,
// metered by seat (seat.go). One mechanism, different numbers — so nothing
// about the trial is a special case that could drift from what customers run.

type Tenant struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Tier       string     `json:"tier"`
	Ref        *string    `json:"ref,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	MaxEntries *int       `json:"max_entries,omitempty"`
	MaxClaim   *int       `json:"max_claim,omitempty"`
	// MaxWords caps how many words one claim may have (owner 3/10/2026: a claim
	// is one sentence, 100 words in a trial store). MaxClaim stays as the
	// backstop in characters. nil = no word cap.
	MaxWords *int `json:"max_words,omitempty"`
	// MaxWrites caps how many writes the whole store takes, for a self-hosted
	// store that wants one. nil = no cap. A trial store has none since 13/10/2026:
	// its writes are counted per person and month on their seat (seat.go).
	MaxWrites *int      `json:"max_writes,omitempty"`
	Writes    int64     `json:"writes"` // writes taken so far
	CreatedAt time.Time `json:"created_at"`
	Entries   int       `json:"entries"` // truths + assumptions now in the store
}

// WritesLeft is how many writes the store still takes; nil when it has no cap.
func (t Tenant) WritesLeft() *int64 {
	if t.MaxWrites == nil {
		return nil
	}
	left := int64(*t.MaxWrites) - t.Writes
	if left < 0 {
		left = 0
	}
	return &left
}

// CreateTenant opens a store. expires_at and max_entries are what make it a
// trial rather than someone's working store; leave them nil for a customer.
func (r *Repo) CreateTenant(ctx context.Context, t Tenant) (Tenant, error) {
	id := strings.TrimSpace(t.ID)
	if id == "" {
		return Tenant{}, fail("INVALID_TENANT", "a store needs an id")
	}
	if t.Tier == "" {
		t.Tier = "free"
	}
	if t.Name == "" {
		t.Name = id
	}
	err := r.Pool.QueryRow(ctx, `
		INSERT INTO tenant (id, name, tier, ref, expires_at, max_entries, max_claim, max_writes, max_words)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`,
		id, t.Name, t.Tier, t.Ref, t.ExpiresAt, t.MaxEntries, t.MaxClaim, t.MaxWrites, t.MaxWords).Scan(&t.CreatedAt)
	if err != nil {
		return Tenant{}, err
	}
	t.ID = id
	return t, nil
}

// SetWriteCap puts a write cap on a store. The door opens a trial store
// uncapped, seeds its principles, and only then caps it — the seed is the
// door's doing and must not use up the person's writes.
func (r *Repo) SetWriteCap(ctx context.Context, id string, n int) error {
	_, err := r.Pool.Exec(ctx, `UPDATE tenant SET max_writes = $2 WHERE id = $1`, TenantOr(id), n)
	return err
}

// ReadTenant reads a store's terms and how full it is.
func (r *Repo) ReadTenant(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := r.withTenant(ctx, id, func(q querier) error {
		var err error
		t, err = readTenant(ctx, q, id)
		return err
	})
	return t, err
}

// readTenant must run with app.tenant_id set: the two counts read tables under
// row-level security, and without the setting they would both come back zero.
func readTenant(ctx context.Context, q querier, id string) (Tenant, error) {
	var t Tenant
	err := q.QueryRow(ctx, `
		SELECT id, name, tier, ref, expires_at, max_entries, max_claim, max_writes, max_words,
		       coalesce((SELECT n FROM tenant_writes w WHERE w.tenant_id = tenant.id), 0), created_at,
		       (SELECT count(*) FROM truth_ledger l WHERE l.tenant_id = tenant.id)
		     + (SELECT count(*) FROM assumption a WHERE a.tenant_id = tenant.id)
		FROM tenant WHERE id = $1`, TenantOr(id)).
		Scan(&t.ID, &t.Name, &t.Tier, &t.Ref, &t.ExpiresAt, &t.MaxEntries,
			&t.MaxClaim, &t.MaxWrites, &t.MaxWords, &t.Writes, &t.CreatedAt, &t.Entries)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, fail("TENANT_UNKNOWN", "%s is not a store on this server", TenantOr(id))
	}
	return t, err
}

// terms reads the store's own terms. A store with no row in the catalogue keeps
// no terms, so it keeps no limits either — that is the single-tenant deployment.
func terms(ctx context.Context, q querier, tenant string) (Tenant, bool, error) {
	t, err := readTenant(ctx, q, tenant)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && e.Code == "TENANT_UNKNOWN" {
			return Tenant{}, false, nil
		}
		return Tenant{}, false, err
	}
	return t, true, nil
}

// writable refuses every write to a store whose date has passed, and charges
// the write: against the store's cap when it has one (a self-hosted store),
// and against the writer's seat for this month when the store is metered by
// seat. Reads still work: a closed or used-up store is read-only, not deleted.
// Every write path of the store calls this or admits inside the transaction
// that writes, so the charge commits with the write and a refused or
// rolled-back write costs nothing.
func (r *Repo) writable(ctx context.Context, q querier, a Actor) error {
	t, ok, err := terms(ctx, q, a.Tenant())
	if err != nil || !ok {
		return err
	}
	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		return failNext("TENANT_EXPIRED",
			"Ask for a fresh link to keep going. Nothing here is lost — reads still work, this store just takes no more writes.",
			"this trial store closed on %s", t.ExpiresAt.Format("2006-01-02"))
	}
	return r.chargeWrite(ctx, q, a, t)
}

// Writable is writable for write paths outside this package (interact): run
// it inside the transaction that writes, with the tenant set.
func (r *Repo) Writable(ctx context.Context, tx pgx.Tx, a Actor) error {
	return r.writable(ctx, tx, a)
}

// writeLimitNext is what to do once a store with a cap of its own has taken
// its last write.
const writeLimitNext = "This store is read-only now: ledger, entry, lookup, thread and documents keep working. To keep writing, move to a paid store — write to the contact on the front page."

// chargeWrite counts one write, once per transaction however many guards one
// move runs (a tx-local marker): against the store's own cap when it has one,
// then against the writer's seat when the store is metered by seat. The upserts
// lock their counter rows, so concurrent writes queue instead of overrunning a
// cap. A store with neither is never counted.
func (r *Repo) chargeWrite(ctx context.Context, q querier, a Actor, t Tenant) error {
	if t.MaxWrites == nil && !metered(t) {
		return nil
	}
	var charged string
	if err := q.QueryRow(ctx, `SELECT coalesce(current_setting('tk.write_charged', true), '')`).Scan(&charged); err != nil {
		return err
	}
	if charged == "1" {
		return nil
	}
	if t.MaxWrites != nil {
		var n int64
		if err := q.QueryRow(ctx, `
			INSERT INTO tenant_writes (tenant_id, n) VALUES ($1, 1)
			ON CONFLICT (tenant_id) DO UPDATE SET n = tenant_writes.n + 1
			RETURNING n`, a.Tenant()).Scan(&n); err != nil {
			return err
		}
		if n > int64(*t.MaxWrites) {
			return failNext("WRITE_LIMIT_REACHED", writeLimitNext,
				"this store has taken its %d writes; it still reads, but takes no more writes", *t.MaxWrites)
		}
	}
	if metered(t) {
		if err := r.chargeSeat(ctx, q, a); err != nil {
			return err
		}
	}
	_, err := q.Exec(ctx, `SELECT set_config('tk.write_charged', '1', true)`)
	return err
}

// admits is writable plus the two limits that only apply to a new claim: how
// many claims this store holds, and how long one may be. promote is deliberately
// not held to the count — it settles a claim already written rather than adding
// one, and a full store must not strand an open assumption.
func (r *Repo) admits(ctx context.Context, q querier, a Actor, claim string) error {
	t, ok, err := terms(ctx, q, a.Tenant())
	if err != nil || !ok {
		return err
	}
	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		return failNext("TENANT_EXPIRED",
			"Ask for a fresh link to keep going. Nothing here is lost — reads still work, this store just takes no more writes.",
			"this trial store closed on %s", t.ExpiresAt.Format("2006-01-02"))
	}
	if t.MaxEntries != nil && t.Entries >= *t.MaxEntries {
		return failNext("TENANT_FULL",
			"Read what is there with ledger() and open_items(), settle what is open, or ask for a fresh store.",
			"this trial store holds %d entries, which is its limit", *t.MaxEntries)
	}
	if err := claimFits(t, claim); err != nil {
		return err
	}
	return r.chargeWrite(ctx, q, a, t)
}

// claimFits holds one claim to the store's sentence limits: words first — a
// claim is one sentence — then characters as the backstop against one huge
// "word" with no spaces in it.
func claimFits(t Tenant, claim string) error {
	if n := ClaimWords(claim); t.MaxWords != nil && n > *t.MaxWords {
		return failNext("CLAIM_TOO_MANY_WORDS",
			"A claim is one sentence. Split it into the claims it makes, and admit or assume each one.",
			"a claim in this store is at most %d words, this one is %d", *t.MaxWords, n)
	}
	if n := ClaimLength(claim); t.MaxClaim != nil && n > *t.MaxClaim {
		return failNext("CLAIM_TOO_LONG",
			"Say it in one sentence. A claim that needs a paragraph is usually several claims.",
			"a claim in this store is at most %d characters, this one is %d", *t.MaxClaim, n)
	}
	return nil
}

// claimLimits is claimFits for a path that rewords a claim already written
// (update_assumption): the same limits, read from the store's terms.
func claimLimits(ctx context.Context, q querier, tenant, claim string) error {
	t, ok, err := terms(ctx, q, tenant)
	if err != nil || !ok {
		return err
	}
	return claimFits(t, claim)
}

// ClaimLength counts a claim the way a reader does: user-perceived characters
// (grapheme clusters, UAX #29) of its NFC form. Counting runes charged a
// decomposed Vietnamese or Korean sentence more than the same sentence typed
// precomposed, and an emoji family seven times (owner 3/10/2026).
func ClaimLength(claim string) int {
	return uniseg.GraphemeClusterCount(CleanClaim(claim))
}

// ClaimWords counts the words of a claim's NFC form, so one cap means about the
// same sentence in every language (owner 3/10/2026: 100 words). A token between
// spaces counts one word when it holds a letter or a digit. Scripts written
// without spaces — Han, Hiragana, Katakana, Thai, Lao, Khmer, Myanmar — have no
// such tokens, so a run of their characters counts one word per two characters
// (rounded up): a Chinese word is one to two characters on average. Other
// letters or digits in the same token add one word. A token of emoji or marks
// alone is not a word.
func ClaimWords(claim string) int {
	words := 0
	for _, token := range strings.Fields(CleanClaim(claim)) {
		run, other := 0, false
		g := uniseg.NewGraphemes(token)
		for g.Next() {
			cluster := []rune(g.Str())
			switch {
			case len(cluster) > 0 && spacelessScript(cluster[0]):
				run++
			default:
				if run > 0 {
					words += (run + 1) / 2
					run = 0
				}
				for _, r := range cluster {
					if unicode.IsLetter(r) || unicode.IsNumber(r) {
						other = true
						break
					}
				}
			}
		}
		words += (run + 1) / 2
		if other {
			words++
		}
	}
	return words
}

// spacelessScript is a character of a script written without spaces between
// words. The prolonged sound mark ー and the iteration mark 々 are Common, not
// Japanese, by script property, but sit inside Japanese words.
func spacelessScript(r rune) bool {
	return r == 'ー' || r == '々' ||
		unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar)
}

// Funnel steps: how far someone got in a trial store.
const (
	StepGrant    = "grant_created"
	StepWrite    = "first_write"
	StepRefusal  = "reached_refusal"
	StepPromote  = "reached_promote"
	StepAct2     = "reached_act2"
	StepReturned = "returned"
)

// Step records how far someone got, and only in a trial store: the SELECT
// writes nothing for a customer's store. It keeps the step, never the claim —
// what a person tried out is their business, not the funnel's.
func (r *Repo) Step(ctx context.Context, tenant, kind string) error {
	_, err := r.Pool.Exec(ctx, `
		INSERT INTO demo_event (tenant_id, kind, ref)
		SELECT id, $2, ref FROM tenant WHERE id = $1 AND expires_at IS NOT NULL
		ON CONFLICT DO NOTHING`, TenantOr(tenant), kind)
	return err
}

// step records a funnel step and swallows its error: a counter must never be
// the reason a write fails.
func (r *Repo) step(ctx context.Context, tenant, kind string) {
	_ = r.Step(ctx, tenant, kind)
}
