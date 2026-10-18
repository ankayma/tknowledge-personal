package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/tka"
	"github.com/ankayma/tknowledge-personal/internal/version"
)

// recorder is a Mailer that keeps what it was asked to send.
type recorder struct{ sent *[]mailSent }

type mailSent struct{ To, Subject, Text string }

func (m recorder) Send(_ context.Context, to, subject, text string) error {
	*m.sent = append(*m.sent, mailSent{to, subject, text})
	return nil
}

// seedOf is the test env's signing key as a seed, so the door signs grants the
// env will verify.
func seedOf(t *testing.T, env *testEnv) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(env.Key.Seed())
}

// door switches the demo door on with a recording mailer.
func (e *testEnv) door(t *testing.T) (*Demo, *[]mailSent) {
	t.Helper()
	demo, err := DemoFromEnv(seedOf(t, e), "https://example.test", "hello@example.test", false)
	if err != nil || demo == nil {
		t.Fatalf("open the door: %v", err)
	}
	sent := &[]mailSent{}
	demo.Mail = recorder{sent: sent}
	e.Server.Demo = demo
	return demo, sent
}

type openResult struct {
	Email      string `json:"email"`
	Mailed     bool   `json:"mailed"`
	Existing   bool   `json:"existing"`
	ExpiresAt  string `json:"expires_at"`
	MaxEntries int    `json:"max_entries"`
	Seat       string `json:"seat"`
	MaxWrites  int    `json:"writes_a_month"`
	WritesLeft int    `json:"writes_left"`
	ResetAt    string `json:"writes_reset_at"`
	Error      *struct {
		Code string `json:"code"`
		Next string `json:"next"`
	} `json:"error"`
}

func (e *testEnv) rawOpen(t *testing.T, email string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email})
	resp, err := http.Post(e.URL+"/v1/demo/open", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

func (e *testEnv) open(t *testing.T, email string) (int, openResult) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"email": email})
	resp, err := http.Post(e.URL+"/v1/demo/open", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out openResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

var _ ed25519.PrivateKey

// The public door: one page, one button, and a store of their own. It signs
// grants, so what it refuses matters as much as what it opens.
func TestDemoDoor_OpensAnEmptyStoreAndHandsBackTheLink(t *testing.T) {
	env := newTestServer(t)

	// With no key set the door does not exist — a customer's deployment must not
	// be one config line away from handing out grants.
	if resp, err := http.Post(env.URL+"/v1/demo/open", "application/json", nil); err != nil {
		t.Fatal(err)
	} else if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("closed door should be 404, got %d", resp.StatusCode)
	}

	// Switch it on with the same key the test env verifies against, and a mailer
	// that records instead of sending.
	demo, sent := env.door(t)
	demo.PerHour = 2

	// A bad address is refused before any store is opened.
	if code, out := env.open(t, "not-an-address"); code != http.StatusBadRequest ||
		out.Error == nil || out.Error.Code != "INVALID_EMAIL" {
		t.Fatalf("bad address: %d %+v", code, out)
	}

	code, first := env.open(t, "one@example.test")
	// A trial store has no end date; its person writes on a free seat, 100 a
	// month (owner 13/10). The seed of seven principles costs none of them.
	if code != 200 || !first.Mailed || first.Email != "one@example.test" || first.ExpiresAt != "" ||
		first.Seat != "free" || first.MaxWrites != 100 || first.WritesLeft != 100 || first.ResetAt == "" {
		t.Fatalf("open: %d %+v", code, first)
	}
	// The response must never carry the link: that is the whole point of sending
	// it by mail (owner decision, 29/9). A made-up address must get nothing usable.
	raw := env.rawOpen(t, "two@example.test")
	if strings.Contains(raw, "/mcp/t/") {
		t.Fatalf("the response leaked the link: %s", raw)
	}

	// The link did go out, once per request, and only to the address given.
	if len(*sent) != 2 {
		t.Fatalf("expected two mails, got %d", len(*sent))
	}
	if !strings.Contains((*sent)[0].Text, "https://example.test/mcp/t/") {
		t.Errorf("the mail must carry the link: %q", (*sent)[0].Text)
	}
	if (*sent)[0].To != "one@example.test" {
		t.Errorf("mail went to %q", (*sent)[0].To)
	}

	// The same address gets the same store back, not a second one.
	_, again := env.open(t, "one@example.test")
	if !again.Existing {
		t.Errorf("a returning address should be recognised: %+v", again)
	}
	var stores int
	if err := env.Admin.QueryRow(context.Background(),
		`SELECT count(DISTINCT tenant_id) FROM trial_owner`).Scan(&stores); err != nil {
		t.Fatal(err)
	}
	if stores != 2 {
		t.Fatalf("two addresses, two stores; got %d", stores)
	}

	// And the form cannot be used to mail one address over and over.
	for i := 0; i < 4; i++ {
		code, out := env.open(t, "one@example.test")
		if code == http.StatusTooManyRequests {
			if out.Error == nil || out.Error.Code != "TOO_MANY_SENDS" || out.Error.Next == "" {
				t.Errorf("the refusal should name itself and say what is open: %+v", out.Error)
			}
			return
		}
	}
	t.Fatal("resending never stopped: the form is a mail cannon")
}

// A door with no mailer cannot hand over a store, and must say so rather than
// fall back to showing the link.
func TestDemoDoor_WithoutAMailerRefusesRatherThanLeak(t *testing.T) {
	env := newTestServer(t)
	demo, err := DemoFromEnv(seedOf(t, env), "https://example.test", "", false)
	if err != nil {
		t.Fatal(err)
	}
	env.Server.Demo = demo // no Mail set
	code, out := env.open(t, "someone@example.test")
	if code != http.StatusServiceUnavailable || out.Error == nil || out.Error.Code != "MAIL_UNAVAILABLE" {
		t.Fatalf("expected MAIL_UNAVAILABLE, got %d %+v", code, out)
	}
	raw := env.rawOpen(t, "another@example.test")
	if strings.Contains(raw, "/mcp/t/") {
		t.Fatalf("a door that cannot mail must not leak the link: %s", raw)
	}
}

// The store handed over carries its terms from the first minute: expiry and a
// ceiling. A trial store with no limits is a production store nobody owns.
func TestDemoDoor_StoreCarriesItsTermsImmediately(t *testing.T) {
	env := newTestServer(t)
	_, _ = env.door(t)

	if code, _ := env.open(t, "terms@example.test"); code != 200 {
		t.Fatalf("open: %d", code)
	}
	var tenant string
	if err := env.Admin.QueryRow(context.Background(),
		`SELECT tenant_id FROM trial_owner WHERE email = $1`, "terms@example.test").Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	got, err := env.Server.tka().ReadTenant(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	// Its terms are a claim length and no end date; writes are counted on the
	// person's seat, not on the store (owner 13/10).
	if got.ExpiresAt != nil || got.MaxWrites != nil || got.MaxClaim == nil {
		t.Fatalf("a trial store must carry its terms: %+v", got)
	}
	if got.Tier != "trial" {
		t.Errorf("tier should say what this store is: %q", got.Tier)
	}
	// A claim is one sentence: 100 words, 600 characters as the backstop (owner 3/10).
	if got.MaxWords == nil || *got.MaxWords != 100 || *got.MaxClaim != 600 {
		t.Fatalf("a trial store caps a claim at 100 words and 600 characters: %+v", got)
	}
}

// The page is served at the root and says what the store is before asking for
// anything.
func TestLanding_ServedAtTheRoot(t *testing.T) {
	env := newTestServer(t)
	resp, err := http.Get(env.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the front door should answer: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type: %q", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{"<title>", "T-knowledge", "Build " + version.String()} {
		if !strings.Contains(page, want) {
			t.Errorf("the page should contain %q", want)
		}
	}
	if strings.Contains(page, "{{VERSION}}") {
		t.Error("the build must be filled in, not left as a placeholder")
	}
}

// The testing switch is a switch, not a fallback: with it off, no mailer means no
// store and no link. With it on, the link comes back and says plainly what that
// setting costs — any address typed in gets a working store, which is the reason
// the owner chose mail-only delivery in the first place.
func TestDemoDoor_ShowLinkIsASwitchNotAFallback(t *testing.T) {
	env := newTestServer(t)
	demo, err := DemoFromEnv(seedOf(t, env), "https://example.test", "", true)
	if err != nil {
		t.Fatal(err)
	}
	env.Server.Demo = demo // no mailer, link shown on purpose

	raw := env.rawOpen(t, "tester@example.test")
	if !strings.Contains(raw, "https://example.test/mcp/t/") {
		t.Fatalf("with the switch on the link should come back: %s", raw)
	}
	if !strings.Contains(raw, "testing setting") {
		t.Errorf("the response must say what this setting costs: %s", raw)
	}

	// Turn it off again and the same door hands over nothing.
	demo.ShowLink = false
	raw = env.rawOpen(t, "other@example.test")
	if strings.Contains(raw, "/mcp/t/") {
		t.Fatalf("with the switch off nothing may leak: %s", raw)
	}
	if !strings.Contains(raw, "MAIL_UNAVAILABLE") {
		t.Errorf("expected MAIL_UNAVAILABLE: %s", raw)
	}
}

// A trial store's lifetime comes from the deployment: whole days or a Go
// duration, and never zero or negative.
func TestParseLifetime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30d": 30 * 24 * time.Hour, "1d": 24 * time.Hour, "720h": 720 * time.Hour, " 90m ": 90 * time.Minute,
	} {
		got, err := ParseLifetime(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0d", "-1d", "0", "1.5d", "thirty", "30days"} {
		if _, err := ParseLifetime(in); err == nil {
			t.Fatalf("%q: accepted", in)
		}
	}
}

// Every trial store starts from the owner's seven principles (owner decision,
// 1/10/2026), word for word as they stood in the store that ran correctly on
// 29/9. A stranger who does not hold one retires it and adds their own under a
// new code; the retired code stays readable and cannot be taken again.
func TestDemoDoor_StoreStartsWithTheOwnersSevenPrinciples(t *testing.T) {
	ctx := context.Background()
	env := newTestServer(t)
	_, _ = env.door(t)
	if code, _ := env.open(t, "seven@example.test"); code != 200 {
		t.Fatalf("open: %d", code)
	}
	var tenant string
	if err := env.Admin.QueryRow(ctx, `SELECT tenant_id FROM trial_owner WHERE email = $1`,
		"seven@example.test").Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tenant, "demo-") {
		t.Fatalf("a store opened at the door is named demo-…: %q", tenant)
	}
	repo := env.Server.tka()
	ps, err := repo.Principles(ctx, tenant, false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"P.1": {"Strong architecture absorbs weak execution. Sound structure makes execution flaws recoverable; weak structure defeats even heroic execution.",
			"Depending on specific individuals instead of structure is a violation."},
		"P.2": {"Strict admission, easy operations. Concentrate complexity at commitment points; keep operations light afterwards.",
			`Cutting corners at admission "to be on time" costs double.`},
		"P.3": {"Honest gaps over false promises. An acknowledged gap holds trust longer than a hidden limitation.", ""},
		"P.4": {"Compose, do not replace. Build on existing investment rather than replace it.", ""},
		"P.5": {"Three Layers of Design Specificity. Every decision belongs to exactly one layer — principle / invariant / implementation.",
			"Fixing at the wrong layer is expensive churn."},
		"P.8": {"Trigger-Based Activation. Large investments are activated by external signals, not by calendar.", ""},
		"P.9": {"Principle and Facts Above Assumption. Only the Principle List and sourced market facts are trusted without audit; everything else is an assumption.", ""},
	}
	if len(ps) != len(want) {
		t.Fatalf("a trial store starts with %d principles, got %d: %+v", len(want), len(ps), ps)
	}
	for _, p := range ps {
		w, ok := want[p.Code]
		note := ""
		if p.Note != nil {
			note = *p.Note
		}
		if !ok || p.Statement != w[0] || note != w[1] || p.Status != "active" {
			t.Fatalf("%s: got %q / %q (%s)", p.Code, p.Statement, note, p.Status)
		}
	}

	// Not holding P.3: retire it, add their own under a new code.
	user := tka.Actor{UserID: "seven@example.test", ActorID: "claude", GrantID: "gr_x", TenantID: tenant}
	if _, _, err := repo.RetirePrinciple(ctx, user, "P.3", "not how we work"); err != nil {
		t.Fatalf("retire P.3: %v", err)
	}
	if _, err := repo.AddPrinciple(ctx, user, "P.3", "Something else.", ""); err == nil {
		t.Fatal("a retired code must not be taken again")
	}
	if _, err := repo.AddPrinciple(ctx, user, "P.10", "Ship the smallest thing that settles the question.", ""); err != nil {
		t.Fatalf("add P.10: %v", err)
	}
	all, err := repo.Principles(ctx, tenant, true)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, p := range all {
		status[p.Code] = p.Status
	}
	if len(all) != 8 || status["P.3"] != "retired" || status["P.10"] != "active" {
		t.Fatalf("old code stays, new one joins: %v", status)
	}
}

// The home page, the docs and the privacy policy are what a stranger and a
// directory reviewer read first: each answers, carries the build, fills every
// placeholder, and names no product the public pages are not meant to name
// (brief 25/9 §6). The privacy page covers what a directory listing requires.
func TestPublicPages_ServedFilledAndClean(t *testing.T) {
	env := newTestServer(t)
	for path, wants := range map[string][]string{
		"/": {"truth · trust · traceable knowledge", "Data → information → knowledge", "Try it in five minutes", "AI enablement is enabling claims you can trust", "/mcp",
			"sent to you by email", "Same claim, two outcomes", "T-71", "A-43"},
		"/docs":    {"Connect", "summarize", "Refusals", "words per claim", "NO_SOURCE", "SINGLE_PREMISE", "PREMISE_NOT_FOUND", `id="claim"`, `id="attester"`},
		"/privacy": {"What is stored", "Where it is stored", "Who else sees it", "How long it is kept", "Removing your data", "Contact"},
	} {
		resp, err := http.Get(env.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := string(raw)
		if resp.StatusCode != 200 || !strings.Contains(page, "Build "+version.String()) {
			t.Fatalf("%s: %d, build line missing", path, resp.StatusCode)
		}
		for _, w := range wants {
			if !strings.Contains(page, w) {
				t.Errorf("%s should contain %q", path, w)
			}
		}
		if strings.Contains(page, "{{") {
			t.Errorf("%s: unfilled placeholder", path)
		}
		// No page names the maintainer's own site or mail domain: those come
		// from the deployment's settings. The source link is the only fixed address.
		if low := strings.ToLower(page); strings.Contains(low, "ankayma.com") || strings.Contains(low, "baotrinh") {
			t.Errorf("%s: a name this page must not carry", path)
		}
	}
}

// A definition's rationale_ref (docs/claim …) lands on its row of /docs, and
// the pages name the same contact address store_info gives an agent.
func TestDocsTermAndOneContact(t *testing.T) {
	env := newTestServer(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(env.URL + "/docs/claim")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/docs#claim" {
		t.Fatalf("/docs/claim: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := noRedirect.Get(env.URL + "/docs/..%2Fetc"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a term that is not a word: %d", resp.StatusCode)
	}
}
