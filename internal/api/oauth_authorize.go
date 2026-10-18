package api

// /oauth/authorize: who is asking, who is signing in, and where the answer
// goes. Three steps on one page, each a fresh check of the whole request —
// nothing is trusted from an earlier step except what the person typed:
//
//	GET              the client and its redirect are checked, the consent is shown, the email asked
//	POST step=email  a six-digit code goes to that address
//	POST step=code   the code is checked, the store found or opened, and an
//	                 authorization code sent back to the client's redirect URI
//
// The consent names the host of the client's metadata URL and the host the
// person is sent back to, never the name a client gives itself.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type authzReq struct {
	ResponseType, ClientID, RedirectURI, CodeChallenge, ChallengeMethod, Scope, State, Resource string
}

func authzFrom(v url.Values) authzReq {
	return authzReq{
		ResponseType: v.Get("response_type"), ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"),
		CodeChallenge: v.Get("code_challenge"), ChallengeMethod: v.Get("code_challenge_method"),
		Scope: v.Get("scope"), State: v.Get("state"), Resource: v.Get("resource"),
	}
}

func (a authzReq) hidden() map[string]string {
	return map[string]string{
		"response_type": a.ResponseType, "client_id": a.ClientID, "redirect_uri": a.RedirectURI,
		"code_challenge": a.CodeChallenge, "code_challenge_method": a.ChallengeMethod,
		"scope": a.Scope, "state": a.State, "resource": a.Resource,
	}
}

// clientView is what the consent shows about the request.
type clientView struct {
	ClientHost   string // host of the CIMD URL; empty for a registered client
	ClientName   string // self-declared, shown only for a registered client and labelled so
	RedirectHost string
	Loopback     bool
	Scope        string
	CanWrite     bool
}

// pageErr is a request this server must not redirect: the client or its
// redirect URI is not established, so sending the browser anywhere would be
// an open redirect.
type pageErr struct{ msg string }

func (e *pageErr) Error() string { return e.msg }

// redirectErr is sent back to the client's redirect URI (RFC 6749 §4.1.2.1).
type redirectErr struct{ code, desc string }

func (e *redirectErr) Error() string { return e.code + ": " + e.desc }

// checkAuthz validates the whole request.
func (s *Server) checkAuthz(ctx context.Context, a authzReq) (clientView, string, error) {
	var view clientView
	if a.ClientID == "" || a.RedirectURI == "" {
		return view, "", &pageErr{"client_id and redirect_uri are required"}
	}
	var registered []string
	if isCIMD(a.ClientID) {
		doc, err := s.OAuth.fetchCIMD(ctx, a.ClientID)
		if err != nil {
			return view, "", &pageErr{"the client could not be identified: " + err.Error()}
		}
		u, _ := url.Parse(a.ClientID)
		view.ClientHost = u.Host
		registered = doc.RedirectURIs
	} else {
		uris, name, err := s.registeredClient(ctx, a.ClientID)
		if errors.Is(err, pgx.ErrNoRows) {
			return view, "", &pageErr{"this client is not registered here, or has gone unused too long"}
		}
		if err != nil {
			return view, "", err
		}
		view.ClientName = name
		registered = uris
	}
	if !redirectMatches(registered, a.RedirectURI) {
		return view, "", &pageErr{"the redirect URI is not one this client registered"}
	}
	ru, _ := url.Parse(a.RedirectURI)
	view.RedirectHost = ru.Host
	view.Loopback = isLoopbackHost(ru.Hostname())

	if a.ResponseType != "code" {
		return view, "", &redirectErr{"unsupported_response_type", "response_type must be code"}
	}
	if a.CodeChallenge == "" || a.ChallengeMethod != "S256" {
		return view, "", &redirectErr{"invalid_request", "PKCE with code_challenge_method=S256 is required"}
	}
	scope, err := parseScope(a.Scope)
	if err != nil {
		return view, "", &redirectErr{"invalid_scope", err.Error()}
	}
	if a.Resource != "" && a.Resource != s.OAuth.Resource {
		return view, "", &redirectErr{"invalid_target", "resource must be " + s.OAuth.Resource}
	}
	view.Scope = scope
	view.CanWrite = strings.Contains(scope, scopeLedgerWrite)
	return view, scope, nil
}

func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, a authzReq, params url.Values) {
	u, _ := url.Parse(a.RedirectURI)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if a.State != "" {
		q.Set("state", a.State)
	}
	q.Set("iss", s.OAuth.Issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// answerAuthzErr shows a page error or redirects a redirect error.
func (s *Server) answerAuthzErr(w http.ResponseWriter, r *http.Request, a authzReq, err error) {
	var re *redirectErr
	if errors.As(err, &re) {
		s.redirectBack(w, r, a, url.Values{"error": {re.code}, "error_description": {re.desc}})
		return
	}
	var pe *pageErr
	msg := "Something went wrong on this side. Try again in a moment."
	if errors.As(err, &pe) {
		msg = pe.msg
	} else {
		log.Printf("oauth: authorize: %v", err)
	}
	s.renderAuthz(w, http.StatusBadRequest, authzPage{Fatal: msg})
}

// GET /oauth/authorize
func (s *Server) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.oauthOn() {
		http.NotFound(w, r)
		return
	}
	a := authzFrom(r.URL.Query())
	view, _, err := s.checkAuthz(r.Context(), a)
	if err != nil {
		s.answerAuthzErr(w, r, a, err)
		return
	}
	s.renderAuthz(w, http.StatusOK, authzPage{Step: "email", View: view, Hidden: a.hidden()})
}

// POST /oauth/authorize
func (s *Server) oauthAuthorizePost(w http.ResponseWriter, r *http.Request) {
	if !s.oauthOn() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderAuthz(w, http.StatusBadRequest, authzPage{Fatal: "the form could not be read"})
		return
	}
	a := authzFrom(r.PostForm)
	view, scope, err := s.checkAuthz(r.Context(), a)
	if err != nil {
		s.answerAuthzErr(w, r, a, err)
		return
	}
	page := authzPage{View: view, Hidden: a.hidden()}
	email, err := cleanEmail(r.PostForm.Get("email"))
	if err != nil {
		page.Step, page.Error = "email", err.Error()
		s.renderAuthz(w, http.StatusOK, page)
		return
	}
	page.Email = email

	switch r.PostForm.Get("step") {
	case "email":
		page.Step = "code"
		if msg := s.sendOTP(r, email, a.ClientID, &page); msg != "" {
			page.Step, page.Error = "email", msg
		}
		s.renderAuthz(w, http.StatusOK, page)
	case "code":
		code := strings.Join(strings.Fields(r.PostForm.Get("code")), "")
		if msg := s.checkOTP(r.Context(), email, a.ClientID, code); msg != "" {
			page.Step, page.Error = "code", msg
			s.renderAuthz(w, http.StatusOK, page)
			return
		}
		store, msg := s.signInStore(r, email)
		if msg != "" {
			page.Step, page.Error = "email", msg
			s.renderAuthz(w, http.StatusOK, page)
			return
		}
		code, err := randomToken(32)
		if err != nil {
			s.answerAuthzErr(w, r, a, err)
			return
		}
		resource := a.Resource
		if resource == "" {
			resource = s.OAuth.Resource
		}
		if _, err := s.Repo.Pool.Exec(r.Context(), `
			INSERT INTO oauth_code (code_hash, client_id, redirect_uri, code_challenge, scope, resource, sub, tenant_id, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			hashOf(code), a.ClientID, a.RedirectURI, a.CodeChallenge, scope, resource, email, store,
			s.OAuth.now().Add(codeTTL)); err != nil {
			s.answerAuthzErr(w, r, a, err)
			return
		}
		s.redirectBack(w, r, a, url.Values{"code": {code}})
	default:
		page.Step = "email"
		s.renderAuthz(w, http.StatusOK, page)
	}
}

func (s *Server) isReviewer(email string) bool {
	return s.OAuth.ReviewEmail != "" && s.OAuth.ReviewCode != "" &&
		strings.EqualFold(email, strings.TrimSpace(s.OAuth.ReviewEmail))
}

// sendOTP mails a fresh code, or explains why it cannot. "" = sent.
func (s *Server) sendOTP(r *http.Request, email, clientID string, page *authzPage) string {
	if s.isReviewer(email) {
		// The review account's code is fixed and known to the reviewer; nothing
		// is mailed, and nothing on the page says this address is special.
		return ""
	}
	if !s.OAuth.limit("otp:email:"+email, 3, otpTTL) || !s.OAuth.limit("otp:ip:"+clientAddr(r), 10, time.Hour) {
		return "Several codes have gone out recently. Use the last one, or try again in a few minutes."
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "A code could not be made. Try again."
	}
	code := fmt.Sprintf("%06d", n.Int64())
	if _, err := s.Repo.Pool.Exec(r.Context(), `
		INSERT INTO oauth_otp (email, code_hash, client_id, ip, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		email, hashOf(email, code), clientID, clientAddr(r), s.OAuth.now().Add(otpTTL)); err != nil {
		log.Printf("oauth: store otp: %v", err)
		return "A code could not be made. Try again."
	}
	// Testing only, the same switch that shows door links on the page — and
	// never for an address that already owns a store: shown on the page, the
	// code would let anyone who types that address into its store. Such an
	// address gets its code by mail, or not at all.
	show := false
	if s.Demo.ShowLink {
		if existing, err := s.trialOf(r.Context(), email); err != nil || existing.Tenant == "" {
			show = true
			log.Printf("oauth: TK_DEMO_SHOW_LINK=1 — the sign-in code for new address %s is shown on the page; testing only", email)
			page.ShownCode = code
		}
	}
	if s.Demo.Mail == nil {
		if show {
			return ""
		}
		return "This server cannot send mail yet, so it cannot sign this address in. Get in touch."
	}
	subject, text, htmlBody := otpMail(code, s.Demo.Contact, s.Demo.Site)
	if hm, ok := s.Demo.Mail.(htmlMailer); ok {
		err = hm.SendHTML(r.Context(), email, subject, text, htmlBody)
	} else {
		err = s.Demo.Mail.Send(r.Context(), email, subject, text)
	}
	if err != nil {
		log.Printf("oauth: mail to %s failed: %v", email, err)
		if show {
			return ""
		}
		return "The code could not be sent. Check the address and try again."
	}
	return ""
}

// otpMail is the sign-in code message: the code in the subject, so it can be
// read without opening the mail; a line saying why it came; and the same in a
// small HTML part. A message that is nothing but a number reads as spam to
// some filters (iCloud junked one on 3/10/2026 with DKIM, SPF and DMARC all
// passing).
func otpMail(code, contact, site string) (subject, text, page string) {
	subject = code + " is your T-knowledge sign-in code"
	reply := "Questions: reply to this message."
	if contact != "" {
		reply = "Questions: reply to this message, or write to " + contact + "."
	}
	text = strings.Join([]string{
		"Your sign-in code for T-knowledge:",
		"",
		"    " + code,
		"",
		"Someone — most likely you — is signing in to T-knowledge to connect an assistant",
		"such as Claude to their store. The code works once, for ten minutes.",
		"",
		"If you did not ask for it, ignore this message: nobody can sign in without it.",
		reply,
	}, "\n") + signoff(site)
	c := html.EscapeString(code)
	siteHTML := ""
	if site != "" {
		e := html.EscapeString(site)
		siteHTML = `<p style="margin:16px 0 0;font-size:13px"><a href="` + e + `" style="color:#5b6470">` + e + `</a></p>`
	}
	contactHTML := ""
	if contact != "" {
		e := html.EscapeString(contact)
		contactHTML = `, or write to <a href="mailto:` + e + `">` + e + `</a>`
	}
	page = `<!doctype html><html><body style="margin:0;padding:24px;background:#f4f6f8;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;color:#15181c">` +
		`<div style="max-width:480px;margin:0 auto;background:#fff;border:1px solid #dbe0e6;border-radius:10px;padding:24px">` +
		`<p style="margin:0 0 12px">Your sign-in code for <b>T-knowledge</b>:</p>` +
		`<p style="margin:0 0 16px;font:600 28px/1.2 ui-monospace,Menlo,monospace;letter-spacing:4px">` + c + `</p>` +
		`<p style="margin:0 0 12px;color:#5b6470;font-size:14px">Someone — most likely you — is signing in to T-knowledge to connect an assistant such as Claude to their store. The code works once, for ten minutes.</p>` +
		`<p style="margin:0 0 12px;color:#5b6470;font-size:14px">If you did not ask for it, ignore this message: nobody can sign in without it. Questions: reply to this message` + contactHTML + `.</p>` +
		siteHTML +
		`</div></body></html>`
	return subject, text, page
}

// checkOTP verifies the code typed for an address. "" = good.
func (s *Server) checkOTP(ctx context.Context, email, clientID, code string) string {
	if code == "" {
		return "Enter the six-digit code from the email."
	}
	if s.isReviewer(email) {
		if subtle.ConstantTimeCompare([]byte(code), []byte(strings.TrimSpace(s.OAuth.ReviewCode))) == 1 {
			return ""
		}
		return "That code is not right. Check it and try again."
	}
	var id int64
	var hash string
	var attempts int
	err := s.Repo.Pool.QueryRow(ctx, `
		SELECT id, code_hash, attempts FROM oauth_otp
		WHERE email = $1 AND client_id = $2 AND used_at IS NULL AND expires_at > now()
		ORDER BY id DESC LIMIT 1`, email, clientID).Scan(&id, &hash, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return "No code is waiting for this address. Ask for a new one."
	}
	if err != nil {
		log.Printf("oauth: read otp: %v", err)
		return "The code could not be checked. Try again."
	}
	if attempts >= otpTries {
		return "Too many tries with this code. Ask for a new one."
	}
	if subtle.ConstantTimeCompare([]byte(hashOf(email, code)), []byte(hash)) != 1 {
		_, _ = s.Repo.Pool.Exec(ctx, `UPDATE oauth_otp SET attempts = attempts + 1 WHERE id = $1`, id)
		return "That code is not right. Check it and try again."
	}
	tag, err := s.Repo.Pool.Exec(ctx, `UPDATE oauth_otp SET used_at = now() WHERE id = $1 AND used_at IS NULL`, id)
	if err != nil || tag.RowsAffected() != 1 {
		return "That code has already been used. Ask for a new one."
	}
	return ""
}

// signInStore is the store a signed-in address works in. A new store is
// opened on the door's terms, under the door's per-address limit.
func (s *Server) signInStore(r *http.Request, email string) (string, string) {
	if existing, err := s.trialOf(r.Context(), email); err == nil && existing.Tenant != "" {
		return existing.Tenant, ""
	}
	if !s.Demo.allow(clientAddr(r)) {
		return "", "Several stores have been opened from here in the last hour. Try again later."
	}
	opened, err := s.openStore(r.Context(), email, nil)
	if err != nil {
		log.Printf("oauth: open store for %s: %v", email, err)
		return "", "Your store could not be opened. Try again in a moment."
	}
	return opened.Tenant, ""
}

// --- page -----------------------------------------------------------------------

type authzPage struct {
	Step      string // email | code
	View      clientView
	Hidden    map[string]string
	Email     string
	Error     string
	Fatal     string
	ShownCode string
}

func (s *Server) renderAuthz(w http.ResponseWriter, status int, p authzPage) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action "+formAction(p)+"; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := authzTmpl.Execute(w, p); err != nil {
		log.Printf("oauth: render: %v", err)
	}
}

// formAction is the CSP form-action list for an authorize page. The code step
// answers the form with a 302 to the client's redirect URI, and browsers apply
// form-action to that redirect too: with 'self' alone, Safari stays on the page
// and the client never receives its code (seen 3/10/2026 with Claude). So the
// redirect URI's origin is allowed — only on a page whose request already
// passed checkAuthz, which is the only kind that carries Hidden.
func formAction(p authzPage) string {
	if p.Fatal != "" || p.Hidden == nil {
		return "'self'"
	}
	u, err := url.Parse(p.Hidden["redirect_uri"])
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "'self'"
	}
	return "'self' " + u.Scheme + "://" + u.Host
}

var authzTmpl = template.Must(template.New("authz").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in — T-knowledge</title>
<style>
 body{font:16px/1.5 system-ui,sans-serif;background:#f7f5f0;color:#1d1b16;margin:0;padding:24px 16px}
 main{max-width:440px;margin:40px auto;background:#fff;border:1px solid #e3ded3;border-radius:10px;padding:28px}
 h1{font-size:1.25rem;margin:0 0 12px} .muted{color:#6b665c;font-size:.9rem}
 .err{color:#a1281b} .warn{background:#fff6dc;border:1px solid #ecd9a0;padding:8px 10px;border-radius:6px;font-size:.9rem}
 input{width:100%;box-sizing:border-box;font:inherit;padding:10px;border:1px solid #cfc8ba;border-radius:6px;margin:6px 0 14px}
 button{font:inherit;font-weight:600;padding:10px 16px;border:0;border-radius:6px;background:#1d1b16;color:#fff;cursor:pointer}
 ul{padding-left:20px} code{font-size:1.1rem;letter-spacing:.15em}
</style></head><body><main>
{{if .Fatal}}
 <h1>This sign-in cannot continue</h1>
 <p class="err">{{.Fatal}}</p>
 <p class="muted">Close this window and try connecting again from the app.</p>
{{else}}
 <h1>Sign in to T-knowledge</h1>
 <p>{{if .View.ClientHost}}<strong>{{.View.ClientHost}}</strong>{{else}}A client registered with this server{{if .View.ClientName}} that calls itself “{{.View.ClientName}}”{{end}}{{end}}
  wants to use your T-knowledge store. It will be able to:</p>
 <ul><li>read your store</li>{{if .View.CanWrite}}<li>write to it — every write signed with your email</li>{{end}}</ul>
 <p class="muted">After you sign in you go back to <strong>{{.View.RedirectHost}}</strong>.</p>
 {{if .View.Loopback}}<p class="warn">That is a program on your own computer, such as Claude Code. Continue only if you started this sign-in yourself.</p>{{end}}
 {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
 <form method="post" action="/oauth/authorize">
  {{range $k, $v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
  {{if eq .Step "code"}}
   <input type="hidden" name="step" value="code">
   <input type="hidden" name="email" value="{{.Email}}">
   <p>A six-digit code has gone to <strong>{{.Email}}</strong>.</p>
   {{if .ShownCode}}<p class="warn">Testing build: the code is <code>{{.ShownCode}}</code></p>{{end}}
   <label for="code">Code</label>
   <input id="code" name="code" inputmode="numeric" autocomplete="one-time-code" autofocus required>
   <button type="submit">Allow and sign in</button>
  {{else}}
   <input type="hidden" name="step" value="email">
   <label for="email">Your email</label>
   <input id="email" name="email" type="email" value="{{.Email}}" autocomplete="email" autofocus required>
   <button type="submit">Send me a code</button>
  {{end}}
 </form>
 <p class="muted" style="margin-top:18px">Your store holds what you write in it, signed with your email. No password: each sign-in sends a fresh code.</p>
{{end}}
</main></body></html>`))
