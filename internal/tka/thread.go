package tka

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/people"
	"github.com/ankayma/tknowledge-personal/internal/subject"
)

// A thread is one matter a person remembers — "the Acme interview" — named by
// the keywords they would use for it, and pointing at what the store holds
// about it: claims, assumptions, documents, principles, people, conversations,
// and other threads (owner decision, 1/10/2026).
//
// It is meta-knowledge, not a claim. It carries no T/A label and passes no
// gate, and nothing about it is stored as a status: which keywords and links
// hold, and whether it is closed, is read from thread_history — the newest
// event for the same target decides. A wrong link never brings a claim down;
// removing it is one more event.
//
// Threads form a graph, not a tree: a thread may sit inside several others,
// and a link that would make a thread contain itself is refused.

// maxThreadDepth is how far thread(id, depth) follows child threads.
const maxThreadDepth = 3

// lookupMin is how close a typed word must come to a keyword or a title before
// lookup offers that thread.
const lookupMin = 0.3

var threadIDPattern = regexp.MustCompile(`^TH-0*([0-9]+)$`)

// CanonThreadID reads TH-<n> the way CanonID reads T-<n>: "th-003" is TH-3.
func CanonThreadID(raw string) (string, bool) {
	m := threadIDPattern.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(raw)))
	if m == nil {
		return "", false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return "", false
	}
	return "TH-" + strconv.FormatInt(n, 10), true
}

// NormalizeKeyword is how a keyword is written down: lowercase, no
// diacritics, words joined by single hyphens — "Dubai 2026" is dubai-2026.
func NormalizeKeyword(raw string) string { return people.Key(raw) }

// keywordKey is what two keywords are compared by: the normalised form with
// the joins taken out, so "A42", "a 42" and "A-42" are one keyword (spec §2.5)
// while it is still stored the way it was first written.
func keywordKey(k string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(NormalizeKeyword(k))
}

// keywordKeySQL is keywordKey in SQL, for a stored keyword column.
func keywordKeySQL(col string) string { return "replace(replace(" + col + ", '-', ''), '_', '')" }

// ---------- what a link can point at ----------

// linkKind is one kind of thing a thread may point at, recognised by how its
// id is spelt. Spec §3 proposed a config table; each kind needs a function that
// says whether an id exists, which is code, so the list lives here. A new
// concept is one more entry.
type linkKind struct {
	kind  string
	canon func(raw string) (string, bool)
	table string // where the id must exist
	col   string
}

var linkKinds = []linkKind{
	{"thread", CanonThreadID, "thread", "id"},
	{"truth", canonPrefixed("T-"), "truth_ledger", "id"},
	{"assumption", canonPrefixed("A-"), "assumption", "id"},
	{"document", canonPrefixed("D-"), "document", "id"},
	{"principle", canonPrinciple, "principle", "code"},
	{"person", canonPerson, "persons", "slug"},
	{"conversation", canonConversation, "conversations", "id"},
}

func canonPrefixed(prefix string) func(string) (string, bool) {
	return func(raw string) (string, bool) {
		id := CanonID(raw)
		if !strings.HasPrefix(id, prefix) || !entryIDPattern.MatchString(id) || id == prefix+"0" {
			return "", false
		}
		return id, true
	}
}

// canonPrinciple wants the P: a bare number could be anything.
func canonPrinciple(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "P") && !strings.HasPrefix(raw, "p") {
		return "", false
	}
	return NormalizeCode(raw)
}

func canonPerson(raw string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(raw), "person:")
	if !ok {
		return "", false
	}
	slug := people.Key(rest)
	if slug == "" || slug == people.Me {
		return "", false
	}
	return "person:" + slug, true
}

// canonConversation takes conv_<n>, or the thr_<n> a conversation had before
// the rename. A bare number is not an id here: it could be anything.
func canonConversation(raw string) (string, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if !strings.HasPrefix(raw, "conv_") && !strings.HasPrefix(raw, "thr_") {
		return "", false
	}
	n, ok := subject.ParseConversationID(raw)
	if !ok {
		return "", false
	}
	return subject.FormatConversationID(n), true
}

// kindOf names the kind of an id already in canonical form, without asking
// whether it exists.
func kindOf(id string) string {
	for _, k := range linkKinds {
		if c, ok := k.canon(id); ok && c == id {
			return k.kind
		}
	}
	return ""
}

// resolveTarget is the canonical id and kind of something a thread points at,
// or LINK_TARGET_UNKNOWN when the spelling is not a known kind or nothing in
// this store has that id.
func resolveTarget(ctx context.Context, q querier, tenant, raw string) (string, string, error) {
	for _, k := range linkKinds {
		id, ok := k.canon(raw)
		if !ok {
			continue
		}
		key := any(id)
		switch k.kind {
		case "person":
			key = strings.TrimPrefix(id, "person:")
		case "conversation":
			n, _ := subject.ParseConversationID(id)
			key = n
		}
		var exists bool
		if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE tenant_id = $1 AND %s = $2)`,
			k.table, k.col), TenantOr(tenant), key).Scan(&exists); err != nil {
			return "", "", err
		}
		if !exists {
			return "", "", fail("LINK_TARGET_UNKNOWN", "%s is not in this store", id)
		}
		return id, k.kind, nil
	}
	return "", "", fail("LINK_TARGET_UNKNOWN", "%q is not an id a thread can point at", strings.TrimSpace(raw))
}

// ---------- reading a thread's state ----------

// Link is one thing a thread points at, as it stands now.
type Link struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Note     *string   `json:"note,omitempty"`
	Actor    string    `json:"actor"`
	LinkedAt time.Time `json:"linked_at"`
}

type threadState struct {
	ID        string
	Title     string
	Status    string // open · closed
	Keywords  []string
	Links     []Link
	Signer    string
	Producer  string
	CreatedAt time.Time
	ClosedBy  *string // the reason, when closed
}

func loadThread(ctx context.Context, q querier, tenant, id string) (threadState, error) {
	var st threadState
	err := q.QueryRow(ctx, `SELECT id, title, signer, producer, created_at FROM thread
		WHERE tenant_id = $1 AND id = $2`, TenantOr(tenant), id).
		Scan(&st.ID, &st.Title, &st.Signer, &st.Producer, &st.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return threadState{}, fail("THREAD_UNKNOWN", "%s is not a thread in this store", id)
	}
	if err != nil {
		return threadState{}, err
	}
	rows, err := q.Query(ctx, `SELECT event, target, actor, note, at FROM thread_history
		WHERE tenant_id = $1 AND thread_id = $2 ORDER BY id`, TenantOr(tenant), id)
	if err != nil {
		return threadState{}, err
	}
	defer rows.Close()
	st.Status = "open"
	links := map[string]Link{}
	var order []string
	for rows.Next() {
		var event, actor string
		var target, note *string
		var at time.Time
		if err := rows.Scan(&event, &target, &actor, &note, &at); err != nil {
			return threadState{}, err
		}
		switch event {
		case "tagged":
			if target != nil && !slices.Contains(st.Keywords, *target) {
				st.Keywords = append(st.Keywords, *target)
			}
		case "untagged":
			if target != nil {
				st.Keywords = slices.DeleteFunc(st.Keywords, func(k string) bool { return k == *target })
			}
		case "linked":
			if target != nil {
				if _, on := links[*target]; !on {
					order = append(order, *target)
				}
				links[*target] = Link{ID: *target, Kind: kindOf(*target), Note: note, Actor: actor, LinkedAt: at}
			}
		case "unlinked":
			if target != nil {
				delete(links, *target)
				order = slices.DeleteFunc(order, func(t string) bool { return t == *target })
			}
		case "closed":
			st.Status, st.ClosedBy = "closed", note
		}
	}
	if err := rows.Err(); err != nil {
		return threadState{}, err
	}
	if st.Keywords == nil {
		st.Keywords = []string{}
	}
	for _, t := range order {
		st.Links = append(st.Links, links[t])
	}
	return st, nil
}

func (st threadState) links(kind string) []Link {
	var out []Link
	for _, l := range st.Links {
		if l.Kind == kind {
			out = append(out, l)
		}
	}
	return out
}

func (st threadState) linked(id string) bool {
	return slices.ContainsFunc(st.Links, func(l Link) bool { return l.ID == id })
}

// threadsByKeyword are the threads a keyword names right now.
func threadsByKeyword(ctx context.Context, q querier, tenant, keyword string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT thread_id FROM (
			SELECT DISTINCT ON (thread_id, target) thread_id, event FROM thread_history
			WHERE tenant_id = $1 AND event IN ('tagged', 'untagged') AND `+keywordKeySQL("target")+` = $2
			ORDER BY thread_id, target, id DESC
		) x WHERE event = 'tagged' GROUP BY thread_id ORDER BY min(length(thread_id)), thread_id`,
		TenantOr(tenant), keywordKey(keyword))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// resolveThread reads what the caller named — TH-<n>, or a keyword — as one
// thread. A keyword naming several is refused with the candidates: the person
// picks, the store does not.
func resolveThread(ctx context.Context, q querier, tenant, ref string) (string, error) {
	if id, ok := CanonThreadID(ref); ok {
		if _, err := loadThread(ctx, q, tenant, id); err != nil {
			return "", err
		}
		return id, nil
	}
	k := NormalizeKeyword(ref)
	if k == "" {
		return "", fail("THREAD_UNKNOWN", "name a thread: TH-… or one of its keywords")
	}
	ids, err := threadsByKeyword(ctx, q, tenant, k)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", fail("THREAD_UNKNOWN", "no thread in this store has the keyword %q", k)
	case 1:
		return ids[0], nil
	}
	e := &Error{Code: "THREAD_AMBIGUOUS", Next: nextByCode["THREAD_AMBIGUOUS"],
		Message: fmt.Sprintf("the keyword %q names %d threads", k, len(ids))}
	for _, id := range ids {
		st, err := loadThread(ctx, q, tenant, id)
		if err != nil {
			return "", err
		}
		e.Candidates = append(e.Candidates, ThreadLine{ID: st.ID, Title: st.Title, Status: st.Status})
	}
	return "", e
}

// reaches says whether thread from contains to, following thread links that
// hold now.
func reaches(ctx context.Context, q querier, tenant, from, to string) (bool, error) {
	seen := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == to {
			return true, nil
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		st, err := loadThread(ctx, q, tenant, cur)
		if err != nil {
			return false, err
		}
		for _, l := range st.links("thread") {
			stack = append(stack, l.ID)
		}
	}
	return false, nil
}

// lockThreads serialises writes to one store's thread graph, so two links made
// at the same moment cannot close a cycle neither of them saw.
func lockThreads(ctx context.Context, q querier, tenant string) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "thread:"+TenantOr(tenant))
	return err
}

func addThreadEvent(ctx context.Context, q querier, tenant, threadID, event, target, actor, note string) error {
	_, err := q.Exec(ctx, `INSERT INTO thread_history (tenant_id, thread_id, event, target, actor, note)
		VALUES ($1, $2, $3, $4, $5, $6)`, TenantOr(tenant), threadID, event, nullable(target), actor, nullable(note))
	return err
}

func addKeyword(ctx context.Context, q querier, a Actor, threadID, keyword string) error {
	if _, err := q.Exec(ctx, `INSERT INTO thread_keyword (tenant_id, thread_id, keyword, actor) VALUES ($1, $2, $3, $4)`,
		a.Tenant(), threadID, keyword, a.UserID); err != nil {
		return err
	}
	return addThreadEvent(ctx, q, a.Tenant(), threadID, "tagged", keyword, a.UserID, "")
}

func normalizeKeywords(raw []string) ([]string, error) {
	var out []string
	for _, r := range raw {
		k := NormalizeKeyword(r)
		if k == "" {
			return nil, fail("EMPTY_KEYWORD", "%q has no letter or digit left once normalised", r)
		}
		if findKeyword(out, k) < 0 {
			out = append(out, k)
		}
	}
	return out, nil
}

// findKeyword is the index of the keyword in list that is the same keyword as
// k (same keywordKey), or -1.
func findKeyword(list []string, k string) int {
	key := keywordKey(k)
	return slices.IndexFunc(list, func(x string) bool { return keywordKey(x) == key })
}

// ---------- writes ----------

type ThreadOpenInput struct {
	Title    string   `json:"title"`
	Keywords []string `json:"keywords"`
	ChatID   string   `json:"chat_id"`
}

// OpenThread opens a thread and tags it with its keywords.
func (r *Repo) OpenThread(ctx context.Context, a Actor, in ThreadOpenInput) (ThreadView, error) {
	if err := r.checkSigner(a); err != nil {
		return ThreadView{}, err
	}
	title := strings.Join(strings.Fields(in.Title), " ")
	if title == "" {
		return ThreadView{}, fail("EMPTY_TITLE", "a thread needs a title")
	}
	keywords, err := normalizeKeywords(in.Keywords)
	if err != nil {
		return ThreadView{}, err
	}
	var view ThreadView
	err = r.withTenant(ctx, a.Tenant(), func(q querier) error {
		if err := r.writable(ctx, q, a); err != nil {
			return err
		}
		if err := lockThreads(ctx, q, a.Tenant()); err != nil {
			return err
		}
		num, err := nextID(ctx, q, a.Tenant(), "TH")
		if err != nil {
			return err
		}
		var id string
		if err := q.QueryRow(ctx, `INSERT INTO thread (tenant_id, num, title, signer, producer, grant_id, chat_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			a.Tenant(), num, title, a.UserID, a.ActorID, a.GrantID, nullable(in.ChatID)).Scan(&id); err != nil {
			return err
		}
		if err := addThreadEvent(ctx, q, a.Tenant(), id, "opened", "", a.UserID, ""); err != nil {
			return err
		}
		for _, k := range keywords {
			if err := addKeyword(ctx, q, a, id, k); err != nil {
				return err
			}
		}
		view, err = r.readThread(ctx, q, a.Tenant(), id, 1)
		return err
	})
	return view, err
}

type ThreadTagInput struct {
	Thread string   `json:"thread"`
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
	Reason string   `json:"reason"`
}

type ThreadTagResult struct {
	Thread    ThreadView `json:"thread"`
	Added     []string   `json:"added"`
	Removed   []string   `json:"removed"`
	Unchanged []string   `json:"unchanged,omitempty"`
}

// TagThread adds and removes keywords. Adding one the thread already has, or
// removing one it does not, changes nothing and is said so.
func (r *Repo) TagThread(ctx context.Context, a Actor, in ThreadTagInput) (ThreadTagResult, error) {
	if err := r.checkSigner(a); err != nil {
		return ThreadTagResult{}, err
	}
	add, err := normalizeKeywords(in.Add)
	if err != nil {
		return ThreadTagResult{}, err
	}
	remove, err := normalizeKeywords(in.Remove)
	if err != nil {
		return ThreadTagResult{}, err
	}
	if len(add) == 0 && len(remove) == 0 {
		return ThreadTagResult{}, fail("EMPTY_KEYWORD", "send at least one keyword to add or remove")
	}
	out := ThreadTagResult{Added: []string{}, Removed: []string{}}
	err = r.withTenant(ctx, a.Tenant(), func(q querier) error {
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
		for _, k := range add {
			if findKeyword(st.Keywords, k) >= 0 {
				out.Unchanged = append(out.Unchanged, k)
				continue
			}
			if err := addKeyword(ctx, q, a, id, k); err != nil {
				return err
			}
			out.Added = append(out.Added, k)
		}
		for _, k := range remove {
			i := findKeyword(st.Keywords, k)
			if i < 0 || findKeyword(out.Added, k) >= 0 {
				out.Unchanged = append(out.Unchanged, k)
				continue
			}
			// Untag the keyword as it was stored, so the history replays.
			if err := addThreadEvent(ctx, q, a.Tenant(), id, "untagged", st.Keywords[i], a.UserID, in.Reason); err != nil {
				return err
			}
			out.Removed = append(out.Removed, st.Keywords[i])
		}
		out.Thread, err = r.readThread(ctx, q, a.Tenant(), id, 1)
		return err
	})
	return out, err
}

type LinkInput struct {
	Thread string   `json:"thread"`
	IDs    []string `json:"ids"`
	Note   string   `json:"note"`
}

type LinkResult struct {
	Thread        string   `json:"thread"`
	Linked        []Link   `json:"linked"`
	AlreadyLinked []string `json:"already_linked,omitempty"`
}

// Link points a thread at things already in the store. One unknown id refuses
// the whole call — half a link is harder to see than none. Ids already linked
// are skipped and named; if every id was, the call is ALREADY_LINKED.
func (r *Repo) Link(ctx context.Context, a Actor, in LinkInput) (LinkResult, error) {
	if err := r.checkSigner(a); err != nil {
		return LinkResult{}, err
	}
	if len(in.IDs) == 0 {
		return LinkResult{}, fail("EMPTY_LINK", "no id to link")
	}
	out := LinkResult{Linked: []Link{}}
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
		out.Thread = id
		var targets []string
		for _, raw := range in.IDs {
			target, _, err := resolveTarget(ctx, q, a.Tenant(), raw)
			if err != nil {
				return err
			}
			if !slices.Contains(targets, target) {
				targets = append(targets, target)
			}
		}
		for _, target := range targets {
			l, linked, err := linkOne(ctx, q, a, id, target, in.Note)
			if err != nil {
				return err
			}
			if !linked {
				out.AlreadyLinked = append(out.AlreadyLinked, target)
				continue
			}
			out.Linked = append(out.Linked, l)
		}
		if len(out.Linked) == 0 {
			return fail("ALREADY_LINKED", "%s already points at %s", id, strings.Join(out.AlreadyLinked, ", "))
		}
		return nil
	})
	return out, err
}

// linkOne writes one link from a thread to a target already resolved. It is
// idempotent: a link that already holds is reported, not written twice. A link
// from a thread to a thread that already contains it is THREAD_CYCLE.
func linkOne(ctx context.Context, q querier, a Actor, threadID, target, note string) (Link, bool, error) {
	st, err := loadThread(ctx, q, a.Tenant(), threadID)
	if err != nil {
		return Link{}, false, err
	}
	if st.linked(target) {
		return Link{}, false, nil
	}
	kind := kindOf(target)
	if kind == "thread" {
		if target == threadID {
			return Link{}, false, fail("THREAD_CYCLE", "%s cannot contain itself", threadID)
		}
		loop, err := reaches(ctx, q, a.Tenant(), target, threadID)
		if err != nil {
			return Link{}, false, err
		}
		if loop {
			return Link{}, false, fail("THREAD_CYCLE", "%s already contains %s, so %s cannot contain it", target, threadID, threadID)
		}
	}
	if _, err := q.Exec(ctx, `INSERT INTO thread_link (tenant_id, thread_id, target_id, kind, actor, note)
		VALUES ($1, $2, $3, $4, $5, $6)`, a.Tenant(), threadID, target, kind, a.UserID, nullable(note)); err != nil {
		return Link{}, false, err
	}
	if err := addThreadEvent(ctx, q, a.Tenant(), threadID, "linked", target, a.UserID, note); err != nil {
		return Link{}, false, err
	}
	l := Link{ID: target, Kind: kind, Actor: a.UserID, LinkedAt: time.Now().UTC()}
	if strings.TrimSpace(note) != "" {
		l.Note = &note
	}
	return l, true, nil
}

// LinkInTx links a thread to an entry inside a transaction opened elsewhere —
// interact writes a conversation and links it in one go. tx must already carry
// the store (SetTenant).
func (r *Repo) LinkInTx(ctx context.Context, tx pgx.Tx, a Actor, threadRef, target, note string) (*ThreadLinked, error) {
	return r.linkWritten(ctx, tx, a, threadRef, target, note)
}

// ThreadLinked is what a write reports when it also linked its entry to a
// thread: which thread, and whether the link is new.
type ThreadLinked struct {
	Thread string `json:"thread"`
	Target string `json:"target"`
	New    bool   `json:"new"`
}

// linkWritten links an entry just written (or found already written) to the
// thread the caller named. It runs in the writer's transaction, so a refused
// thread rolls the write back with it.
func (r *Repo) linkWritten(ctx context.Context, q querier, a Actor, threadRef, target, note string) (*ThreadLinked, error) {
	if strings.TrimSpace(threadRef) == "" {
		return nil, nil
	}
	if err := lockThreads(ctx, q, a.Tenant()); err != nil {
		return nil, err
	}
	id, err := resolveThread(ctx, q, a.Tenant(), threadRef)
	if err != nil {
		return nil, err
	}
	canon, _, err := resolveTarget(ctx, q, a.Tenant(), target)
	if err != nil {
		return nil, err
	}
	_, linked, err := linkOne(ctx, q, a, id, canon, note)
	if err != nil {
		return nil, err
	}
	return &ThreadLinked{Thread: id, Target: canon, New: linked}, nil
}

// checkThreadRef fails early, before anything is written, when the thread the
// caller named does not resolve to exactly one.
func checkThreadRef(ctx context.Context, q querier, tenant, ref string) error {
	if strings.TrimSpace(ref) == "" {
		return nil
	}
	_, err := resolveThread(ctx, q, tenant, ref)
	return err
}

type UnlinkInput struct {
	Thread string `json:"thread"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Unlink takes one link off a thread. The entry it pointed at is not touched.
func (r *Repo) Unlink(ctx context.Context, a Actor, in UnlinkInput) (ThreadView, error) {
	if err := r.checkSigner(a); err != nil {
		return ThreadView{}, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return ThreadView{}, fail("EMPTY_REASON", "unlinking requires a reason")
	}
	var view ThreadView
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
		target := canonLinkID(in.ID)
		if !st.linked(target) {
			return fail("NOT_LINKED", "%s does not point at %s", id, strings.TrimSpace(in.ID))
		}
		if err := addThreadEvent(ctx, q, a.Tenant(), id, "unlinked", target, a.UserID, in.Reason); err != nil {
			return err
		}
		view, err = r.readThread(ctx, q, a.Tenant(), id, 1)
		return err
	})
	return view, err
}

// canonLinkID spells an id the way links store it, without asking whether it
// exists — an entry gone from view can still be unlinked.
func canonLinkID(raw string) string {
	for _, k := range linkKinds {
		if id, ok := k.canon(raw); ok {
			return id
		}
	}
	return strings.TrimSpace(raw)
}

type ThreadCloseInput struct {
	Thread string `json:"thread"`
	Reason string `json:"reason"`
}

// CloseThread closes a thread with the reason. It stays readable and keeps its
// links; it only leaves the list of matters still running.
func (r *Repo) CloseThread(ctx context.Context, a Actor, in ThreadCloseInput) (ThreadView, error) {
	if err := r.checkSigner(a); err != nil {
		return ThreadView{}, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return ThreadView{}, fail("EMPTY_REASON", "closing a thread requires a reason")
	}
	var view ThreadView
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
			return fail("ALREADY_CLOSED", "%s is already closed", id)
		}
		if err := addThreadEvent(ctx, q, a.Tenant(), id, "closed", "", a.UserID, in.Reason); err != nil {
			return err
		}
		view, err = r.readThread(ctx, q, a.Tenant(), id, 1)
		return err
	})
	return view, err
}

// ---------- the read view ----------

// ThreadClaim is a claim a thread points at, directly or through an
// assumption that was promoted into it (Via names that assumption).
type ThreadClaim struct {
	ID        string    `json:"id"`
	Claim     string    `json:"claim"`
	Effective string    `json:"effective"`
	Via       *string   `json:"via,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ThreadAssumption struct {
	ID        string    `json:"id"`
	Claim     string    `json:"claim"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	If        *string   `json:"if,omitempty"`
	Not       *string   `json:"not,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ThreadDocument struct {
	ID        string  `json:"id"`
	Filename  *string `json:"filename,omitempty"`
	MediaType string  `json:"media_type"`
}

type ThreadPrinciple struct {
	Code      string `json:"code"`
	Statement string `json:"statement"`
	Status    string `json:"status"`
}

// ThreadLine is a thread in one line: what a parent shows of a child, what
// lookup and THREAD_AMBIGUOUS list.
type ThreadLine struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Status   string       `json:"status"`
	Standing *int         `json:"standing,omitempty"`
	Open     *int         `json:"open,omitempty"`
	Latest   *ThreadClaim `json:"latest,omitempty"`
	Threads  []ThreadLine `json:"threads,omitempty"`
}

// ThreadView is thread(id): what the matter is, where it stands as of today,
// and everything it points at. Nothing in it is stored — it is all read from
// the entries and the thread's history at the moment of asking.
//
// Overdue assumptions are not marked: the deadline of an A-p lives in the
// words of its not:, and reading a date out of prose would be guessing.
type ThreadView struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	ClosedBy *string  `json:"closed_reason,omitempty"`
	Keywords []string `json:"keywords"`
	AsOf     string   `json:"as_of"`
	// Summary is the person's reading of the matter — an A with an author,
	// never a T — or nil when nobody has written one. Counts is the line the
	// store makes on its own from the entries: numbers and ids, no story.
	Summary *Summary `json:"summary"`
	// Summaries is the whole chain, oldest first, each current or
	// superseded — only when asked for (thread(TH, summaries="all")), and
	// then [] when the thread has none.
	Summaries  *[]Summary         `json:"summaries,omitempty"`
	Counts     string             `json:"counts"`
	Chain      []ThreadClaim      `json:"chain"`
	Fallen     []ThreadClaim      `json:"fallen,omitempty"`
	Open       []ThreadAssumption `json:"open"`
	Settled    []ThreadAssumption `json:"settled,omitempty"`
	Documents  []ThreadDocument   `json:"documents"`
	Principles []ThreadPrinciple  `json:"principles,omitempty"`
	// Conversations and Persons are the ids only; tk-api fills in the turns
	// and names from the conversation register.
	Conversations []Link       `json:"conversations"`
	Persons       []Link       `json:"persons"`
	Threads       []ThreadLine `json:"threads"`
	Parents       []ThreadLine `json:"parents"`
	Signer        string       `json:"signer"`
	CreatedAt     time.Time    `json:"created_at"`
}

// ReadThread is thread(id, depth): TH-<n> or a keyword naming one thread.
// allSummaries adds the thread's whole summary chain to the view.
func (r *Repo) ReadThread(ctx context.Context, tenant, ref string, depth int, allSummaries bool) (ThreadView, error) {
	var view ThreadView
	err := r.withTenant(ctx, tenant, func(q querier) error {
		id, err := resolveThread(ctx, q, tenant, ref)
		if err != nil {
			return err
		}
		if view, err = r.readThread(ctx, q, tenant, id, depth); err != nil {
			return err
		}
		if !allSummaries {
			return nil
		}
		st, err := loadThread(ctx, q, tenant, id)
		if err != nil {
			return err
		}
		all, err := r.summaryHistory(ctx, q, tenant, st)
		view.Summaries = &all
		return err
	})
	return view, err
}

func (r *Repo) readThread(ctx context.Context, q querier, tenant, id string, depth int) (ThreadView, error) {
	if depth < 1 {
		depth = 1
	}
	if depth > maxThreadDepth {
		depth = maxThreadDepth
	}
	st, err := loadThread(ctx, q, tenant, id)
	if err != nil {
		return ThreadView{}, err
	}
	v := ThreadView{
		ID: st.ID, Title: st.Title, Status: st.Status, ClosedBy: st.ClosedBy, Keywords: st.Keywords,
		AsOf: time.Now().UTC().Format("2006-01-02"), Signer: st.Signer, CreatedAt: st.CreatedAt,
		Chain: []ThreadClaim{}, Open: []ThreadAssumption{}, Documents: []ThreadDocument{},
		Conversations: []Link{}, Persons: []Link{}, Threads: []ThreadLine{}, Parents: []ThreadLine{},
	}
	chain, fallen, open, settled, err := r.claimsOf(ctx, q, tenant, st)
	if err != nil {
		return ThreadView{}, err
	}
	v.Chain, v.Fallen, v.Open, v.Settled = chain, fallen, open, settled
	for _, l := range st.links("document") {
		d, err := readDocument(ctx, q, tenant, l.ID)
		if err != nil {
			return ThreadView{}, err
		}
		v.Documents = append(v.Documents, ThreadDocument{ID: d.ID, Filename: nilIfEmpty(d.Filename), MediaType: d.MediaType})
	}
	for _, l := range st.links("principle") {
		p, err := readPrinciple(ctx, q, tenant, l.ID)
		if err != nil {
			return ThreadView{}, err
		}
		v.Principles = append(v.Principles, ThreadPrinciple{Code: p.Code, Statement: p.Statement, Status: p.Status})
	}
	v.Conversations = append(v.Conversations, st.links("conversation")...)
	v.Persons = append(v.Persons, st.links("person")...)
	seen := map[string]bool{st.ID: true}
	for _, l := range st.links("thread") {
		line, err := r.threadLine(ctx, q, tenant, l.ID, depth-1, seen)
		if err != nil {
			return ThreadView{}, err
		}
		v.Threads = append(v.Threads, line)
	}
	parents, err := parentsOf(ctx, q, tenant, st.ID)
	if err != nil {
		return ThreadView{}, err
	}
	for _, p := range parents {
		pst, err := loadThread(ctx, q, tenant, p)
		if err != nil {
			return ThreadView{}, err
		}
		v.Parents = append(v.Parents, ThreadLine{ID: pst.ID, Title: pst.Title, Status: pst.Status})
	}
	v.Counts = countsLine(v)
	if v.Summary, err = r.currentSummary(ctx, q, tenant, st); err != nil {
		return ThreadView{}, err
	}
	return v, nil
}

func nilIfEmpty(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// claimsOf sorts what a thread points at in the two stores: claims that stand
// (newest first), claims that fell, assumptions still open, and assumptions
// settled. An assumption promoted into a claim brings that claim into the
// chain — it is the same sentence, now standing.
func (r *Repo) claimsOf(ctx context.Context, q querier, tenant string, st threadState) (
	chain, fallen []ThreadClaim, open, settled []ThreadAssumption, err error) {
	chain, open = []ThreadClaim{}, []ThreadAssumption{}
	seen := map[string]bool{}
	addTruth := func(t Truth, via *string) {
		if seen[t.ID] {
			return
		}
		seen[t.ID] = true
		c := ThreadClaim{ID: t.ID, Claim: t.Claim, Effective: t.Effective, Via: via, CreatedAt: t.CreatedAt}
		if t.Effective == "T" {
			chain = append(chain, c)
		} else {
			fallen = append(fallen, c)
		}
	}
	for _, l := range st.links("truth") {
		t, err := r.ReadTruth(ctx, q, tenant, l.ID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		addTruth(t, nil)
	}
	for _, l := range st.links("assumption") {
		a, err := r.ReadAssumption(ctx, q, tenant, l.ID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		ta := ThreadAssumption{ID: a.ID, Claim: a.Claim, Type: a.Type, Status: a.Status, If: a.If, Not: a.Not, CreatedAt: a.CreatedAt}
		if a.Status == "open" {
			open = append(open, ta)
			continue
		}
		settled = append(settled, ta)
		if a.Status == "promoted" {
			var tid string
			err := q.QueryRow(ctx, `SELECT id FROM truth_ledger WHERE tenant_id = $1 AND promoted_from = $2
				ORDER BY num DESC LIMIT 1`, TenantOr(tenant), a.ID).Scan(&tid)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, nil, nil, nil, err
			}
			t, err := r.ReadTruth(ctx, q, tenant, tid)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			via := a.ID
			addTruth(t, &via)
		}
	}
	sort.SliceStable(chain, func(i, j int) bool { return chain[i].CreatedAt.After(chain[j].CreatedAt) })
	return chain, fallen, open, settled, nil
}

// threadLine is a child thread in one line; with depth left, its own children
// come along, one line each.
func (r *Repo) threadLine(ctx context.Context, q querier, tenant, id string, depth int, seen map[string]bool) (ThreadLine, error) {
	st, err := loadThread(ctx, q, tenant, id)
	if err != nil {
		return ThreadLine{}, err
	}
	chain, _, open, _, err := r.claimsOf(ctx, q, tenant, st)
	if err != nil {
		return ThreadLine{}, err
	}
	standing, waiting := len(chain), len(open)
	line := ThreadLine{ID: st.ID, Title: st.Title, Status: st.Status, Standing: &standing, Open: &waiting}
	if len(chain) > 0 {
		latest := chain[0]
		line.Latest = &latest
	}
	if depth > 0 && !seen[id] {
		seen[id] = true
		for _, l := range st.links("thread") {
			child, err := r.threadLine(ctx, q, tenant, l.ID, depth-1, seen)
			if err != nil {
				return ThreadLine{}, err
			}
			line.Threads = append(line.Threads, child)
		}
	}
	return line, nil
}

// parentsOf are the threads that point at this one now.
func parentsOf(ctx context.Context, q querier, tenant, id string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT thread_id FROM (
			SELECT DISTINCT ON (thread_id) thread_id, event FROM thread_history
			WHERE tenant_id = $1 AND target = $2 AND event IN ('linked', 'unlinked')
			ORDER BY thread_id, id DESC
		) x WHERE event = 'linked' ORDER BY thread_id`, TenantOr(tenant), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// countsLine says, from the view alone, how many claims stand and what is
// waiting. It is counting, not a reading of the matter — that is the summary a
// person writes. The date comes first so the line stays true when read later:
// it only gets older. Every claim it names carries its id.
func countsLine(v ThreadView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "As of %s", v.AsOf)
	if v.Status == "closed" {
		b.WriteString(" (closed)")
	}
	b.WriteString(": ")
	switch len(v.Chain) {
	case 0:
		b.WriteString("no standing claim")
	case 1:
		fmt.Fprintf(&b, "1 standing claim, %s %q", v.Chain[0].ID, v.Chain[0].Claim)
	default:
		fmt.Fprintf(&b, "%d standing claims, newest %s %q", len(v.Chain), v.Chain[0].ID, v.Chain[0].Claim)
	}
	switch len(v.Open) {
	case 0:
		b.WriteString("; nothing waiting")
	case 1:
		fmt.Fprintf(&b, "; waiting on %s %q", v.Open[0].ID, v.Open[0].Claim)
	default:
		ids := make([]string, 0, len(v.Open))
		for _, a := range v.Open {
			ids = append(ids, a.ID)
		}
		fmt.Fprintf(&b, "; %d open assumptions (%s)", len(v.Open), strings.Join(ids, ", "))
	}
	if len(v.Fallen) > 0 {
		fmt.Fprintf(&b, "; %d linked claim(s) no longer stand", len(v.Fallen))
	}
	if len(v.Threads) > 0 {
		fmt.Fprintf(&b, "; %d thread(s) inside", len(v.Threads))
	}
	b.WriteString(".")
	return b.String()
}

// ---------- lookup ----------

// ThreadMatch is a thread lookup found, and why.
type ThreadMatch struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Status  string  `json:"status"`
	Matched string  `json:"matched"` // keyword:<k> · title
	Score   float64 `json:"score"`
}

// LookupThreads finds threads from what a person typed: a keyword exactly,
// then keywords and titles that come close. It reads; it never resolves — the
// list goes back to the person.
func (r *Repo) LookupThreads(ctx context.Context, tenant, raw string) ([]ThreadMatch, error) {
	k := keywordKey(raw)
	text := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	out := []ThreadMatch{}
	if k == "" {
		return out, nil
	}
	err := r.withTenant(ctx, tenant, func(q querier) error {
		best := map[string]ThreadMatch{}
		rows, err := q.Query(ctx, `
			WITH kw AS (
				SELECT thread_id, target AS keyword, `+keywordKeySQL("target")+` AS key FROM (
					SELECT DISTINCT ON (thread_id, target) thread_id, target, event FROM thread_history
					WHERE tenant_id = $1 AND event IN ('tagged', 'untagged')
					ORDER BY thread_id, target, id DESC
				) x WHERE event = 'tagged'
			)
			SELECT thread_id, keyword, CASE WHEN key = $2 THEN 1.0 ELSE similarity(key, $2) END
			FROM kw WHERE key = $2 OR similarity(key, $2) >= $3`, TenantOr(tenant), k, lookupMin)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, kw string
			var score float64
			if err := rows.Scan(&id, &kw, &score); err != nil {
				rows.Close()
				return err
			}
			if m, ok := best[id]; !ok || score > m.Score {
				best[id] = ThreadMatch{ID: id, Matched: "keyword:" + kw, Score: score}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// A thread nobody tagged is still findable by what it is called.
		trows, err := q.Query(ctx, `
			SELECT id, greatest(similarity(lower(title), $2), CASE WHEN lower(title) LIKE '%' || $3 || '%' THEN 0.5 ELSE 0 END)
			FROM thread WHERE tenant_id = $1
			  AND (similarity(lower(title), $2) >= $4 OR lower(title) LIKE '%' || $3 || '%')`,
			TenantOr(tenant), text, likeEscape(text), lookupMin)
		if err != nil {
			return err
		}
		for trows.Next() {
			var id string
			var score float64
			if err := trows.Scan(&id, &score); err != nil {
				trows.Close()
				return err
			}
			if m, ok := best[id]; !ok || score > m.Score {
				best[id] = ThreadMatch{ID: id, Matched: "title", Score: score}
			}
		}
		trows.Close()
		if err := trows.Err(); err != nil {
			return err
		}
		for id, m := range best {
			st, err := loadThread(ctx, q, tenant, id)
			if err != nil {
				return err
			}
			m.Title, m.Status = st.Title, st.Status
			out = append(out, m)
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return threadNum(out[i].ID) > threadNum(out[j].ID)
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out, err
}

func threadNum(id string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(id, "TH-"), 10, 64)
	return n
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// SetTenant carries the store on a transaction opened outside this package, so
// row-level security applies to everything written in it.
func SetTenant(ctx context.Context, tx pgx.Tx, tenant string) error {
	return setTenant(ctx, tx, tenant)
}
