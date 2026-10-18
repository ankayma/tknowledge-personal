package api

// The authorization server behind the standard door (brief 2/10, "three doors"):
// Claude — web, Desktop, mobile, Cowork, Code — and any MCP client that follows
// the authorization spec find it from /mcp's 401, sign the person in with a
// one-time code sent by email, and get an access token.
//
// The access token is a tk grant, signed with this server's key, living one
// hour, marked via=oauth. It reaches the store through the same verification
// as every other grant, so the header door accepts it unchanged and nothing
// downstream learns a second kind of credential. Refresh tokens are opaque,
// stored hashed, rotate on every use, and a rotated one presented again
// revokes its whole family.
//
// Scope names follow the brief (ledger:read, ledger:write, ledger:audit) on the
// wire and map onto the grant scopes tk already has (tk:read, tk:write). A
// person signing in gets the store their email already owns, or a new one on
// the door's terms — the same store either way.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ankayma/tknowledge-personal/internal/grant"
)

const (
	scopeLedgerRead   = "ledger:read"
	scopeLedgerWrite  = "ledger:write"
	scopeLedgerAudit  = "ledger:audit"
	scopeOffline      = "offline_access"
	defaultOAuthScope = scopeLedgerRead + " " + scopeLedgerWrite

	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
	codeTTL    = 60 * time.Second
	otpTTL     = 10 * time.Minute
	otpTries   = 5
	dcrIdle    = 90 * 24 * time.Hour
)

// OAuth is the authorization server's configuration. nil on Server = no
// authorization server: discovery and /oauth/* answer 404, and only the header
// and URL doors exist (TK_LOGIN_MODE=off, a sovereign deployment).
type OAuth struct {
	// Issuer is the public origin, e.g. https://tk.example.org.
	Issuer string
	// Resource is the MCP URL exactly as people paste it into Claude.
	Resource string
	// Key signs access tokens. Its public half must be among the keys tk-api
	// verifies grants with; main adds it.
	Key ed25519.PrivateKey
	// Aud is the grant audience this deployment verifies.
	Aud string
	// ReviewEmail and ReviewCode let the directory's reviewers sign in without
	// a mailbox: that one address, that one fixed code, nothing else. Both come
	// from the environment at deploy time; either empty = no review account.
	ReviewEmail string
	ReviewCode  string
	// NoCIMD stops advertising client ID metadata documents, so Claude registers
	// through DCR instead. For a host whose outbound fetch of the client's
	// metadata URL is refused — the hosted server got a Cloudflare challenge from claude.ai
	// (2/10/2026) — CIMD can never succeed, and advertising it breaks sign-in.
	NoCIMD bool
	// HTTP fetches client metadata documents. nil = the SSRF-guarded client.
	HTTP *http.Client
	// Now is the clock; nil = time.Now.
	Now func() time.Time

	mu    sync.Mutex
	cimd  map[string]cimdEntry
	sends map[string][]time.Time
}

func (o *OAuth) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// limit allows n events per window for key. Keys that have gone quiet are
// dropped so the map does not grow without bound.
func (o *OAuth) limit(key string, n int, window time.Duration) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sends == nil {
		o.sends = map[string][]time.Time{}
	}
	cutoff := o.now().Add(-window)
	kept := o.sends[key][:0]
	for _, t := range o.sends[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(o.sends) > 10000 {
		for k, ts := range o.sends {
			if len(ts) == 0 || ts[len(ts)-1].Before(o.now().Add(-24*time.Hour)) {
				delete(o.sends, k)
			}
		}
	}
	if len(kept) >= n {
		o.sends[key] = kept
		return false
	}
	o.sends[key] = append(kept, o.now())
	return true
}

func (s *Server) oauthOn() bool { return s.OAuth != nil && s.OAuth.Key != nil && s.Demo.on() }

// --- discovery (RFC 9728, RFC 8414) ------------------------------------------

func (s *Server) oauthProtectedResource(w http.ResponseWriter, r *http.Request) {
	if !s.oauthOn() {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.OAuth.Resource,
		"authorization_servers":    []string{s.OAuth.Issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{scopeLedgerRead, scopeLedgerWrite, scopeLedgerAudit},
		"resource_name":            "T-knowledge",
	})
}

func (s *Server) oauthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.oauthOn() {
		http.NotFound(w, r)
		return
	}
	iss := s.OAuth.Issuer
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         iss,
		"authorization_endpoint":                         iss + "/oauth/authorize",
		"token_endpoint":                                 iss + "/oauth/token",
		"registration_endpoint":                          iss + "/oauth/register",
		"scopes_supported":                               []string{scopeLedgerRead, scopeLedgerWrite, scopeLedgerAudit, scopeOffline},
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"client_id_metadata_document_supported":          !s.OAuth.NoCIMD,
		"authorization_response_iss_parameter_supported": true,
	})
}

// --- scopes -------------------------------------------------------------------

// parseScope keeps the requested scopes this server knows, in a fixed order.
// An empty request means read and write. An unknown scope is an error the
// client must hear (invalid_scope), not something to drop silently.
func parseScope(raw string) (string, error) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return defaultOAuthScope, nil
	}
	known := []string{scopeLedgerRead, scopeLedgerWrite, scopeLedgerAudit, scopeOffline}
	for _, f := range fields {
		if !slices.Contains(known, f) {
			return "", fmt.Errorf("scope %q is not one this server grants", f)
		}
	}
	out := []string{}
	for _, k := range known {
		if slices.Contains(fields, k) {
			out = append(out, k)
		}
	}
	return strings.Join(out, " "), nil
}

// grantScopes maps the wire scopes onto the grant scopes tk-api checks. Every
// store reached through sign-in is a door store, so it carries tk:demo — the
// same tool list the door's own link shows. ledger:audit has no grant scope of
// its own until the auditor role exists (spec T–D, phase 2); it reads.
func grantScopes(scope string) []string {
	f := strings.Fields(scope)
	out := []string{grant.ScopeDemo}
	if slices.Contains(f, scopeLedgerRead) || slices.Contains(f, scopeLedgerWrite) || slices.Contains(f, scopeLedgerAudit) {
		out = append(out, grant.ScopeRead)
	}
	if slices.Contains(f, scopeLedgerWrite) {
		out = append(out, grant.ScopeWrite)
	}
	return out
}

// --- tokens -------------------------------------------------------------------

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashOf(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

type tokenErr struct {
	status int
	code   string
	desc   string
}

func (e *tokenErr) Error() string { return e.code + ": " + e.desc }

func writeTokenError(w http.ResponseWriter, err error) {
	var te *tokenErr
	if !errors.As(err, &te) {
		te = &tokenErr{http.StatusInternalServerError, "server_error", err.Error()}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, te.status, map[string]string{"error": te.code, "error_description": te.desc})
}

func invalidGrant(desc string) error { return &tokenErr{http.StatusBadRequest, "invalid_grant", desc} }

// POST /oauth/token — application/x-www-form-urlencoded (RFC 6749 §4.1.3).
func (s *Server) oauthToken(w http.ResponseWriter, r *http.Request) {
	if !s.oauthOn() {
		http.NotFound(w, r)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		writeTokenError(w, &tokenErr{http.StatusBadRequest, "invalid_request",
			"the token endpoint takes application/x-www-form-urlencoded"})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeTokenError(w, &tokenErr{http.StatusBadRequest, "invalid_request", err.Error()})
		return
	}
	var (
		out map[string]any
		err error
	)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		out, err = s.exchangeCode(r.Context(), r)
	case "refresh_token":
		out, err = s.refresh(r.Context(), r)
	default:
		err = &tokenErr{http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token"}
	}
	if err != nil {
		writeTokenError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) exchangeCode(ctx context.Context, r *http.Request) (map[string]any, error) {
	f := r.PostForm
	code, verifier, clientID := f.Get("code"), f.Get("code_verifier"), f.Get("client_id")
	if code == "" || verifier == "" || clientID == "" {
		return nil, &tokenErr{http.StatusBadRequest, "invalid_request", "code, code_verifier and client_id are required"}
	}
	var c struct {
		ClientID, RedirectURI, Challenge, Scope, Resource, Sub, Tenant string
	}
	// Single use, atomically: two exchanges of one code cannot both win.
	err := s.Repo.Pool.QueryRow(ctx, `
		UPDATE oauth_code SET used_at = now()
		WHERE code_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING client_id, redirect_uri, code_challenge, scope, resource, sub, tenant_id`,
		hashOf(code)).Scan(&c.ClientID, &c.RedirectURI, &c.Challenge, &c.Scope, &c.Resource, &c.Sub, &c.Tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalidGrant("the code is unknown, used, or expired")
	}
	if err != nil {
		return nil, err
	}
	if clientID != c.ClientID {
		return nil, invalidGrant("the code was issued to another client")
	}
	if ru := f.Get("redirect_uri"); ru != "" && ru != c.RedirectURI {
		return nil, invalidGrant("redirect_uri does not match the authorization request")
	}
	if res := f.Get("resource"); res != "" && res != s.OAuth.Resource {
		return nil, &tokenErr{http.StatusBadRequest, "invalid_target", "resource must be " + s.OAuth.Resource}
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != c.Challenge {
		return nil, invalidGrant("code_verifier does not match the code_challenge")
	}
	family, err := randomToken(12)
	if err != nil {
		return nil, err
	}
	return s.issueTokens(ctx, c.Sub, c.Tenant, c.Scope, c.ClientID, family)
}

func (s *Server) refresh(ctx context.Context, r *http.Request) (map[string]any, error) {
	f := r.PostForm
	token, clientID := f.Get("refresh_token"), f.Get("client_id")
	if token == "" {
		return nil, &tokenErr{http.StatusBadRequest, "invalid_request", "refresh_token is required"}
	}
	h := hashOf(token)
	var rt struct {
		Family, ClientID, Sub, Tenant, Scope string
		Expires                              time.Time
		Used, Revoked                        *time.Time
	}
	err := s.Repo.Pool.QueryRow(ctx, `
		SELECT family_id, client_id, sub, tenant_id, scope, expires_at, used_at, revoked_at
		FROM oauth_refresh WHERE token_hash = $1`, h).
		Scan(&rt.Family, &rt.ClientID, &rt.Sub, &rt.Tenant, &rt.Scope, &rt.Expires, &rt.Used, &rt.Revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalidGrant("unknown refresh token")
	}
	if err != nil {
		return nil, err
	}
	revokeFamily := func() {
		_, _ = s.Repo.Pool.Exec(ctx,
			`UPDATE oauth_refresh SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, rt.Family)
	}
	switch {
	case rt.Revoked != nil:
		return nil, invalidGrant("refresh token revoked")
	case rt.Used != nil:
		// A rotated token presented again: someone else may hold it. Kill the
		// whole family so neither copy keeps working (OAuth 2.1 §4.3.1).
		revokeFamily()
		return nil, invalidGrant("refresh token already used; sign in again")
	case !rt.Expires.After(s.OAuth.now()):
		return nil, invalidGrant("refresh token expired")
	case clientID != "" && clientID != rt.ClientID:
		return nil, invalidGrant("refresh token was issued to another client")
	}
	scope := rt.Scope
	if req := strings.TrimSpace(f.Get("scope")); req != "" {
		want, err := parseScope(req)
		if err != nil {
			return nil, &tokenErr{http.StatusBadRequest, "invalid_scope", err.Error()}
		}
		for _, sc := range strings.Fields(want) {
			if !slices.Contains(strings.Fields(rt.Scope), sc) {
				return nil, &tokenErr{http.StatusBadRequest, "invalid_scope", "cannot widen scope on refresh"}
			}
		}
		scope = want
	}
	tag, err := s.Repo.Pool.Exec(ctx, `
		UPDATE oauth_refresh SET used_at = now()
		WHERE token_hash = $1 AND used_at IS NULL AND revoked_at IS NULL`, h)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		revokeFamily()
		return nil, invalidGrant("refresh token already used; sign in again")
	}
	return s.issueTokens(ctx, rt.Sub, rt.Tenant, scope, rt.ClientID, rt.Family)
}

// issueTokens signs the access grant and stores a fresh refresh token in the
// family.
func (s *Server) issueTokens(ctx context.Context, sub, tenant, scope, clientID, family string) (map[string]any, error) {
	exp := s.OAuth.now().Add(accessTTL)
	access, err := grant.Sign(s.OAuth.Key, grant.Grant{
		UserID: sub, ActorID: s.clientProducer(ctx, clientID, sub), NodeID: "tknowledge", Aud: s.OAuth.Aud,
		TenantID: tenant, ExpiresAt: exp.Unix(), Scope: grantScopes(scope), Via: grant.ViaOAuth,
	})
	if err != nil {
		return nil, err
	}
	refresh, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	if _, err := s.Repo.Pool.Exec(ctx, `
		INSERT INTO oauth_refresh (token_hash, family_id, client_id, sub, tenant_id, scope, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		hashOf(refresh), family, clientID, sub, tenant, scope, s.OAuth.now().Add(refreshTTL)); err != nil {
		return nil, err
	}
	return map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	}, nil
}

// clientProducer names the AI behind an OAuth client — the producer on every
// row written through its grant — from what the client says of itself: the
// host of its metadata document (CIMD) or the name it registered with (DCR).
// It is self-declared, a label and not proof. A client no known name matches
// keeps its own name as a slug; one without a name is "agent".
func (s *Server) clientProducer(ctx context.Context, clientID, sub string) string {
	label := ""
	if isCIMD(clientID) {
		if u, err := url.Parse(clientID); err == nil {
			label = u.Hostname()
		}
	} else {
		_ = s.Repo.Pool.QueryRow(ctx, `SELECT coalesce(client_name, '') FROM oauth_client WHERE client_id = $1`,
			clientID).Scan(&label)
	}
	p := producerOf(label)
	if p == sub || p == producerOf(sub) || p == "owner" {
		// A client cannot pass itself off as the person.
		return "agent"
	}
	return p
}

var knownProducers = []struct{ word, producer string }{
	{"claude", "claude"}, {"anthropic", "claude"},
	{"chatgpt", "chatgpt"}, {"openai", "chatgpt"},
	{"gemini", "gemini"}, {"google", "gemini"},
	{"cursor", "cursor"},
}

func producerOf(label string) string {
	l := strings.ToLower(label)
	for _, k := range knownProducers {
		if strings.Contains(l, k.word) {
			return k.producer
		}
	}
	var b strings.Builder
	dash := false
	for _, r := range l {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	if slug := strings.Trim(b.String(), "-"); slug != "" {
		return slug
	}
	return "agent"
}

// --- dynamic client registration (RFC 7591) -----------------------------------

// POST /oauth/register — JSON. For MCP clients that do not identify themselves
// with a metadata document; Claude uses CIMD and never comes here unless that
// path is unavailable. A client that goes 90 days unused is no longer honoured.
func (s *Server) oauthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.oauthOn() {
		http.NotFound(w, r)
		return
	}
	var in struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := decodeJSONBody(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata", "error_description": err.Error()})
		return
	}
	if len(in.RedirectURIs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri", "error_description": "redirect_uris is required"})
		return
	}
	for _, u := range in.RedirectURIs {
		if err := checkRegisteredRedirect(u); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri", "error_description": err.Error()})
			return
		}
	}
	if m := in.TokenEndpointAuthMethod; m != "" && m != "none" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata",
			"error_description": "this server registers public clients only: token_endpoint_auth_method none, with PKCE"})
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata",
				"error_description": "grant_types: authorization_code and refresh_token only"})
			return
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if len(name) > 100 {
		name = name[:100]
	}
	id, err := randomToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	clientID := "tkc_" + id
	if _, err := s.Repo.Pool.Exec(r.Context(),
		`INSERT INTO oauth_client (client_id, client_name, redirect_uris) VALUES ($1, $2, $3)`,
		clientID, name, in.RedirectURIs); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        s.OAuth.now().Unix(),
		"client_name":                name,
		"redirect_uris":              in.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// registeredClient reads a DCR client still in use, and marks it used.
func (s *Server) registeredClient(ctx context.Context, clientID string) (redirects []string, name string, err error) {
	err = s.Repo.Pool.QueryRow(ctx, `
		UPDATE oauth_client SET last_used_at = now()
		WHERE client_id = $1 AND last_used_at > now() - $2::interval
		RETURNING redirect_uris, coalesce(client_name, '')`,
		clientID, fmt.Sprintf("%d seconds", int(dcrIdle.Seconds()))).Scan(&redirects, &name)
	return redirects, name, err
}
