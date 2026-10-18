package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/tka"
)

const (
	claudeCallback = "https://claude.ai/api/mcp/auth_callback"
	testVerifier   = "verifier-verifier-verifier-verifier-verifier-0123"
)

func challengeOf(v string) string {
	s := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

// enableOAuth opens the door with a recording mailer and the authorization
// server, the issuer being the test server.
func (e *testEnv) enableOAuth(t *testing.T) *[]mailSent {
	t.Helper()
	_, sent := e.door(t)
	e.Server.OAuth = &OAuth{Issuer: e.URL, Resource: e.URL + "/mcp", Key: e.Key, Aud: testAud}
	return sent
}

// fakeClaude serves a client metadata document over TLS, as claude.ai does.
func fakeClaude(t *testing.T, redirects ...string) (*httptest.Server, string) {
	t.Helper()
	var clientID string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/client.json" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id": clientID, "client_name": "Claude (self-declared)", "redirect_uris": redirects,
			"token_endpoint_auth_method": "none",
		})
	}))
	t.Cleanup(ts.Close)
	clientID = ts.URL + "/oauth/client.json"
	return ts, clientID
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func authzValues(clientID, redirect, scope string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {challengeOf(testVerifier)}, "code_challenge_method": {"S256"},
		"scope": {scope}, "state": {"xyz"},
	}
}

func postAuthz(t *testing.T, e *testEnv, v url.Values, step string, extra map[string]string) *http.Response {
	t.Helper()
	form := url.Values{"step": {step}}
	for k, vs := range v {
		form[k] = vs
	}
	for k, s := range extra {
		form.Set(k, s)
	}
	resp, err := noFollow.PostForm(e.URL+"/oauth/authorize", form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

var sixDigits = regexp.MustCompile(`\b\d{6}\b`)

// signIn runs authorize up to the redirect and returns the code.
func signIn(t *testing.T, e *testEnv, sent *[]mailSent, v url.Values, email string) string {
	t.Helper()
	postAuthz(t, e, v, "email", map[string]string{"email": email}).Body.Close()
	if len(*sent) == 0 {
		t.Fatal("no code was mailed")
	}
	code := sixDigits.FindString((*sent)[len(*sent)-1].Text)
	resp := postAuthz(t, e, v, "code", map[string]string{"email": email, "code": code})
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc == nil || loc.Query().Get("code") == "" {
		t.Fatalf("authorize: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != e.URL {
		t.Fatalf("redirect must carry state and iss: %s", loc)
	}
	return loc.Query().Get("code")
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func token(t *testing.T, e *testEnv, form url.Values) (int, tokenResp) {
	t.Helper()
	resp, err := http.PostForm(e.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out tokenResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestOAuth_DiscoveryDocuments(t *testing.T) {
	env := newTestServer(t)
	if resp, _ := http.Get(env.URL + "/.well-known/oauth-protected-resource/mcp"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no authorization server, no discovery: %d", resp.StatusCode)
	}
	env.enableOAuth(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		var pr map[string]any
		resp, _ := http.Get(env.URL + path)
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		if pr["resource"] != env.URL+"/mcp" || pr["authorization_servers"].([]any)[0] != env.URL {
			t.Fatalf("%s: %v", path, pr)
		}
	}
	var as map[string]any
	resp, _ := http.Get(env.URL + "/.well-known/oauth-authorization-server")
	_ = json.NewDecoder(resp.Body).Decode(&as)
	resp.Body.Close()
	// Claude takes CIMD only when both of these are advertised.
	if as["client_id_metadata_document_supported"] != true ||
		!containsAny(as["token_endpoint_auth_methods_supported"], "none") ||
		!containsAny(as["code_challenge_methods_supported"], "S256") ||
		!containsAny(as["scopes_supported"], "offline_access") ||
		as["registration_endpoint"] != env.URL+"/oauth/register" || as["issuer"] != env.URL {
		t.Fatalf("authorization server metadata: %v", as)
	}

	// A host that cannot fetch client metadata stops advertising CIMD, and Claude
	// registers through DCR instead; registration stays advertised.
	env.Server.OAuth.NoCIMD = true
	as = map[string]any{}
	resp, _ = http.Get(env.URL + "/.well-known/oauth-authorization-server")
	_ = json.NewDecoder(resp.Body).Decode(&as)
	resp.Body.Close()
	if as["client_id_metadata_document_supported"] != false || as["registration_endpoint"] != env.URL+"/oauth/register" {
		t.Fatalf("CIMD off must leave DCR: %v", as)
	}
}

func containsAny(v any, want string) bool {
	list, _ := v.([]any)
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

// Claude's path end to end: its client metadata document, the consent naming
// the host (not the self-declared name), a code by email, the exchange, and an
// access token that reaches a store of the person's own with seven principles.
func TestOAuth_CIMDFlowToAStore(t *testing.T) {
	env := newTestServer(t)
	sent := env.enableOAuth(t)
	fake, clientID := fakeClaude(t, claudeCallback)
	env.Server.OAuth.HTTP = fake.Client()

	v := authzValues(clientID, claudeCallback, "ledger:read ledger:write offline_access")
	resp, err := http.Get(env.URL + "/oauth/authorize?" + v.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	host, _ := url.Parse(clientID)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), host.Host) ||
		strings.Contains(string(page), "self-declared") || !strings.Contains(string(page), "claude.ai") {
		t.Fatalf("consent must name the client's host and the redirect host: %d %s", resp.StatusCode, page)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("the consent page must not be framed")
	}

	code := signIn(t, env, sent, v, "new.person@example.test")
	st, tok := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {clientID}, "redirect_uri": {claudeCallback}})
	if st != http.StatusOK || tok.TokenType != "Bearer" || tok.ExpiresIn != 3600 || tok.RefreshToken == "" {
		t.Fatalf("token: %d %+v", st, tok)
	}
	g, err := env.Server.Grants.Verify(tok.AccessToken)
	if err != nil || g.Via != grant.ViaOAuth || g.UserID != "new.person@example.test" ||
		!strings.HasPrefix(g.TenantID, "demo-") || !g.Has(grant.ScopeWrite) || !g.Has(grant.ScopeDemo) {
		t.Fatalf("access token: %+v %v", g, err)
	}
	if left := time.Until(time.Unix(g.ExpiresAt, 0)); left < 55*time.Minute || left > 61*time.Minute {
		t.Fatalf("access token lives one hour: %s", left)
	}
	ps, err := env.Server.tka().Principles(context.Background(), g.TenantID, false)
	if err != nil || len(ps) != 7 {
		t.Fatalf("a signed-in store starts with the seven principles: %d %v", len(ps), err)
	}
	// Door terms (owner 13/10): no end date, no store-wide cap; the person is a
	// member on a free seat with this month's 100 writes untouched by the seed.
	ten, _ := env.Server.tka().ReadTenant(context.Background(), g.TenantID)
	if ten.MaxWrites != nil || ten.ExpiresAt != nil || ten.Tier != "trial" {
		t.Fatalf("door terms: %+v", ten)
	}
	if u, ok, err := env.Server.tka().SeatUsage(context.Background(), g.TenantID, "new.person@example.test"); err != nil || !ok ||
		u.Plan != "free" || u.Used != 0 || u.Limit == nil || *u.Limit != 100 {
		t.Fatalf("seat: %+v %v %v", u, ok, err)
	}

	// Signing in again lands in the same store.
	code2 := signIn(t, env, sent, v, "new.person@example.test")
	_, tok2 := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code2},
		"code_verifier": {testVerifier}, "client_id": {clientID}})
	if g2, _ := grant.Claims(tok2.AccessToken); g2.TenantID != g.TenantID {
		t.Fatalf("second sign-in opened another store: %s vs %s", g2.TenantID, g.TenantID)
	}

	// The code works once; a second exchange is invalid_grant.
	if st, out := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {clientID}}); st != http.StatusBadRequest || out.Error != "invalid_grant" {
		t.Fatalf("code reuse: %d %+v", st, out)
	}
}

func TestOAuth_TokenEndpointRefusals(t *testing.T) {
	env := newTestServer(t)
	sent := env.enableOAuth(t)
	fake, clientID := fakeClaude(t, claudeCallback)
	env.Server.OAuth.HTTP = fake.Client()
	v := authzValues(clientID, claudeCallback, "")

	// JSON is not what a token endpoint takes.
	resp, _ := http.Post(env.URL+"/oauth/token", "application/json", strings.NewReader(`{"grant_type":"authorization_code"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("json body: %d", resp.StatusCode)
	}
	resp.Body.Close()

	code := signIn(t, env, sent, v, "a@example.test")
	if st, out := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {"wrong-verifier-wrong-verifier-wrong-verifier-00"}, "client_id": {clientID}}); st != 400 || out.Error != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %+v", st, out)
	}
	// A failed exchange still spent the code.
	if st, out := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {clientID}}); st != 400 || out.Error != "invalid_grant" {
		t.Fatalf("spent code: %d %+v", st, out)
	}
	code = signIn(t, env, sent, v, "a@example.test")
	if st, out := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {"https://other.example/client.json"}}); st != 400 || out.Error != "invalid_grant" {
		t.Fatalf("another client: %d %+v", st, out)
	}
	if st, out := token(t, env, url.Values{"grant_type": {"password"}}); st != 400 || out.Error != "unsupported_grant_type" {
		t.Fatalf("grant type: %d %+v", st, out)
	}
}

// Refresh tokens rotate; presenting a rotated one again kills the family, so
// the copy that was not reused stops working too.
func TestOAuth_RefreshRotationAndReuse(t *testing.T) {
	env := newTestServer(t)
	sent := env.enableOAuth(t)
	fake, clientID := fakeClaude(t, claudeCallback)
	env.Server.OAuth.HTTP = fake.Client()
	v := authzValues(clientID, claudeCallback, "ledger:read ledger:write")
	code := signIn(t, env, sent, v, "r@example.test")
	_, first := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {clientID}})

	refresh := func(rt string, extra ...string) (int, tokenResp) {
		f := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {clientID}}
		if len(extra) == 2 {
			f.Set(extra[0], extra[1])
		}
		return token(t, env, f)
	}
	st, second := refresh(first.RefreshToken)
	if st != 200 || second.RefreshToken == "" || second.RefreshToken == first.RefreshToken || second.AccessToken == "" {
		t.Fatalf("rotation: %d %+v", st, second)
	}
	if st, out := refresh(first.RefreshToken, "scope", "ledger:read ledger:write ledger:audit"); st != 400 || out.Error != "invalid_grant" {
		t.Fatalf("reusing a rotated token: %d %+v", st, out)
	}
	if st, out := refresh(second.RefreshToken); st != 400 || out.Error != "invalid_grant" {
		t.Fatalf("the family must be revoked after reuse: %d %+v", st, out)
	}
	if st, out := refresh("nonsense"); st != 400 || out.Error != "invalid_grant" {
		t.Fatalf("unknown token: %d %+v", st, out)
	}

	// Narrowing on refresh is allowed, widening is not.
	code = signIn(t, env, sent, v, "r@example.test")
	_, fresh := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {clientID}})
	if st, out := refresh(fresh.RefreshToken, "scope", "ledger:read ledger:write ledger:audit"); st != 400 || out.Error != "invalid_scope" {
		t.Fatalf("widen: %d %+v", st, out)
	}
	st, narrow := refresh(fresh.RefreshToken, "scope", "ledger:read")
	if g, _ := grant.Claims(narrow.AccessToken); st != 200 || g.Has(grant.ScopeWrite) {
		t.Fatalf("narrow: %d %+v", st, g)
	}
}

// Dynamic registration for clients without a metadata document; a loopback
// redirect matches on any port (Claude Code); anything not registered is
// refused on the page, never redirected to.
func TestOAuth_DCRAndRedirectChecks(t *testing.T) {
	env := newTestServer(t)
	env.enableOAuth(t)
	reg := func(body string) (int, map[string]any) {
		resp, err := http.Post(env.URL+"/oauth/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if st, _ := reg(`{"redirect_uris":["http://evil.example/cb"]}`); st != 400 {
		t.Fatalf("plain http off loopback: %d", st)
	}
	if st, _ := reg(`{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`); st != 400 {
		t.Fatalf("confidential client: %d", st)
	}
	st, client := reg(`{"redirect_uris":["http://localhost/callback","http://127.0.0.1/callback"],"client_name":"cli"}`)
	id, _ := client["client_id"].(string)
	if st != http.StatusCreated || id == "" || client["token_endpoint_auth_method"] != "none" {
		t.Fatalf("register: %d %v", st, client)
	}
	for _, redirect := range []string{"http://localhost:3118/callback", "http://127.0.0.1:61000/callback"} {
		resp, _ := http.Get(env.URL + "/oauth/authorize?" + authzValues(id, redirect, "").Encode())
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(page), "your own computer") {
			t.Fatalf("loopback %s: %d", redirect, resp.StatusCode)
		}
	}
	// The sign-in page's form ends in a 302 to the client. Browsers apply CSP
	// form-action to that redirect, so the page must allow the redirect's origin
	// — with 'self' alone Safari stayed on the page and Claude never got its code
	// (3/10/2026). An unvalidated page allows nothing but itself.
	reg2, claude := reg(`{"redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"client_name":"Claude"}`)
	if reg2 != http.StatusCreated {
		t.Fatalf("register claude: %d", reg2)
	}
	for redirect, want := range map[string]string{
		"https://claude.ai/api/mcp/auth_callback": "form-action 'self' https://claude.ai;",
		"http://localhost:3118/callback":          "form-action 'self' http://localhost:3118;",
	} {
		cid := id
		if strings.HasPrefix(redirect, "https://claude.ai") {
			cid = claude["client_id"].(string)
		}
		resp, _ := http.Get(env.URL + "/oauth/authorize?" + authzValues(cid, redirect, "").Encode())
		resp.Body.Close()
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, want) {
			t.Fatalf("%s: CSP %q must contain %q", redirect, csp, want)
		}
	}
	for _, redirect := range []string{"http://localhost:3118/other", "https://evil.example/callback", "http://10.0.0.1/callback"} {
		resp, _ := noFollow.Get(env.URL + "/oauth/authorize?" + authzValues(id, redirect, "").Encode())
		if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "evil.example") || strings.Contains(csp, "10.0.0.1") {
			t.Fatalf("a refused redirect must not be allowed by CSP: %q", csp)
		}
		resp.Body.Close()
		resp, _ = noFollow.Get(env.URL + "/oauth/authorize?" + authzValues(id, redirect, "").Encode())
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Fatalf("unregistered redirect %s: %d %q", redirect, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// With the client and redirect established, other errors go back to the client.
	v := authzValues(id, "http://127.0.0.1:5000/callback", "")
	v.Set("code_challenge_method", "plain")
	resp, _ := noFollow.Get(env.URL + "/oauth/authorize?" + v.Encode())
	resp.Body.Close()
	if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != http.StatusFound || loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("plain PKCE: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	v = authzValues(id, "http://127.0.0.1:5000/callback", "admin")
	resp, _ = noFollow.Get(env.URL + "/oauth/authorize?" + v.Encode())
	resp.Body.Close()
	if loc, _ := url.Parse(resp.Header.Get("Location")); loc.Query().Get("error") != "invalid_scope" {
		t.Fatalf("unknown scope: %q", resp.Header.Get("Location"))
	}
}

// The email code: wrong codes count, five wrong ones end it, a used code is
// gone; the review account takes its fixed code and nothing else does.
func TestOAuth_EmailCodeAndReviewAccount(t *testing.T) {
	env := newTestServer(t)
	sent := env.enableOAuth(t)
	env.Server.OAuth.ReviewEmail, env.Server.OAuth.ReviewCode = "reviewer@example.test", "135792"
	fake, clientID := fakeClaude(t, claudeCallback)
	env.Server.OAuth.HTTP = fake.Client()
	v := authzValues(clientID, claudeCallback, "")

	postAuthz(t, env, v, "email", map[string]string{"email": "c@example.test"}).Body.Close()
	for i := 0; i < 5; i++ {
		resp := postAuthz(t, env, v, "code", map[string]string{"email": "c@example.test", "code": "000000"})
		resp.Body.Close()
		if resp.StatusCode == http.StatusFound {
			t.Fatal("a wrong code signed someone in")
		}
	}
	good := sixDigits.FindString((*sent)[len(*sent)-1].Text)
	resp := postAuthz(t, env, v, "code", map[string]string{"email": "c@example.test", "code": good})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusFound || !strings.Contains(string(body), "Too many tries") {
		t.Fatalf("after five wrong tries the code is dead: %d", resp.StatusCode)
	}

	before := len(*sent)
	postAuthz(t, env, v, "email", map[string]string{"email": "Reviewer@Example.test"}).Body.Close()
	if len(*sent) != before {
		t.Fatal("nothing is mailed to the review account")
	}
	resp = postAuthz(t, env, v, "code", map[string]string{"email": "reviewer@example.test", "code": "111111"})
	resp.Body.Close()
	if resp.StatusCode == http.StatusFound {
		t.Fatal("the review account takes only its own code")
	}
	resp = postAuthz(t, env, v, "code", map[string]string{"email": "reviewer@example.test", "code": "135792"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("review account: %d", resp.StatusCode)
	}
	// The review code is not a master key for other addresses.
	postAuthz(t, env, v, "email", map[string]string{"email": "d@example.test"}).Body.Close()
	resp = postAuthz(t, env, v, "code", map[string]string{"email": "d@example.test", "code": "135792"})
	resp.Body.Close()
	if resp.StatusCode == http.StatusFound {
		t.Fatal("the review code signed in another address")
	}
}

// A paid store (owner 2/10): no end date, no ceiling. Signing in lands in it
// rather than opening a new one, writes past any trial ceiling go through, and
// a door link for it carries no expiry of its own.
func TestOAuth_PaidStoreWithNoExpiry(t *testing.T) {
	env := newTestServer(t)
	sent := env.enableOAuth(t)
	fake, clientID := fakeClaude(t, claudeCallback)
	env.Server.OAuth.HTTP = fake.Client()
	ctx := context.Background()

	const owner = "owner@example.test"
	if _, err := env.Server.tka().CreateTenant(ctx, tka.Tenant{ID: "demo-paid", Name: "paid", Tier: "paid"}); err != nil {
		t.Fatal(err)
	}
	if err := env.Server.rememberTrial(ctx, owner, "demo-paid", nil); err != nil {
		t.Fatal(err)
	}
	// 1001 entries already: beyond both the old 60 and the new 1000.
	if _, err := env.Admin.Exec(ctx, `
		INSERT INTO assumption (tenant_id, num, claim, claim_key, type, source_kind, signer, producer, grant_id)
		SELECT 'demo-paid', n, 'entry ' || n, 'entry ' || n, 'A-r', 'owner', $1, 'claude', 'g'
		FROM generate_series(1, 1001) n`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Admin.Exec(ctx, `
		INSERT INTO tenant_counter (tenant_id, kind, next) VALUES ('demo-paid', 'A', 1002)
		ON CONFLICT (tenant_id, kind) DO UPDATE SET next = 1002`); err != nil {
		t.Fatal(err)
	}

	v := authzValues(clientID, claudeCallback, "")
	code := signIn(t, env, sent, v, owner)
	_, tok := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {testVerifier}, "client_id": {clientID}})
	g, err := env.Server.Grants.Verify(tok.AccessToken)
	if err != nil || g.TenantID != "demo-paid" {
		t.Fatalf("sign-in must land in the paid store: %+v %v", g, err)
	}
	if g.ExpiresAt == 0 || time.Until(time.Unix(g.ExpiresAt, 0)) > 61*time.Minute {
		t.Fatalf("an access token still lives one hour: %d", g.ExpiresAt)
	}
	resp := postJSON(t, env.URL+"/v1/tka/assume", tok.AccessToken, map[string]any{
		"claim": "Entry number 1002 goes in past any trial ceiling", "type": "A-r", "source": "owner"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "TENANT_FULL") {
		t.Fatalf("a paid store has no ceiling: %d %s", resp.StatusCode, body)
	}

	// The door hands the same store back, with a link that has no expiry of its
	// own and no expires_at to report.
	env.Server.Demo.ShowLink = true
	st, out := env.open(t, owner)
	if st != 200 || !out.Existing || out.ExpiresAt != "" {
		t.Fatalf("door for a paid store: %d %+v", st, out)
	}
	var full map[string]any
	_ = json.Unmarshal([]byte(env.rawOpen(t, owner)), &full)
	link, _ := full["mcp_url"].(string)
	gr, ok := grant.Claims(strings.TrimPrefix(link, "https://example.test/mcp/t/"))
	if !ok || gr.TenantID != "demo-paid" || gr.ExpiresAt != 0 {
		t.Fatalf("door link for a paid store: %+v", gr)
	}
}

// The testing switch that shows codes on the page never shows one for an
// address that already owns a store: that would let anyone typing the address
// into its store (the owner's paid store among them).
func TestOAuth_ShownCodeNeverForAnExistingStore(t *testing.T) {
	env := newTestServer(t)
	env.enableOAuth(t)
	env.Server.Demo.Mail = nil
	env.Server.Demo.ShowLink = true
	fake, clientID := fakeClaude(t, claudeCallback)
	env.Server.OAuth.HTTP = fake.Client()
	v := authzValues(clientID, claudeCallback, "")

	pageFor := func(email string) string {
		resp := postAuthz(t, env, v, "email", map[string]string{"email": email})
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	if page := pageFor("newcomer@example.test"); !strings.Contains(page, "Testing build: the code is") {
		t.Fatal("a new address sees its code in the testing build")
	}
	if _, err := env.Server.tka().CreateTenant(context.Background(), tka.Tenant{ID: "demo-owned", Name: "x", Tier: "paid"}); err != nil {
		t.Fatal(err)
	}
	if err := env.Server.rememberTrial(context.Background(), "owner@example.test", "demo-owned", nil); err != nil {
		t.Fatal(err)
	}
	page := pageFor("owner@example.test")
	if sixDigits.MatchString(page) || strings.Contains(page, "Testing build") || !strings.Contains(page, "cannot send mail") {
		t.Fatal("the code for an address that owns a store must never be on the page")
	}
}

// The fetch of a client metadata document is fenced: https on 443 only, and
// never to a non-public address however the name resolves.
func TestOAuth_CIMDFetchGuard(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false, "fc00::1": false, "fe80::1": false,
	} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("publicIP(%s) = %v", ip, got)
		}
	}
	if guardDial("tcp", "8.8.8.8:80", nil) == nil || guardDial("tcp", "127.0.0.1:443", nil) == nil ||
		guardDial("tcp", "169.254.169.254:443", nil) == nil || guardDial("tcp", "8.8.8.8:443", nil) != nil {
		t.Fatal("guardDial lets the wrong address through")
	}

	o := &OAuth{} // the default, guarded fetcher
	fake, clientID := fakeClaude(t, claudeCallback)
	_ = fake
	if _, err := o.fetchCIMD(context.Background(), clientID); err == nil {
		t.Fatal("a metadata document on a loopback address must not be fetched")
	}
	if _, err := o.fetchCIMD(context.Background(), "http://example.com/client.json"); err == nil {
		t.Fatal("a metadata document over plain http must not be fetched")
	}
	// The document must name itself.
	liar := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "https://claude.ai/oauth/x", "redirect_uris": []string{claudeCallback}})
	}))
	defer liar.Close()
	o.HTTP = liar.Client()
	if _, err := o.fetchCIMD(context.Background(), liar.URL+"/client.json"); err == nil {
		t.Fatal("a document naming another client_id must be refused")
	}
}

// Store-free tools answer without a grant (lazy auth), so Claude can use them
// before anyone signs in.
func TestLazyAuth_StoreFreeRoutesNeedNoGrant(t *testing.T) {
	env := newTestServer(t)
	resp := postJSON(t, env.URL+"/v1/tka/check-claim", "", map[string]any{"claim": "Acme signed on 2026-09-30"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("check-claim without a grant: %d", resp.StatusCode)
	}
	r2, _ := http.Get(env.URL + "/v1/tka/definitions")
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("definitions without a grant: %d", r2.StatusCode)
	}
	resp = postJSON(t, env.URL+"/v1/tka/assume", "", map[string]any{"claim": "x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a write still needs a grant: %d", resp.StatusCode)
	}
}

func TestDoorTerms_DefaultsAndOverride(t *testing.T) {
	d, err := DemoFromEnv(base64.StdEncoding.EncodeToString(make([]byte, 32)), "", "", false)
	// No end date; writes are counted by seat, 100 a month on free (owner 13/10).
	if err != nil || d.MaxEntries != 0 || d.Lifetime != 0 ||
		d.Terms() != "no end date, 100 writes a month, reads unlimited" {
		t.Fatalf("defaults: %+v %q %v", d, d.Terms(), err)
	}
	for in, want := range map[string]int{"1000": 1000, " 60 ": 60, "5000": 5000} {
		if n, err := ParseMaxEntries(in); err != nil || n != want {
			t.Fatalf("ParseMaxEntries(%q) = %d %v", in, n, err)
		}
	}
	for _, in := range []string{"", "0", "-1", "1.5", "lots"} {
		if _, err := ParseMaxEntries(in); err == nil {
			t.Fatalf("ParseMaxEntries(%q) accepted", in)
		}
	}
	env := newTestServer(t)
	env.door(t)
	resp, _ := http.Get(env.URL + "/")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "no end date, 100 writes a month, reads unlimited") || strings.Contains(string(page), "{{") {
		t.Fatal("the page states the door's own terms")
	}
}
