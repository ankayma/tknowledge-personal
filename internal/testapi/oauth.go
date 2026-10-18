package testapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/api"
)

// EnableOAuth opens the door (showing links and codes, no mailer — the testing
// switch) and the authorization server, with a review account. The issuer is
// the test server itself.
func (e *Env) EnableOAuth(t *testing.T, reviewEmail, reviewCode string) {
	t.Helper()
	demo, err := api.DemoFromEnv(base64.StdEncoding.EncodeToString(e.Key.Seed()), e.Server.URL, "", true)
	if err != nil || demo == nil {
		t.Fatalf("door: %v", err)
	}
	e.API.Demo = demo
	e.API.OAuth = &api.OAuth{
		Issuer: e.Server.URL, Resource: e.Server.URL + "/mcp", Key: e.Key, Aud: Aud,
		ReviewEmail: reviewEmail, ReviewCode: reviewCode,
	}
}

// Tokens is what /oauth/token answers.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

// OAuthLogin signs email in through the whole flow — dynamic registration with
// a loopback redirect, authorize, the email code, the token exchange — and
// returns the tokens. code is the review account's fixed code.
func (e *Env) OAuthLogin(t *testing.T, email, code, scope string) Tokens {
	t.Helper()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	reg, _ := json.Marshal(map[string]any{"redirect_uris": []string{"http://127.0.0.1/callback"}, "client_name": "test"})
	resp, err := http.Post(e.Server.URL+"/oauth/register", "application/json", bytes.NewReader(reg))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %v %v", err, resp)
	}
	var client struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&client)
	resp.Body.Close()

	verifier := "test-verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	authz := url.Values{
		"response_type": {"code"}, "client_id": {client.ClientID}, "redirect_uri": {"http://127.0.0.1:53111/callback"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"scope": {scope}, "state": {"st"}, "resource": {e.Server.URL + "/mcp"},
	}
	post := func(step string, extra url.Values) *http.Response {
		t.Helper()
		form := url.Values{"step": {step}}
		for k, v := range authz {
			form[k] = v
		}
		for k, v := range extra {
			form[k] = v
		}
		r, err := noFollow.PostForm(e.Server.URL+"/oauth/authorize", form)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	post("email", url.Values{"email": {email}}).Body.Close()
	r := post("code", url.Values{"email": {email}, "code": {code}})
	r.Body.Close()
	loc, err := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != http.StatusFound || err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("authorize did not return a code: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	tok, err := http.PostForm(e.Server.URL+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "code_verifier": {verifier},
		"client_id": {client.ClientID}, "redirect_uri": {"http://127.0.0.1:53111/callback"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Body.Close()
	var out Tokens
	_ = json.NewDecoder(tok.Body).Decode(&out)
	if out.AccessToken == "" || !strings.Contains(out.Scope, "ledger:") {
		t.Fatalf("token: %+v", out)
	}
	return out
}
