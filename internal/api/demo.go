package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/tka"
	"github.com/ankayma/tknowledge-personal/internal/version"

	"github.com/go-chi/chi/v5"
)

// The door a stranger comes in by: one page that says what the store does, and
// one button that opens a store of their own and hands back the link.
//
// It only exists where it is switched on. tk-api normally holds public keys and
// verifies grants; here it also signs them, which is a different kind of power —
// so it is behind TK_DEMO_KEY, and a deployment that does not set it has no
// door at all, not a disabled one.
//
// What limits the damage if the link is found and hammered: every store opened
// here is new and empty, the grant reaches only that store, the store carries an
// expiry and a ceiling from the moment it is created, and one address may open
// only a few. A grant from this door can never read another tenant, because the
// tenant is inside the signed payload and never comes from the request.

//go:embed landing.html docs.html privacy.html
var landingFS embed.FS

// Demo is the door's terms. Zero value means closed.
type Demo struct {
	Key ed25519.PrivateKey // signing seed; nil = the door does not exist
	// BaseURL is what the link handed to the person says, e.g.
	// https://tk.example.org. Empty falls back to the request's host.
	BaseURL string
	// Site is the operator's own site (TK_SITE_URL): the pages link to it and
	// every message signs off with it. Empty leaves both out.
	Site string

	Lifetime   time.Duration // how long a trial store stays open; 0 = forever
	MaxEntries int           // claims it holds; 0 = no entry ceiling
	MaxWords   int           // words in one claim; 0 = no word cap
	MaxClaim   int           // characters in one claim — the backstop behind MaxWords
	PerHour    int           // stores one address may open in an hour
	Contact    string        // where to write when the mail does not arrive
	// ShowLink puts the link in the response and on the page. Off by default and
	// meant only for testing before mail is configured (owner 29/9): with it on,
	// any made-up address gets a working store, which is the exact reason the
	// owner chose mail-only delivery. It is a separate switch and never a
	// fallback — a mail outage must not quietly reopen that hole.
	ShowLink bool
	// Mail sends the link. nil = this deployment cannot send, and the door then
	// shows the link on the page instead of losing it: a mail problem must never
	// be the reason someone cannot try the store.
	Mail Mailer

	mu     sync.Mutex
	opened map[string][]time.Time
}

// DemoFromEnv builds the door from the environment, or returns nil when
// TK_DEMO_KEY is unset.
func DemoFromEnv(seed, baseURL, contact string, showLink bool) (*Demo, error) {
	if strings.TrimSpace(seed) == "" {
		return nil, nil
	}
	key, err := grant.ParseSeed(seed)
	if err != nil {
		return nil, err
	}
	return &Demo{
		Key: key, BaseURL: strings.TrimRight(baseURL, "/"),
		// No end date; the person who opens it gets a free seat, 100 writes a
		// month (owner 13/10, replacing 1000 writes in all of 3/10 and 30 days of
		// 2/10). A claim is one sentence: 100 words, with 600 characters as the
		// backstop against one huge token (owner 3/10/2026).
		MaxWords: 100, MaxClaim: 600, PerHour: 5,
		Contact:  strings.TrimSpace(contact),
		ShowLink: showLink,
		opened:   map[string][]time.Time{},
	}, nil
}

func (d *Demo) on() bool { return d != nil && d.Key != nil }

// ParseLifetime reads a trial store's lifetime: whole days as "30d", or any Go
// duration ("720h", "90m"). It must be positive — a store that is born expired
// hands out links that never work.
func ParseLifetime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("%q: days must be a whole number, e.g. 30d", s)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("%q: use 30d or a duration like 720h", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q: a lifetime must be positive", s)
	}
	return d, nil
}

// humanLifetime says a store's lifetime the way the page does: "30 days",
// "48 hours".
func humanLifetime(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		if n := int(d / (24 * time.Hour)); n == 1 {
			return "1 day"
		} else {
			return strconv.Itoa(n) + " days"
		}
	}
	if h := int(d / time.Hour); h >= 1 && d%time.Hour == 0 {
		if h == 1 {
			return "1 hour"
		}
		return strconv.Itoa(h) + " hours"
	}
	return d.String()
}

// ParseMaxEntries reads how many claims a new store holds: a whole number above
// zero. A store that can hold nothing is a store nobody can try.
func ParseMaxEntries(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q: a whole number of claims, e.g. 1000", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%q: a store must hold at least one claim", s)
	}
	return n, nil
}

// ParseMaxWords reads how many words one claim in a new store may have: a
// whole number above zero.
func ParseMaxWords(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q: a whole number of words, e.g. 100", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%q: a claim must be allowed at least one word", s)
	}
	return n, nil
}

// Terms says the door's terms in one sentence, the same on the page, in the
// mail and in the log: "no end date, 100 writes a month, reads unlimited".
func (d *Demo) Terms() string {
	writes := strconv.Itoa(*tka.SeatLimit(tka.SeatFree)) + " writes a month"
	if d == nil {
		return "no end date, " + writes + ", reads unlimited"
	}
	parts := []string{"no end date"}
	if d.Lifetime > 0 {
		parts[0] = "open for " + humanLifetime(d.Lifetime)
	}
	if d.MaxEntries > 0 {
		parts = append(parts, strconv.Itoa(d.MaxEntries)+" claims")
	}
	parts = append(parts, writes)
	return strings.Join(append(parts, "reads unlimited"), ", ")
}

// allow counts stores opened from one address in the last hour.
func (d *Demo) allow(addr string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	kept := d.opened[addr][:0]
	for _, t := range d.opened[addr] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	// Addresses that have gone quiet stop taking up room.
	for a, times := range d.opened {
		if len(times) > 0 && times[len(times)-1].Before(cutoff) {
			delete(d.opened, a)
		}
	}
	if len(kept) >= d.PerHour {
		d.opened[addr] = kept
		return false
	}
	d.opened[addr] = append(kept, time.Now())
	return true
}

func clientAddr(r *http.Request) string {
	// Behind Caddy on the same host; X-Forwarded-For is the person's address.
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// GET / — the page.
func (s *Server) getLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.servePage(w, r, "landing.html")
}

// GET /docs and GET /privacy — the two pages a directory listing links to.
func (s *Server) getDocsPage(w http.ResponseWriter, r *http.Request) { s.servePage(w, r, "docs.html") }

// GET /docs/{term} — a definition's rationale_ref (docs/claim, docs/truth …)
// lands on that term's row of /docs.
func (s *Server) getDocsTerm(w http.ResponseWriter, r *http.Request) {
	term := strings.ToLower(chi.URLParam(r, "term"))
	if !docsAnchor.MatchString(term) {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/docs#"+term, http.StatusFound)
}

var docsAnchor = regexp.MustCompile(`^[a-z][a-z-]{0,39}$`)

func (s *Server) getPrivacyPage(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "privacy.html")
}

// servePage fills one embedded page with what only the deployment knows.
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, name string) {
	page, err := landingFS.ReadFile(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "no_page", err.Error())
		return
	}
	// The contact address is configuration, not content: a page that names an
	// address nobody reads is worse than a page that names none. It is the
	// same address store_info gives an agent (store.feedback_email, else
	// TK_CONTACT), so the page, the docs and the agent name one address.
	contact := ""
	if s.Demo != nil {
		contact = s.Demo.Contact
	}
	if info, err := s.storeInfo(r.Context()); err == nil && info.FeedbackEmail != "" {
		contact = info.FeedbackEmail
	}
	// Which build answered, so a look at the page tells whether a deploy landed.
	page = bytes.ReplaceAll(page, []byte("{{VERSION}}"), []byte(html.EscapeString(version.String())))
	// The terms on the page are the terms the door applies, read from it.
	page = bytes.ReplaceAll(page, []byte("{{TERMS}}"), []byte(html.EscapeString(s.Demo.Terms())))
	words := "100"
	if s.Demo != nil && s.Demo.MaxWords > 0 {
		words = strconv.Itoa(s.Demo.MaxWords)
	}
	page = bytes.ReplaceAll(page, []byte("{{WORDS}}"), []byte(words))
	// Every contact block goes when there is no contact; a block left unclosed
	// stops the cutting rather than looping on it.
	for contact == "" {
		cut := cutBetween(page, "<!--contact-->", "<!--/contact-->")
		if len(cut) == len(page) {
			break
		}
		page = cut
	}
	page = bytes.ReplaceAll(page, []byte("{{CONTACT}}"), []byte(html.EscapeString(contact)))
	// The address to connect is this deployment's, never one written into the page.
	base := s.publicBase(r)
	page = bytes.ReplaceAll(page, []byte("{{MCP_URL}}"), []byte(html.EscapeString(base+"/mcp")))
	page = bytes.ReplaceAll(page, []byte("{{HOST}}"), []byte(html.EscapeString(strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"))))
	site := ""
	if s.Demo != nil {
		site = s.Demo.Site
	}
	for site == "" {
		cut := cutBetween(page, "<!--site-->", "<!--/site-->")
		if len(cut) == len(page) {
			break
		}
		page = cut
	}
	page = bytes.ReplaceAll(page, []byte("{{SITE}}"), []byte(html.EscapeString(site)))
	page = bytes.ReplaceAll(page, []byte("{{SITE_NAME}}"), []byte(html.EscapeString(strings.TrimPrefix(strings.TrimPrefix(site, "https://"), "http://"))))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// POST /v1/demo/open {email} — open a store for this address, or hand back the
// one it already has. There is no password and no confirmation loop: the email
// is how a person finds their store again, not a login.
func (s *Server) postDemoOpen(w http.ResponseWriter, r *http.Request) {
	if !s.Demo.on() {
		http.NotFound(w, r)
		return
	}
	in, ok := decodeInto[struct {
		Email string `json:"email"`
	}](w, r)
	if !ok {
		return
	}
	email, err := cleanEmail(in.Email)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{
			"code": "INVALID_EMAIL", "message": err.Error(),
			"next": "Check the address and try again.",
		}})
		return
	}

	// An address that already has a store gets that store back, not a second one.
	if existing, e := s.trialOf(r.Context(), email); e == nil && existing.Tenant != "" {
		// And not an unlimited number of mails: otherwise this form is a way to
		// mail any address over and over, from our domain.
		if !s.Demo.allow("mail:" + email) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{
				"code":    "TOO_MANY_SENDS",
				"message": "the link has been sent to this address several times in the last hour",
				"next":    "Look in the spam folder, or get in touch.",
			}})
			return
		}
		s.handOver(w, r, email, existing.Tenant, existing.Expires, true)
		return
	}

	if !s.Demo.allow(clientAddr(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{
			"code":    "TOO_MANY_STORES",
			"message": "this address has opened several stores in the last hour",
			"next":    "Use one of the links you already have, or come back in an hour.",
		}})
		return
	}
	ref := r.URL.Query().Get("ref")
	var refp *string
	if ref = strings.TrimSpace(ref); ref != "" && len(ref) <= 64 {
		refp = &ref
	}
	opened, err := s.openStore(r.Context(), email, refp)
	if err != nil {
		writeStore(w, err)
		return
	}
	s.handOver(w, r, email, opened.Tenant, opened.Expires, false)
}

// trial is what the store remembers about one address. Expires is nil for a
// store with no end date — a paid store (owner 2/10).
type trial struct {
	Tenant  string
	Expires *time.Time
}

// trialOf finds the store an address already has, if it is still open: no end
// date, or an end date still ahead. A store past its date has been or will be
// deleted, so the address gets a new one.
func (s *Server) trialOf(ctx context.Context, email string) (trial, error) {
	var t trial
	err := s.Repo.Pool.QueryRow(ctx, `
		SELECT o.tenant_id, t.expires_at FROM trial_owner o
		JOIN tenant t ON t.id = o.tenant_id
		WHERE o.email = $1 AND (t.expires_at IS NULL OR t.expires_at > now())`, email).Scan(&t.Tenant, &t.Expires)
	return t, err
}

// openStore opens a new store for an address on the door's terms — the same
// store whether the person came through the page or signed in through Claude:
// the starting principles, the lifetime, the ceiling.
func (s *Server) openStore(ctx context.Context, email string, ref *string) (trial, error) {
	id := make([]byte, 5)
	if _, err := rand.Read(id); err != nil {
		return trial{}, err
	}
	tenant := "demo-" + hex.EncodeToString(id)
	t := tka.Tenant{ID: tenant, Name: "trial " + tenant, Tier: "trial", Ref: ref, MaxClaim: &s.Demo.MaxClaim}
	var expires *time.Time
	if s.Demo.Lifetime > 0 {
		at := time.Now().Add(s.Demo.Lifetime)
		expires, t.ExpiresAt = &at, &at
	}
	if s.Demo.MaxEntries > 0 {
		t.MaxEntries = &s.Demo.MaxEntries
	}
	if s.Demo.MaxWords > 0 {
		t.MaxWords = &s.Demo.MaxWords
	}
	if _, err := s.tka().CreateTenant(ctx, t); err != nil {
		return trial{}, err
	}
	_ = s.tka().Step(ctx, tenant, tka.StepGrant)
	// A store with nothing in it teaches nothing, so the principles it will
	// reason from are there from the first minute.
	s.seedTrial(ctx, tenant)
	// The person is the store's first member, on a free seat. The seed above was
	// the door's writes, signed "trial", and costs the person none of theirs.
	if _, err := s.tka().SetSeat(ctx, tenant, email, tka.SeatFree, "door", "opened a trial store"); err != nil {
		return trial{}, err
	}
	if err := s.rememberTrial(ctx, email, tenant, ref); err != nil {
		return trial{}, err
	}
	return trial{Tenant: tenant, Expires: expires}, nil
}

// storeFor is the store an address works in: the one it already has, or a new
// one on the door's terms.
func (s *Server) storeFor(ctx context.Context, email string) (trial, error) {
	if existing, err := s.trialOf(ctx, email); err == nil && existing.Tenant != "" {
		return existing, nil
	}
	return s.openStore(ctx, email, nil)
}

// linkExpiry is how long a door link lives: as long as its store. A store with
// no end date (a paid store) gets a link with no end date either — the link is
// the person's own key, withdrawn through /v1/grants/revoke rather than by a
// clock (owner 2/10). OAuth access tokens are not door links: they always live
// one hour and are renewed by refresh.
func linkExpiry(expires *time.Time) int64 {
	if expires == nil {
		return 0
	}
	return expires.Unix()
}

// publicBase is the address people reach this deployment on.
func (s *Server) publicBase(r *http.Request) string {
	if s.Demo != nil && s.Demo.BaseURL != "" {
		return s.Demo.BaseURL
	}
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

// connectWays are the three doors into one store (brief 2/10 V8), none of them
// the default: Claude signs in by email at /mcp; Cursor or a script sends the
// grant as a header; a private link carries it in the path. The last two hold
// the grant itself, so they are present only where the grant may be shown.
func connectWays(base, token string) map[string]any {
	out := map[string]any{
		"claude": map[string]string{
			"url":  base + "/mcp",
			"note": "Add a custom connector with this URL, then sign in with your email when Claude asks.",
		},
	}
	if token != "" {
		out["header"] = map[string]string{
			"url":    base + "/mcp",
			"header": "Authorization: Bearer " + token,
			"note":   "For Cursor, scripts, or mcp-remote. Not yet tried on Cursor itself.",
		}
		out["private_link"] = map[string]string{
			"url":  base + "/mcp/t/" + token,
			"note": "One link that carries the key. Treat it like a password.",
		}
	}
	return out
}

func (s *Server) rememberTrial(ctx context.Context, email, tenant string, ref *string) error {
	_, err := s.Repo.Pool.Exec(ctx, `
		INSERT INTO trial_owner (email, tenant_id, ref) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE SET tenant_id = $2, ref = COALESCE($3, trial_owner.ref)`,
		email, tenant, ref)
	return err
}

// handOver mints the link, mails it, and answers. The link is in the response
// either way: mail can be slow, filtered or mistyped, and none of that should
// cost a person the thing they came for.
func (s *Server) handOver(w http.ResponseWriter, r *http.Request, email, tenant string, expires *time.Time, again bool) {
	token, err := grant.Sign(s.Demo.Key, grant.Grant{
		// The link and the header serve any client — Claude, Cursor, a script —
		// so the grant names none of them.
		UserID: email, ActorID: "agent", NodeID: "tknowledge", Aud: "tk",
		TenantID: tenant, ExpiresAt: linkExpiry(expires),
		Scope: []string{grant.ScopeRead, grant.ScopeWrite, grant.ScopeDemo},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sign", err.Error())
		return
	}

	base := s.publicBase(r)
	url := base + "/mcp/t/" + token

	// The link goes by mail and nowhere else (owner decision, 29/9). Putting it on the
	// page would mean any made-up address gets a working store, and the address
	// collected would then be worth nothing. Someone who wants this and does not
	// receive it will say so, and that message is worth more than a higher count.
	out := map[string]any{
		"email": email, "existing": again, "mailed": false,
		"expires_at": nil, "max_entries": nil, "writes_a_month": nil, "max_words": nil,
		"max_claim": s.Demo.MaxClaim, "terms": s.Demo.Terms(),
		"connect": connectWays(base, ""),
	}
	// The terms of the store handed over, not the door's defaults: an address
	// that already has a store (a paid one, say) gets that store's terms.
	if st, err := s.tka().ReadTenant(r.Context(), tenant); err == nil {
		if st.MaxEntries != nil {
			out["max_entries"] = *st.MaxEntries
		}
		if st.MaxWords != nil {
			out["max_words"] = *st.MaxWords
		}
		if st.MaxClaim != nil {
			out["max_claim"] = *st.MaxClaim
		}
	}
	// What this person may still write this month, on their own seat.
	if u, ok, err := s.tka().SeatUsage(r.Context(), tenant, email); err == nil && ok {
		out["seat"] = u.Plan
		if u.Limit != nil {
			out["writes_a_month"], out["writes_left"] = *u.Limit, *u.Left
		}
		out["writes_reset_at"] = u.ResetsAt.Format(time.RFC3339)
	}
	if expires != nil {
		out["expires_at"] = expires.UTC().Format(time.RFC3339)
	}
	if s.Demo.ShowLink {
		out["mcp_url"] = url
		out["connect"] = connectWays(base, token)
		out["show_link_warning"] = "This deployment shows the link on the page, so any address " +
			"that is typed in gets a working store. It is a testing setting."
	}

	if s.Demo.Mail == nil {
		// Without a mailer there is nothing to hand over — unless the link is being
		// shown on purpose, which is the testing case.
		if !s.Demo.ShowLink {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{
				"code":    "MAIL_UNAVAILABLE",
				"message": "this deployment cannot send mail, so it cannot hand over a store",
				"next":    "Get in touch and a store will be opened by hand.",
			}})
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	subject, text := linkMail(base, token, tenant, expires, s.Demo.Terms(), s.Demo.Site)
	if err := s.Demo.Mail.Send(r.Context(), email, subject, text); err != nil {
		log.Printf("demo: mail to %s failed: %v", email, err)
		if !s.Demo.ShowLink {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{
				"code":    "MAIL_FAILED",
				"message": "the store is open but the email did not go out",
				"next":    "Check the address, try again in a minute, or get in touch.",
			}})
			return
		}
		out["mail_error"] = "the email did not go out; the link is here instead"
		writeJSON(w, http.StatusOK, out)
		return
	}
	_, _ = s.Repo.Pool.Exec(r.Context(),
		`UPDATE trial_owner SET sent_count = sent_count + 1, last_sent = now() WHERE email = $1`, email)

	out["mailed"] = true
	writeJSON(w, http.StatusOK, out)
}

// trialPrinciples are the axioms a trial store starts with: the owner's seven,
// read from the starter template so the list has one home (owner decision, 1/10/2026).
// A stranger who does not hold one retires it and adds their own under a new
// code. A binary whose template cannot be read does not start.
var trialPrinciples = func() []setup.Principle {
	ps, err := setup.StarterPrinciples()
	if err != nil {
		panic("starter principles: " + err.Error())
	}
	return ps
}()

// seedTrial writes the starting principles. A failure here is not fatal: the
// store is open, and the person can add principles themselves.
func (s *Server) seedTrial(ctx context.Context, tenant string) []string {
	actor := tka.Actor{UserID: "trial", ActorID: "claude", GrantID: "gr_trial", TenantID: tenant}
	out := []string{}
	for _, p := range trialPrinciples {
		if _, err := s.tka().AddPrinciple(ctx, actor, p.Code, p.Statement, p.Note); err == nil {
			out = append(out, p.Code)
		}
	}
	return out
}

// cutBetween removes a marked block, so an unconfigured optional section leaves
// no trace on the page rather than an empty box.
func cutBetween(page []byte, open, close string) []byte {
	i := bytes.Index(page, []byte(open))
	j := bytes.Index(page, []byte(close))
	if i < 0 || j < 0 || j < i {
		return page
	}
	return append(page[:i:i], page[j+len(close):]...)
}
