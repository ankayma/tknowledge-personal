package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ankayma/tknowledge-personal/internal/grant"
)

// Three doors into one server, one handler (brief 2/10, "three doors"):
//
//	/mcp + Authorization: Bearer <access token from tk's OAuth>  entry oauth   (Claude, any spec client)
//	/mcp + Authorization: Bearer <grant>                          entry header  (Cursor, scripts, mcp-remote)
//	/mcp/t/<grant>                                                entry url     (Ankayma privUrl, a private link)
//
// The first two differ only in where the grant came from (an access token is a
// grant marked via=oauth). The third is never advertised: no discovery points
// at it and it never answers 401 with WWW-Authenticate — a grant there that
// does not verify gets a bare 404, so a private URL that has expired or been
// revoked simply stops existing. The MCP authorization spec forbids tokens in
// URLs for spec clients; this door is for things that are not MCP clients (a
// private network URL, a shared link), and spec clients are never led to it.
//
// Every tool gets the same context whichever door it came through: the grant,
// and the entry it used — so an answer that carries a link carries one the
// caller can open (LinkFor), and a grant from the URL door never shows up in
// an answer through another door.

type tokenKey struct{}
type doorKey struct{}

// Entry kinds.
const (
	EntryOAuth  = "oauth"
	EntryHeader = "header"
	EntryURL    = "url"
)

type door struct{ entry, base string }

// ContextWithToken attaches the caller's grant to ctx.
func ContextWithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey{}, token)
}

// TokenFromContext returns the caller's grant, or "".
func TokenFromContext(ctx context.Context) string {
	tok, _ := ctx.Value(tokenKey{}).(string)
	return tok
}

func contextWithDoor(ctx context.Context, entry, base string) context.Context {
	return context.WithValue(ctx, doorKey{}, door{entry: entry, base: base})
}

// EntryFromContext is the door the call came through: oauth, header, url, or
// "" (stdio, or no grant at all).
func EntryFromContext(ctx context.Context) string {
	d, _ := ctx.Value(doorKey{}).(door)
	return d.entry
}

// LinkFor is the one place a URL to something in the store is made, in the
// form the caller's door can open: through the URL door, the caller's own
// private link; through any other door, the plain MCP URL — never a grant. kind
// names what is linked (thread, entry, document) for when the store has pages
// of its own; for now the id rides in the fragment. "" when there is no public
// address to link to (stdio).
func LinkFor(ctx context.Context, kind, id string) string {
	d, _ := ctx.Value(doorKey{}).(door)
	if d.base == "" || id == "" {
		return ""
	}
	_ = kind
	if d.entry == EntryURL {
		if tok := TokenFromContext(ctx); tok != "" {
			return d.base + tokenPathPrefix + tok + "#" + id
		}
	}
	return d.base + "/mcp#" + id
}

const tokenPathPrefix = "/mcp/t/"

func bearer(r *http.Request) string {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	return ""
}

func tokenFromRequest(r *http.Request) string {
	if tok, ok := strings.CutPrefix(r.URL.Path, tokenPathPrefix); ok {
		return strings.Trim(tok, "/")
	}
	return bearer(r)
}

// HTTPOption configures HTTPHandler.
type HTTPOption func(*doors)

// WithGate turns on the door checks: grants are verified against tk-api
// (GET /v1/whoami, so revocation counts), /mcp answers 401/403 before the MCP
// SDK runs, and /mcp/t/ answers 404 for a grant that does not verify.
// publicURL is the address people reach tk-mcp on (TK_PUBLIC_URL); "" derives
// it from the request.
func WithGate(apiURL, publicURL string) HTTPOption {
	return func(d *doors) {
		d.apiURL = strings.TrimRight(apiURL, "/")
		d.publicURL = strings.TrimRight(publicURL, "/")
	}
}

type doors struct {
	s         *server.MCPServer
	next      http.Handler
	apiURL    string
	publicURL string
	client    *http.Client

	mu    sync.Mutex
	cache map[string]cachedGrant
}

type cachedGrant struct {
	g     grant.Grant
	ok    bool
	until time.Time
}

// HTTPHandler serves the MCP server over stateless Streamable HTTP on `/mcp`
// and `/mcp/t/<grant>`. Without WithGate it only passes the grant on and lets
// tk-api judge it (the shape before 2/10, kept for local and test use).
//
// mcp-go's DNS-rebinding guard is off: Ankayma's relay forwards raw bytes to a
// loopback port with the privUrl's fqdn as Host, which the guard would refuse.
// The guard protects unauthenticated local servers; here every write needs a
// signed grant a rebinding page does not have.
func HTTPHandler(s *server.MCPServer, opts ...HTTPOption) http.Handler {
	d := &doors{s: s, client: &http.Client{Timeout: 5 * time.Second}, cache: map[string]cachedGrant{}}
	for _, o := range opts {
		o(d)
	}
	h := server.NewStreamableHTTPServer(s,
		server.WithStateLess(true),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			ctx = ContextWithToken(ctx, tokenFromRequest(r))
			if dr, ok := r.Context().Value(doorKey{}).(door); ok {
				ctx = context.WithValue(ctx, doorKey{}, dr)
			}
			return ctx
		}),
		server.WithDisableLocalhostProtection(true),
	)
	d.next = h
	mux := http.NewServeMux()
	mux.Handle("/mcp", d)
	mux.Handle(tokenPathPrefix, d)
	return mux
}

func (d *doors) base(r *http.Request) string {
	if d.publicURL != "" {
		return d.publicURL
	}
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

func (d *doors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.apiURL == "" {
		d.next.ServeHTTP(w, r)
		return
	}
	base := d.base(r)
	if tok, ok := strings.CutPrefix(r.URL.Path, tokenPathPrefix); ok {
		d.serveURLDoor(w, r, strings.Trim(tok, "/"), base)
		return
	}
	d.serveStandardDoor(w, r, base)
}

// serveURLDoor: a private link. Never 401, never WWW-Authenticate.
func (d *doors) serveURLDoor(w http.ResponseWriter, r *http.Request, tok, base string) {
	if tok == "" {
		http.NotFound(w, r)
		return
	}
	_, ok, err := d.verify(r.Context(), tok)
	if err != nil {
		http.Error(w, "the store is not reachable right now", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.next.ServeHTTP(w, r.WithContext(contextWithDoor(r.Context(), EntryURL, base)))
}

// serveStandardDoor: /mcp. Lazy auth (owner 2/10): connecting, listing tools,
// and the store-free tools need no token; a call that needs the person's store
// without a valid token gets 401 from here, before the SDK — a 401 is the only
// thing that makes Claude show its Connect card (a 200 tool error would not).
func (d *doors) serveStandardDoor(w http.ResponseWriter, r *http.Request, base string) {
	var body []byte
	if r.Method == http.MethodPost {
		b, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			http.Error(w, "could not read the request", http.StatusBadRequest)
			return
		}
		body = b
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	calls := toolCalls(body)
	tok := bearer(r)
	if tok == "" {
		for _, name := range calls {
			if !publicTools[name] {
				d.unauthorized(w, base, "Sign in to use this ledger")
				return
			}
		}
		d.next.ServeHTTP(w, r)
		return
	}
	g, ok, err := d.verify(r.Context(), tok)
	if err != nil {
		http.Error(w, "the store is not reachable right now", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		d.unauthorized(w, base, "The sign-in has expired or was withdrawn; sign in again")
		return
	}
	if !g.Has(grant.ScopeWrite) {
		readOnly := d.readOnlyTools()
		for _, name := range calls {
			if !readOnly[name] && !publicTools[name] {
				d.insufficientScope(w, base)
				return
			}
		}
	}
	entry := EntryHeader
	if g.Via == grant.ViaOAuth {
		entry = EntryOAuth
	}
	d.next.ServeHTTP(w, r.WithContext(contextWithDoor(r.Context(), entry, base)))
}

// publicTools need no store and so no sign-in: whether a sentence is a claim
// yet, and what the store's words mean.
var publicTools = map[string]bool{ToolCheckClaim: true, ToolDefine: true}

func (d *doors) readOnlyTools() map[string]bool {
	out := map[string]bool{}
	for name, t := range d.s.ListTools() {
		if ro := t.Tool.Annotations.ReadOnlyHint; ro != nil && *ro {
			out[name] = true
		}
	}
	return out
}

const scopeChallenge = "ledger:read ledger:write"

func (d *doors) unauthorized(w http.ResponseWriter, base, desc string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="`+desc+
		`", resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp", scope="`+scopeChallenge+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_token", "error_description": desc})
}

func (d *doors) insufficientScope(w http.ResponseWriter, base string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", error_description="Writing to the ledger needs write access", scope="`+
		scopeChallenge+`", resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "insufficient_scope", "error_description": "Writing to the ledger needs write access"})
}

// toolCalls names the tools a JSON-RPC body calls, single message or batch.
func toolCalls(body []byte) []string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil
	}
	type msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	var msgs []msg
	if body[0] == '[' {
		_ = json.Unmarshal(body, &msgs)
	} else {
		var m msg
		if json.Unmarshal(body, &m) == nil {
			msgs = []msg{m}
		}
	}
	var out []string
	for _, m := range msgs {
		if m.Method == "tools/call" && !slices.Contains(out, m.Params.Name) {
			out = append(out, m.Params.Name)
		}
	}
	return out
}

// verify asks tk-api who this grant is. ok=false means tk-api refused it
// (bad signature, expired, revoked); err means tk-api could not be asked.
// Answers are kept for 30 seconds, so a revocation reaches this gate within
// that window — tk-api itself checks every write regardless.
func (d *doors) verify(ctx context.Context, tok string) (grant.Grant, bool, error) {
	key := tokenHash(tok)
	d.mu.Lock()
	if c, hit := d.cache[key]; hit && time.Now().Before(c.until) {
		d.mu.Unlock()
		return c.g, c.ok, nil
	}
	d.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.apiURL+"/v1/whoami", nil)
	if err != nil {
		return grant.Grant{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := d.client.Do(req)
	if err != nil {
		log.Printf("tk-mcp: whoami: %v", err)
		return grant.Grant{}, false, err
	}
	defer resp.Body.Close()
	var g grant.Grant
	ok := false
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
			return grant.Grant{}, false, err
		}
		ok = true
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
	default:
		return grant.Grant{}, false, &httpStatusErr{resp.StatusCode}
	}
	d.mu.Lock()
	if len(d.cache) > 5000 {
		d.cache = map[string]cachedGrant{}
	}
	d.cache[key] = cachedGrant{g: g, ok: ok, until: time.Now().Add(30 * time.Second)}
	d.mu.Unlock()
	return g, ok, nil
}

type httpStatusErr struct{ code int }

func (e *httpStatusErr) Error() string { return "tk-api whoami answered " + http.StatusText(e.code) }

func tokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}
