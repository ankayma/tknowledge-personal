package tka

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/rivo/uniseg"

	"github.com/ankayma/tknowledge-personal/internal/subject"
)

// A summary is a person's reading of where one thread stands (owner 3/10/2026):
// "Martin answered agent risk with identity, scope, owner and log (T-9) …".
// It is an A, never a T — a reading aid like A-r, written by someone, signed,
// never promoted, never evidence. Every sentence names an id of the thread, so
// whoever reads it can walk back to what it rests on. It is replaced by a newer
// one, never edited; the old ones stay readable as superseded. Whether it is
// out of date is read, not stored: a write to the thread after it makes it
// stale, and the store never rewrites it on its own.

// Summary is one summary as it reads now.
type Summary struct {
	ID        string    `json:"id"`
	Thread    string    `json:"thread"`
	Text      string    `json:"text"`
	Author    string    `json:"author"` // owner · agent (claude, older) — who wrote the words
	Signer    string    `json:"signer"`
	Producer  string    `json:"producer"`
	AsOf      *string   `json:"as_of"` // the newest entry of the thread it accounts for
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"` // current · superseded
	// Stale is true when the thread was written to after the summary: read it,
	// then read what changed since.
	Stale bool `json:"stale"`
}

// maxSummaryLength is a backstop, not a style rule: a summary longer than this
// was pasted by mistake.
const maxSummaryLength = 4000

// "agent" is any AI (which one is the row's producer); "claude" stays accepted.
var summaryAuthors = []string{"owner", "agent", "claude"}

// citedID finds the ids a sentence names: the kinds a thread can point at.
// What stands before an id must not be a letter or digit, so "data-3" is not
// A-3; TH- comes first so it is not read as T-.
var citedID = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])(TH-\d+|[TAD]-\d+|P\.\d+|conv_\d+|thr_\d+|person:[\p{L}\p{N}][\p{L}\p{N}_-]*)`)

// summarySentenceEnd splits a summary into the sentences each of which must
// cite: a full stop, question or exclamation mark followed by space, the
// full-width CJK marks, or a line break.
var summarySentenceEnd = regexp.MustCompile(`[.!?]+\s+|[。！？]+|\n+`)

// summarySentences are the sentences of a summary that say something — a piece
// with no letter or digit (a dash, a bullet) is not a sentence.
func summarySentences(text string) []string {
	var out []string
	last := 0
	for _, loc := range summarySentenceEnd.FindAllStringIndex(text, -1) {
		out = append(out, text[last:loc[1]])
		last = loc[1]
	}
	out = append(out, text[last:])
	return slices.DeleteFunc(out, func(s string) bool {
		return !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) })
	})
}

// canonCited is the canonical spelling of an id a summary names, or "".
func canonCited(raw string) string {
	for _, k := range linkKinds {
		if id, ok := k.canon(raw); ok {
			return id
		}
	}
	return ""
}

// SummarizeInput is summarize(thread, text, author, as_of?).
type SummarizeInput struct {
	Thread string `json:"thread"`
	Text   string `json:"text"`
	Author string `json:"author"`
	AsOf   string `json:"as_of"`
	ChatID string `json:"chat_id"`
}

// Summarize writes the thread's new current summary. The text is checked
// before anything is written: every sentence must cite an id the thread points
// at, and no sentence may cite one it does not.
func (r *Repo) Summarize(ctx context.Context, a Actor, in SummarizeInput) (Summary, error) {
	if err := r.checkSigner(a); err != nil {
		return Summary{}, err
	}
	author := strings.ToLower(strings.TrimSpace(in.Author))
	if !slices.Contains(summaryAuthors, author) {
		return Summary{}, failNext("INVALID_AUTHOR",
			"Say who wrote the words: owner when the person wrote them, agent when you drafted them and the person approved.",
			"author must be owner, agent or claude, got %q", in.Author)
	}
	text := CleanClaim(in.Text)
	if text == "" {
		return Summary{}, failNext("EMPTY_SUMMARY", "Write where the matter stands, citing the ids it rests on.",
			"a summary needs text")
	}
	if n := uniseg.GraphemeClusterCount(text); n > maxSummaryLength {
		return Summary{}, failNext("SUMMARY_TOO_LONG",
			"Keep the reading short; the ids carry the detail.",
			"a summary is at most %d characters, this one is %d", maxSummaryLength, n)
	}
	var out Summary
	err := r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		if err := lockThreads(ctx, q, a.Tenant()); err != nil {
			return err
		}
		id, err := resolveThread(ctx, q, a.Tenant(), in.Thread)
		if err != nil {
			return err
		}
		st, err := loadThread(ctx, q, a.Tenant(), id)
		if err != nil {
			return err
		}
		if st.Status == "closed" {
			return failNext("THREAD_CLOSED", "Open a new thread for what continues, or read the closed one as it stands.",
				"%s is closed; a closed thread takes no new summary", id)
		}
		scope, err := r.summaryScope(ctx, q, a.Tenant(), st)
		if err != nil {
			return err
		}
		for _, sentence := range summarySentences(text) {
			cited := false
			for _, m := range citedID.FindAllStringSubmatch(sentence, -1) {
				cid := canonCited(m[1])
				switch {
				case cid == st.ID:
					// the thread naming itself cites nothing
				case cid != "" && scope.allowed[cid]:
					cited = true
				default:
					return failNext("SUMMARY_FOREIGN_ID",
						"Cite only what the thread points at: link it first with link(...), or take it out of the summary.",
						"%s is not in %s", strings.TrimSpace(m[1]), st.ID)
				}
			}
			if !cited {
				return failNext("SUMMARY_UNCITED",
					"Every sentence names at least one id of the thread (T-…, A-…, D-…, conv_…, person:…, an inner TH-…), so a reader can check it.",
					"this sentence cites nothing in %s: %q", st.ID, clip(strings.TrimSpace(sentence), 120))
			}
		}
		asOf := scope.newest
		if raw := strings.TrimSpace(in.AsOf); raw != "" {
			want := CanonID(raw)
			if _, ok := scope.entryAt[want]; !ok {
				return failNext("SUMMARY_AS_OF_UNKNOWN",
					"Leave as_of out and the store stamps the newest entry of the thread; give one only to say the summary stops earlier.",
					"%s is not an entry of %s", raw, st.ID)
			}
			asOf = want
		}
		var num int64
		if err := q.QueryRow(ctx, `SELECT coalesce(max(num), 0) + 1 FROM thread_summary WHERE tenant_id = $1`,
			TenantOr(a.Tenant())).Scan(&num); err != nil {
			return err
		}
		s := Summary{Thread: st.ID, Text: text, Author: author, Signer: a.UserID, Producer: a.ActorID,
			AsOf: nilIfEmpty(asOf), Status: "current"}
		if err := q.QueryRow(ctx, `
			INSERT INTO thread_summary (tenant_id, num, thread_id, text, author, signer, producer, grant_id, chat_id, as_of)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id, created_at`,
			a.Tenant(), num, st.ID, text, author, a.UserID, a.ActorID, a.GrantID, nullable(in.ChatID), s.AsOf).
			Scan(&s.ID, &s.CreatedAt); err != nil {
			return err
		}
		if err := addThreadEvent(ctx, q, a.Tenant(), st.ID, "summarized", s.ID, a.UserID, ""); err != nil {
			return err
		}
		out = s
		return nil
	})
	return out, err
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// summaryScope is what a summary of one thread may cite, and the entries it
// may say it accounts for. A claim an assumption of the thread was promoted
// into counts as the thread's: it is the same sentence, now standing.
type summaryScope struct {
	allowed map[string]bool      // every id the thread points at, plus promoted claims
	entryAt map[string]time.Time // T, A and D entries, with when they were written
	newest  string               // the newest of them, "" when there is none
	truths  []string             // T ids, for staleness
	assums  []string             // A ids, for staleness
	convs   []int64              // conversation numbers, for staleness
}

func (r *Repo) summaryScope(ctx context.Context, q querier, tenant string, st threadState) (summaryScope, error) {
	sc := summaryScope{allowed: map[string]bool{}, entryAt: map[string]time.Time{}}
	for _, l := range st.Links {
		sc.allowed[l.ID] = true
		if l.Kind == "conversation" {
			if n, ok := subject.ParseConversationID(l.ID); ok {
				sc.convs = append(sc.convs, n)
			}
		}
	}
	chain, fallen, open, settled, err := r.claimsOf(ctx, q, tenant, st)
	if err != nil {
		return summaryScope{}, err
	}
	for _, c := range append(chain, fallen...) {
		sc.allowed[c.ID] = true
		sc.entryAt[c.ID] = c.CreatedAt
		sc.truths = append(sc.truths, c.ID)
	}
	for _, x := range append(open, settled...) {
		sc.entryAt[x.ID] = x.CreatedAt
		sc.assums = append(sc.assums, x.ID)
	}
	for _, l := range st.links("document") {
		d, err := readDocument(ctx, q, tenant, l.ID)
		if err != nil {
			return summaryScope{}, err
		}
		sc.entryAt[d.ID] = d.AddedAt
	}
	var at time.Time
	for id, t := range sc.entryAt {
		// ties go to the higher id, so the stamp does not depend on map order
		if t.After(at) || (t.Equal(at) && entryOrder(id) > entryOrder(sc.newest)) {
			sc.newest, at = id, t
		}
	}
	return sc, nil
}

// entryOrder orders ids written at the same instant: by kind, then number.
func entryOrder(id string) int64 {
	if len(id) < 3 {
		return -1
	}
	n, _ := strconv.ParseInt(id[2:], 10, 64)
	return int64(strings.IndexByte("DAT", id[0]))<<40 | n
}

// summaryRow is one row of thread_summary, without status or staleness.
func scanSummary(row pgx.Row) (Summary, error) {
	var s Summary
	err := row.Scan(&s.ID, &s.Thread, &s.Text, &s.Author, &s.Signer, &s.Producer, &s.AsOf, &s.CreatedAt)
	return s, err
}

const summaryColumns = `id, thread_id, text, author, signer, producer, as_of, created_at`

// currentSummary is the thread's newest summary with its staleness, or nil.
func (r *Repo) currentSummary(ctx context.Context, q querier, tenant string, st threadState) (*Summary, error) {
	s, err := scanSummary(q.QueryRow(ctx, `SELECT `+summaryColumns+` FROM thread_summary
		WHERE tenant_id = $1 AND thread_id = $2 ORDER BY num DESC LIMIT 1`, TenantOr(tenant), st.ID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.Status = "current"
	if s.Stale, err = r.stale(ctx, q, tenant, st, s); err != nil {
		return nil, err
	}
	return &s, nil
}

// stale says whether anything touched the thread after the summary: a link,
// unlink, tag or close in the thread's own history; a move on a claim or
// assumption it points at (promote, demote, refute, update …); evidence
// attached to or withdrawn from one of its claims; or a new turn in one of its
// conversations.
func (r *Repo) stale(ctx context.Context, q querier, tenant string, st threadState, s Summary) (bool, error) {
	var marked int64
	err := q.QueryRow(ctx, `SELECT id FROM thread_history WHERE tenant_id = $1 AND thread_id = $2
		AND event = 'summarized' AND target = $3 ORDER BY id DESC LIMIT 1`, TenantOr(tenant), st.ID, s.ID).Scan(&marked)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	sc, err := r.summaryScope(ctx, q, tenant, st)
	if err != nil {
		return false, err
	}
	entries := append(append([]string{}, sc.truths...), sc.assums...)
	if sc.convs == nil {
		sc.convs = []int64{}
	}
	if sc.truths == nil {
		sc.truths = []string{}
	}
	var stale bool
	err = q.QueryRow(ctx, `SELECT
		    EXISTS(SELECT 1 FROM thread_history WHERE tenant_id = $1 AND thread_id = $2 AND id > $3 AND event <> 'summarized')
		 OR EXISTS(SELECT 1 FROM entry_history WHERE tenant_id = $1 AND entry_id = ANY($4) AND at > $5)
		 OR EXISTS(SELECT 1 FROM evidence_history WHERE tenant_id = $1 AND truth = ANY($6) AND at > $5)
		 OR EXISTS(SELECT 1 FROM events WHERE tenant_id = $1 AND conversation_id = ANY($7) AND created_at > $5)`,
		TenantOr(tenant), st.ID, marked, entries, s.CreatedAt, sc.truths, sc.convs).Scan(&stale)
	return stale, err
}

// canonSummaryID is S-<n>, unpadded, or "".
func canonSummaryID(raw string) string {
	id := strings.ToUpper(strings.TrimSpace(raw))
	n, ok := strings.CutPrefix(id, "S-")
	if !ok {
		return ""
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil || v <= 0 {
		return ""
	}
	return "S-" + strconv.FormatInt(v, 10)
}

// ReadSummary is entry(S-…): one summary, current or superseded; a current one
// carries its staleness.
func (r *Repo) readSummary(ctx context.Context, q querier, tenant, raw string) (Summary, error) {
	id := canonSummaryID(raw)
	s, err := scanSummary(q.QueryRow(ctx, `SELECT `+summaryColumns+` FROM thread_summary
		WHERE tenant_id = $1 AND id = $2`, TenantOr(tenant), id))
	if errors.Is(err, pgx.ErrNoRows) || id == "" {
		return Summary{}, fail("NOT_FOUND", "%s is not a summary in this store", strings.TrimSpace(raw))
	}
	if err != nil {
		return Summary{}, err
	}
	var newest string
	if err := q.QueryRow(ctx, `SELECT id FROM thread_summary WHERE tenant_id = $1 AND thread_id = $2
		ORDER BY num DESC LIMIT 1`, TenantOr(tenant), s.Thread).Scan(&newest); err != nil {
		return Summary{}, err
	}
	if newest != s.ID {
		s.Status = "superseded"
		return s, nil
	}
	s.Status = "current"
	st, err := loadThread(ctx, q, tenant, s.Thread)
	if err != nil {
		return Summary{}, err
	}
	if s.Stale, err = r.stale(ctx, q, tenant, st, s); err != nil {
		return Summary{}, err
	}
	return s, nil
}

// summaryHistory is every summary of the thread, oldest first: the newest is
// current and carries its staleness, the rest are superseded.
func (r *Repo) summaryHistory(ctx context.Context, q querier, tenant string, st threadState) ([]Summary, error) {
	rows, err := q.Query(ctx, `SELECT `+summaryColumns+` FROM thread_summary
		WHERE tenant_id = $1 AND thread_id = $2 ORDER BY num`, TenantOr(tenant), st.ID)
	if err != nil {
		return nil, err
	}
	out := []Summary{}
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		s.Status = "superseded"
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if n := len(out); n > 0 {
		out[n-1].Status = "current"
		if out[n-1].Stale, err = r.stale(ctx, q, tenant, st, out[n-1]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LookupSummaries finds current summaries whose text carries what the person
// typed. Only a current summary is in summary_search — a superseded one left
// it when the newer one was written — so an old reading never comes back. Each
// hit carries its staleness.
//
// The words verbatim win outright: when any summary carries every word typed
// (in any order, as for "quant-CTO modelrank"), only those come back, and text
// that merely comes close is looked at only when none does.
// Summaries are long prose, and trigram closeness alone found "quan sát" ("observe") for
// "quant-CTO" on 5/10/2026 (owner acceptance: lookup("quant-CTO",
// scope="summaries") returns S-7 alone).
func (r *Repo) LookupSummaries(ctx context.Context, tenant, raw string) ([]Summary, error) {
	q0 := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	out := []Summary{}
	if q0 == "" {
		return out, nil
	}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		rows, err := q.Query(ctx, `
			SELECT s.id, s.thread_id, s.text, s.author, s.signer, s.producer, s.as_of, s.created_at
			FROM summary_search x JOIN thread_summary s ON s.tenant_id = x.tenant_id AND s.id = x.summary_id
			WHERE x.tenant_id = $1
			  AND CASE WHEN EXISTS (SELECT 1 FROM summary_search v WHERE v.tenant_id = $1
			                          AND NOT EXISTS (SELECT 1 FROM unnest($4::text[]) w WHERE strpos(lower(v.text), w) = 0))
			           THEN NOT EXISTS (SELECT 1 FROM unnest($4::text[]) w WHERE strpos(lower(x.text), w) = 0)
			           ELSE word_similarity($2, lower(x.text)) >= $3 END
			ORDER BY (strpos(lower(x.text), $2) > 0) DESC, word_similarity($2, lower(x.text)) DESC, s.num DESC
			LIMIT 10`, TenantOr(tenant), q0, lookupMin, strings.Fields(q0))
		if err != nil {
			return err
		}
		for rows.Next() {
			s, err := scanSummary(rows)
			if err != nil {
				rows.Close()
				return err
			}
			s.Status = "current"
			out = append(out, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			st, err := loadThread(ctx, q, tenant, out[i].Thread)
			if err != nil {
				return err
			}
			if out[i].Stale, err = r.stale(ctx, q, tenant, st, out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
