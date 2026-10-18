// Package tka is the T/A store of D-2 and D-7: three classes in three
// tables, none of them resting on itself.
//
//	principle     axioms. Only ever added; changing one is an amendment, and
//	              it pulls down every T standing on it.
//	truth_ledger  claims that passed the gate. Content never changes.
//	assumption    claims still waiting. The owner may edit one while it is open.
//
// Nothing carries a status column: status is read from entry_history, so no
// write ever overwrites what an earlier write said.
package tka

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/unicode/norm"
)

// DefaultThreshold is when two claims are close enough that the store refuses
// to write the second one blind (owner set 0.90, 19/9).
const DefaultThreshold = 0.90

// minPremises is how many premises a derivation stands on (owner decision, 28/9).
// One premise yields a reading of that premise, not a new claim — and a chain
// written from one premise is always leaning on a second one it has not named.
const minPremises = 2

// Actor is who is writing: a person (signer) through an agent (producer),
// inside one tenant. The tenant comes from the grant, never from the request
// body — a caller cannot write into someone else's store by asking.
type Actor struct {
	UserID   string // signer — always a person
	ActorID  string // producer — the agent, e.g. claude-desktop
	GrantID  string
	TenantID string // empty = DefaultTenant
}

// DefaultTenant is where a store with no tenant in its grants keeps its rows —
// the single-tenant deployment, which is also what a siloed customer gets.
const DefaultTenant = "default"

// Tenant of an actor.
func (a Actor) Tenant() string {
	if strings.TrimSpace(a.TenantID) == "" {
		return DefaultTenant
	}
	return a.TenantID
}

// TenantOr is the tenant to read under; empty reads the default store.
func TenantOr(tenant string) string {
	if strings.TrimSpace(tenant) == "" {
		return DefaultTenant
	}
	return tenant
}

type Repo struct {
	Pool      *pgxpool.Pool
	Threshold float64 // 0 → DefaultThreshold
	// Agents are the machine identities. One of them may be the producer of a
	// row, never its signer: a claim is something a person stands behind.
	Agents map[string]bool
	// WriteTZ is the zone the write month is counted in; nil = DefaultWriteTZ.
	WriteTZ *time.Location
	// Clock is now for the write month; nil = time.Now. Tests move it.
	Clock func() time.Time
	// Contact and PricingURL go into the refusal at the end of a seat's month:
	// where to ask for a bigger seat. store.feedback_email, when set, wins over
	// Contact.
	Contact    string
	PricingURL string
}

// withTenant runs fn inside a transaction that carries the tenant, so row-level
// security applies to every statement in it. Reads use it too: without the
// setting, the policy matches nothing and a read would come back empty.
func (r *Repo) withTenant(ctx context.Context, tenant string, fn func(q querier) error) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := setTenant(ctx, tx, tenant); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func setTenant(ctx context.Context, q querier, tenant string) error {
	_, err := q.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, TenantOr(tenant))
	return err
}

// nextID draws the next number for this tenant and returns the entry id.
// Numbering is per tenant: T-1 is the first claim of this store, not of the
// database (owner decision, 28/9).
func nextID(ctx context.Context, q querier, tenant, kind string) (int64, error) {
	var num int64
	err := q.QueryRow(ctx, `
		INSERT INTO tenant_counter (tenant_id, kind, next) VALUES ($1, $2, 2)
		ON CONFLICT (tenant_id, kind) DO UPDATE SET next = tenant_counter.next + 1
		RETURNING next - 1`, TenantOr(tenant), kind).Scan(&num)
	return num, err
}

// checkSigner refuses a write signed by a machine.
func (r *Repo) checkSigner(a Actor) error {
	if r.Agents[a.UserID] {
		return fail("SIGNER_IS_AGENT", "%s is an agent: it can produce a row, not sign one", a.UserID)
	}
	if strings.TrimSpace(a.UserID) == "" {
		return fail("NO_SIGNER", "every row carries the name of a person")
	}
	return nil
}

func New(pool *pgxpool.Pool) *Repo { return &Repo{Pool: pool} }

func (r *Repo) threshold() float64 {
	if r.Threshold > 0 {
		return r.Threshold
	}
	return DefaultThreshold
}

// Error is a refusal with three parts: the code a program acts on, one
// sentence a person reads, and the move that is actually open. A refusal is a
// result, not a failure — the caller should be able to carry on from it without
// guessing.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Next    string `json:"next,omitempty"`
	// Check rides along on CLAIM_FAILED: the code is what a program acts on, and
	// this is what an assistant reads out to the person — which words, why they
	// cannot be a claim, what to write instead.
	Check *ClaimCheck `json:"check,omitempty"`
	// Candidates rides along on THREAD_AMBIGUOUS: the threads a keyword names,
	// for the person to pick from. The store never picks.
	Candidates []ThreadLine `json:"candidates,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// nextByCode is the move left open after each refusal. A code missing here is
// a refusal nobody has thought through yet.
var nextByCode = map[string]string{
	"NO_SOURCE":              "Call admit(...) again with where — the place in the document the claim is read — or write it with assume(...) instead.",
	"EMPTY_DERIVATION":       "Give both: the premises (P.x or T-id) and the chain that leads to the claim.",
	"SINGLE_PREMISE":         "Name the second premise, or write it as a reading with assume(type: \"A-r\").",
	"CLAIM_FAILED":           "Read check.problems: each one says which words fail, why, and what to write instead. check_claim(...) asks the same question without writing.",
	"INVALID_BASIS":          "Pick one basis: fact with a source, or derivation with premises and a chain.",
	"INVALID_DATE":           "Use YYYY-MM-DD.",
	"EXPIRED_VALID_UNTIL":    "Drop valid_until, or set a date in the future.",
	"PREMISE_NOT_FOUND":      "Check the id with entry(...), or admit that premise first.",
	"PREMISE_NOT_T":          "Look at it with entry(...): re-establish that premise, or rest this claim on something that still stands.",
	"PRINCIPLE_UNKNOWN":      "List them with principles(), or add it with principle_add(...).",
	"PRINCIPLE_RETIRED":      "Rest the claim on a principle that is still active, or on a fact.",
	"PRINCIPLE_EXISTS":       "A principle is never rewritten. Retire it with principle_retire(...) and add a new code.",
	"EMPTY_CLAIM":            "Send the sentence the claim makes.",
	"EMPTY_STATEMENT":        "Send the principle's canonical sentence.",
	"EMPTY_REASON":           "Say why. A claim that falls carries the reason it fell.",
	"INVALID_CODE":           "Use P.<number>, e.g. P.3. The owner's numbering is kept as it is, gaps included.",
	"INVALID_TYPE":           "Use one of A-r, A-c, A-p, A-b.",
	"INVALID_SOURCE":         "Use owner, ai, hearsay: <who>, unverified: <source>, or unknown.",
	"MISSING_TAIL":           "Declare the way out: if:/not: for A-p, via: for A-c and A-b.",
	"INVALID_TAIL":           "Keep only the tail this kind takes: if:/not: for A-p, via: for A-c and A-b, none for A-r.",
	"RESTATE_TAIL":           "Send if: and not: again with the new wording — retyping them unchanged is a fine answer.",
	"TYPE_CONFLICT":          "One claim has one way out. Edit the existing assumption, or change this wording.",
	"DUPLICATE":              "Read the other one with entry(...) and decide which wording to keep.",
	"NOT_FOUND":              "Check the id: P.x, T-…, A-…. open_items() and ledger() list what is there.",
	"NOT_OPEN":               "Its wording is part of the record now. Write a new claim instead.",
	"NOT_T":                  "Read it with entry(...) to see where it stands.",
	"NOT_A_FACT":             "Re-derive it instead: chain(...) shows every link and whether it still holds.",
	"NEVER_RAISED":           "A reading aid is not raised. If it is really a claim, admit(...) it as one.",
	"RESOLVE_BY_AMENDMENT":   "Close it by amending the principle it bets on, not by observing.",
	"CHANNEL_BLOCKED":        "Build the channel first, then update_assumption(...) to A-p, then promote.",
	"ALREADY_T":              "It already stands. Read it with entry(...).",
	"ALREADY_RETIRED":        "Nothing to do.",
	"USE_PROMOTE":            "Raise the assumption instead of writing a second copy of the claim.",
	"USE_PRINCIPLE_RETIRE":   "Use principle_retire(code, reason).",
	"SIGNER_IS_AGENT":        "Use a grant whose user_id is a person. An agent is the producer, never the signer.",
	"NO_SIGNER":              "Issue a grant with a user_id and try again.",
	"TENANT_UNKNOWN":         "Check the store named in the grant. A store has to be opened before it can be written to.",
	"INVALID_TENANT":         "Give the store an id.",
	"TENANT_EXPIRED":         "Reads still work. Ask for a fresh link if you want to keep writing.",
	"TENANT_FULL":            "Read what is there with ledger() and open_items(), or ask for a fresh store.",
	"CLAIM_TOO_LONG":         "Say it in one sentence. A claim that needs a paragraph is usually several claims.",
	"WRITE_LIMIT_REACHED":    writeLimitNext,
	"SOURCE_NOT_RETRIEVABLE": "Read the evidence into a document with document_add(text, method, source_uri) and admit with its D-… as source_doc. If the claim is public and no source can be found, write nothing. assume(...) is only for a claim nobody outside could check.",
	"SOURCE_ENTRY_NOT_FOUND": "Check the id with entry(...). Only an entry in the truth ledger can be cited as a source.",
	"SOURCE_ENTRY_NOT_T":     "Re-establish that entry first, or cite evidence that stands.",
	"DOCUMENT_UNKNOWN":       "List what the store holds with documents(), or record the evidence with document_add(...).",
	"INVALID_SOURCE_URL":     "Give a full http or https link someone else can open.",
	"DOCUMENT_TOO_LARGE":     "Keep the text of the part that carries the evidence; the original stays where it is.",
	"INVALID_MEDIA_TYPE":     "Give the original's type as type/subtype, e.g. application/pdf, or leave it out.",
	"EMPTY_DOCUMENT":         "Send the evidence as text.",
	"FILE_NOT_KEPT":          "Read the file and send what it says as text, with method and text_producer. Point at the original with source_uri — https://, drive://, s3://, file:// — and its sha256. The original stays where it is.",
	"EMPTY_TEXT":             "Send what the evidence says: for a page, its words; for a screenshot, the text in it. That is what the ledger reads.",
	"INVALID_TEXT_PRODUCER":  "Say who wrote the text: owner (the person), agent (you, the AI, reading for them — whichever AI you are) or extract (a tool).",
	"INVALID_METHOD":         "Say how the person looked: opened_source (opened the record or the link), rehashed (got the original and the hash matched) or read_text (only read another document's text).",
	"INVALID_SOURCE_URI":     "Point at where the original lives: https://…, drive://…, s3://bucket/key or file:///path.",
	"INVALID_SHA256":         "Give the sha256 as 64 hexadecimal characters, or leave it out.",
	"INVALID_SIZE":           "Give the original's size in bytes, or leave it out.",
	"USE_DOCUMENT":           "Read the page into a document: document_add(text: what it says, method: \"opened_source\", source_uri: the link, content_sha256 if you have it). Then admit with its D-… as source_doc.",
	"DOCUMENT_NOT_YOURS":     "Add your own document — what you saw, in your own words, with how you looked at it (method) — and use that one.",
	"EMPTY_WHERE":            "Say where in the document: a page, a line, a field.",
	"NOT_ATTACHED":           "entry(...) on the claim lists the documents attached to it.",
	"EVIDENCE_NOT_YOURS":     "Only the person who attached a document can take it back.",
	"ALREADY_WITHDRAWN":      "Nothing to do. entry(...) shows it as withdrawn, with the reason.",
	"FLAG_UNKNOWN":           "flags(status: \"all\") lists every flag in this store.",
	"INVALID_STATUS":         "Use open, closed or all.",
	"THREAD_UNKNOWN":         "Find it with lookup(...), or open it with thread_open(...).",
	"THREAD_AMBIGUOUS":       "Pick one id from candidates and call again with it. The store does not choose.",
	"LINK_TARGET_UNKNOWN":    "A thread only points at something in this store: T-…, A-…, D-…, P.x, person:<name>, conv_…, TH-…. Check the id with entry(...) or lookup(...).",
	"ALREADY_LINKED":         "Nothing to do. thread(...) shows what it points at.",
	"NOT_LINKED":             "Nothing to unlink. thread(...) shows what it points at.",
	"EMPTY_KEYWORD":          "A keyword needs at least one letter or digit.",
	"EMPTY_TITLE":            "Name the matter in a few words — what the person would call it.",
	"EMPTY_LINK":             "Send the ids to link: T-…, A-…, D-…, P.x, person:<name>, conv_…, TH-….",
	"THREAD_CYCLE":           "A thread cannot contain itself, directly or through others. Link the other way round, or not at all.",
	"ALREADY_CLOSED":         "Nothing to do. A closed thread stays readable with thread(...).",
	"NOT_REFUTABLE":          "Only an open A-p is refuted by an observation. A-r, A-c and A-b close with retire(...) or their own path; a claim in the ledger falls with demote(...).",
	"EMPTY_OBSERVATION":      "Say what was observed — the not: that happened.",
}

func fail(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Next: nextByCode[code]}
}

// failNext is for a refusal whose next move names something concrete.
func failNext(code, next, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Next: next}
}

// ClaimKey is the comparison key: the claim with its spacing and case
// normalised, and the punctuation that ends a sentence taken off. Two claims
// with the same key are the same claim, whoever typed them.
//
// Only sentence punctuation goes. A unit is not punctuation: "tăng 800%" and
// "tăng 800" ("up 800%", "up 800") are different claims, and trimming every trailing symbol made them
// one key — so a question about 800 came back answered with the row about 800%.
//
// The key is taken from the NFC form: the same Vietnamese sentence typed
// precomposed and pasted decomposed (macOS, PDFs) are one claim, not two keys
// that slip past the duplicate check (W3C string matching normalizes to NFC).
func ClaimKey(claim string) string {
	key := strings.ToLower(strings.Join(strings.Fields(CleanClaim(claim)), " "))
	return strings.TrimRight(key, sentenceEnd)
}

// CleanClaim is the one form a claim enters the store in: trimmed, Unicode NFC.
// Every path that takes a claim from outside runs it first, so what is stored,
// what is keyed and what is counted are the same string.
func CleanClaim(claim string) string {
	return norm.NFC.String(strings.TrimSpace(claim))
}

// sentenceEnd is punctuation that never changes what a claim asserts — the
// ASCII marks, quotes, and their full-width CJK counterparts.
const sentenceEnd = ".,;:!? \t\"'”’«»。！？；：，、．」』"

var codePattern = regexp.MustCompile(`^[0-9]{1,3}$`)

var entryIDPattern = regexp.MustCompile(`^([TAD])-0*([0-9]+)$`)

// CanonID is the one spelling of an entry id: the row's own number, unpadded
// (owner decision, 1/10). "t-0007", " T-7 " and "T-007" all read as T-7, so an id
// typed the old way still finds its entry. Anything else comes back trimmed and
// upper-cased, for the caller to refuse as not found.
func CanonID(raw string) string {
	id := strings.ToUpper(strings.TrimSpace(raw))
	m := entryIDPattern.FindStringSubmatch(id)
	if m == nil {
		return id
	}
	num := strings.TrimLeft(m[2], "0")
	if num == "" {
		num = "0"
	}
	return m[1] + "-" + num
}

// NormalizeCode accepts P.3, p3 or 3 and returns P.3. The owner's numbering is
// kept as it is — a gap (P.6, P.7) is not an error.
func NormalizeCode(raw string) (string, bool) {
	digits := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "p"), ".")
	if !codePattern.MatchString(digits) {
		return "", false
	}
	return "P." + digits, true
}

// Similar is one entry close enough to the claim being written.
//
//	Reason "similar"      the two claims score above the threshold
//	Reason "contains"     the new claim swallows an existing one word for word
//	Reason "contained_by" an existing claim swallows the new one
//	Reason "restated"     the shorter claim's words are nearly all in the longer
//
// Trigram scoring alone lets a rewrite through as soon as enough words change.
// The containment reasons catch a claim quoted inside another; "restated"
// catches the commoner case — the same claim with a clause in the middle
// dropped, which is not a substring of anything.
type Similar struct {
	ID     string  `json:"id"`
	Claim  string  `json:"claim"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
	Status string  `json:"status"`
	Table  string  `json:"table"`
}

// containMin is the shortest claim that may trigger the containment check: a
// handful of words inside a long sentence means nothing.
const containMin = 20

// The restatement check, and why each number is where it is.
const (
	// candidateScore is the trigram score worth looking at more closely. Below
	// it two claims share too little wording to be one claim rewritten. The
	// licence fact said shorter scored 0.507 against the original.
	candidateScore = 0.45
	// coverMin is how much of the shorter claim must reappear in the longer one
	// before the store asks. Not 1.0: a rewrite usually swaps a word or two
	// while dropping the clause.
	coverMin = 0.80
	// coverWords is the shortest claim this may fire on. Below it almost any
	// claim about the same subject would match almost any other.
	coverWords = 5
	// dropWords is how much shorter the rewrite has to be before this check
	// applies at all. Two sentences of the same length with a word swapped are
	// not a clause dropped — that case belongs to the trigram threshold, and
	// the owner set that at 0.90 on purpose.
	dropWords = 3
)

// restates says whether the shorter of two claims is the longer one with a
// piece taken out — the ordinary way a person rewrites a claim, and the way
// trigram scoring and substring checks both miss.
//
// Numbers are the exception: a number the shorter claim asserts and the longer
// one does not makes them different claims, however much wording they share.
// "Supplier 1 will ship in October" is not a rewrite of "Supplier 2 will ship in
// October". A shorter claim carrying no number at all is still a shortening —
// the dropped clause is usually where the number was.
func restates(a, b string) bool {
	short, long := strings.Fields(a), strings.Fields(b)
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(long)-len(short) < dropWords {
		return false
	}
	in := make(map[string]bool, len(long))
	for _, w := range long {
		in[w] = true
	}
	for _, n := range numbers(short) {
		if !in[n] {
			return false
		}
	}
	// One of them denies what the other asserts: different claims, however much
	// wording they share.
	if negated(short) != negated(long) {
		return false
	}
	seen := map[string]bool{}
	found, total := 0, 0
	for _, w := range short {
		if seen[w] {
			continue
		}
		seen[w] = true
		total++
		if in[w] {
			found++
		}
	}
	if total < coverWords {
		return false
	}
	return float64(found)/float64(total) >= coverMin
}

// negations flip what a claim says while leaving almost every word in place, so
// coverage cannot see them: "trong call này thì trình bày method" ("in this
// call, present the method") and "trong call này thì không trình bày thesis"
// ("in this call, do not present the thesis") share nearly all their words. A
// claim and its opposite must never be folded together as one restated — that
// is the one mistake here that would actively mislead.
var negations = map[string]bool{
	"không": true, "ko": true, "chưa": true, "chẳng": true, "đừng": true, "khỏi": true,
	"not": true, "no": true, "never": true, "none": true, "nor": true, "without": true,
	"cannot": true, "can't": true, "won't": true, "don't": true, "doesn't": true, "isn't": true,
}

// numbers are the tokens carrying a digit — what a claim asserts
// quantitatively. "49%" and "49" are two different such tokens, which is the
// point.
// negated counts the denials in a claim, so a claim and its opposite never look
// like one restated.
func negated(words []string) int {
	n := 0
	for _, w := range words {
		if negations[strings.Trim(strings.ToLower(w), sentenceEnd)] {
			n++
		}
	}
	return n
}

func numbers(words []string) []string {
	var out []string
	for _, w := range words {
		if strings.IndexFunc(w, unicode.IsDigit) >= 0 {
			out = append(out, w)
		}
	}
	return out
}

// Event is one line of an entry's history.
type Event struct {
	Event string    `json:"event"`
	Actor string    `json:"actor"`
	Note  *string   `json:"note,omitempty"`
	At    time.Time `json:"at"`
}

// Status of an entry, read from its newest history event.
//
//	assumption: open · promoted · refuted · retired
//	truth:      T · A (demoted) · retired
//	principle:  active · retired
func status(kind string, events []Event) string {
	last := "created"
	for _, e := range events {
		switch e.Event {
		case "promoted", "refuted", "retired", "demoted", "superseded":
			last = e.Event
		}
	}
	switch kind {
	case "assumption":
		if last == "created" {
			return "open"
		}
		return last
	case "truth":
		switch last {
		case "created":
			return "T"
		case "demoted":
			return "A"
		}
		return last
	default:
		if last == "retired" {
			return "retired"
		}
		return "active"
	}
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// history reads every event of one entry, oldest first.
func history(ctx context.Context, q querier, tenant, entryID string) ([]Event, error) {
	rows, err := q.Query(ctx, `SELECT event, actor, note, at FROM entry_history
		WHERE tenant_id = $1 AND entry_id = $2 ORDER BY id`, TenantOr(tenant), entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Event, &e.Actor, &e.Note, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func addEvent(ctx context.Context, q querier, tenant, entryID, event, actor, note string) error {
	var n *string
	if strings.TrimSpace(note) != "" {
		n = &note
	}
	_, err := q.Exec(ctx, `INSERT INTO entry_history (tenant_id, entry_id, event, actor, note)
		VALUES ($1, $2, $3, $4, $5)`, TenantOr(tenant), entryID, event, actor, n)
	return err
}

// closeness is why two keys are one sentence written twice — the Reason of
// Similar — or "" when they are not. score is pg_trgm similarity of the two.
func (r *Repo) closeness(key, otherKey string, score float64) string {
	switch {
	case score >= r.threshold():
		return "similar"
	case len(key) >= containMin && strings.Contains(otherKey, key):
		return "contained_by"
	case len(otherKey) >= containMin && strings.Contains(key, otherKey):
		return "contains"
	case restates(key, otherKey):
		return "restated"
	}
	return ""
}

// Similar finds entries close to a claim in both stores. It never merges and
// never edits: it hands the list back so the owner decides.
// The database narrows; this function decides. The SQL gate is deliberately
// looser than the threshold so a rewrite that scores in the middle still comes
// back to be looked at — the index makes that cheap, and dropping it here costs
// nothing.
func (r *Repo) Similar(ctx context.Context, q querier, tenant, key string, exclude string) ([]Similar, error) {
	const q1 = `
		SELECT id, claim, claim_key, similarity(claim_key, $1) AS score, '%s' AS tbl FROM %s
		WHERE tenant_id = $4 AND claim_key <> $1 AND id <> $3 AND (
			similarity(claim_key, $1) >= $2
			OR (length($1) >= %d AND position($1 in claim_key) > 0)
			OR (length(claim_key) >= %d AND position(claim_key in $1) > 0))`
	gate := candidateScore
	if t := r.threshold(); t < gate {
		gate = t
	}
	sql := fmt.Sprintf(q1, "assumption", "assumption", containMin, containMin) + " UNION ALL " +
		fmt.Sprintf(q1, "truth_ledger", "truth_ledger", containMin, containMin) + " ORDER BY score DESC LIMIT 20"
	rows, err := q.Query(ctx, sql, key, gate, exclude, TenantOr(tenant))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Similar{}
	for rows.Next() {
		var s Similar
		var otherKey string
		if err := rows.Scan(&s.ID, &s.Claim, &otherKey, &s.Score, &s.Table); err != nil {
			return nil, err
		}
		if s.Reason = r.closeness(key, otherKey, s.Score); s.Reason == "" {
			continue // close enough to fetch, not close enough to stop a write
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		events, err := history(ctx, q, tenant, out[i].ID)
		if err != nil {
			return nil, err
		}
		kind := "assumption"
		if out[i].Table == "truth_ledger" {
			kind = "truth"
		}
		out[i].Status = status(kind, events)
	}
	return out, nil
}
