package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/ccclient"
	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

const publicURL = "https://tk.example"

type doorsEnv struct {
	t   *testing.T
	env *testapi.Env
	mcp *httptest.Server
}

func startDoors(t *testing.T) *doorsEnv {
	t.Helper()
	env := testapi.Start(t)
	cc := testapi.StartCC(t)
	env.EnableOAuth(t, "review@example.test", "246810")
	ts := httptest.NewServer(HTTPHandler(New(tkclient.New(env.Server.URL), ccclient.New(cc.URL)),
		WithGate(env.Server.URL, publicURL)))
	t.Cleanup(ts.Close)
	return &doorsEnv{t: t, env: env, mcp: ts}
}

// post sends one JSON-RPC message to path with an optional bearer.
func (d *doorsEnv) post(path, bearer string, msg map[string]any) (int, http.Header, []byte) {
	d.t.Helper()
	msg["jsonrpc"] = "2.0"
	if _, ok := msg["id"]; !ok {
		msg["id"] = 1
	}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, d.mcp.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

func (d *doorsEnv) tool(path, bearer, name string, args map[string]any) (int, http.Header, []byte) {
	d.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	return d.post(path, bearer, map[string]any{"method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
}

// result decodes a 200 tool call into its structured content.
func (d *doorsEnv) result(status int, _ http.Header, raw []byte) map[string]any {
	d.t.Helper()
	if status != http.StatusOK {
		d.t.Fatalf("HTTP %d: %s", status, raw)
	}
	var rpc rpcResp
	if err := json.Unmarshal(raw, &rpc); err != nil || rpc.Error != nil {
		d.t.Fatalf("rpc: %v %s", err, raw)
	}
	var tr toolResult
	if err := json.Unmarshal(rpc.Result, &tr); err != nil {
		d.t.Fatal(err)
	}
	if tr.IsError {
		d.t.Fatalf("tool error: %s", tr.Content[0].Text)
	}
	var out map[string]any
	if err := json.Unmarshal(tr.Structured, &out); err != nil {
		d.t.Fatalf("structured: %s", tr.Structured)
	}
	return out
}

// Lazy auth (owner 2/10): connecting, listing and the store-free tools need no
// token; anything that needs the person's store gets HTTP 401 with the
// discovery pointer — never a 200 "please sign in" — and a token that does not
// verify gets 401 too.
func TestDoors_LazyAuthAnd401(t *testing.T) {
	d := startDoors(t)

	if st, _, raw := d.post("/mcp", "", map[string]any{"method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"},
	}}); st != http.StatusOK {
		t.Fatalf("initialize without a token: %d %s", st, raw)
	}
	if st, _, raw := d.post("/mcp", "", map[string]any{"method": "tools/list"}); st != http.StatusOK || !strings.Contains(string(raw), ToolAssume) {
		t.Fatalf("tools/list without a token: %d", st)
	}
	out := d.result(d.tool("/mcp", "", ToolCheckClaim, map[string]any{"claim": "Acme signed the renewal on 2026-09-30"}))
	if len(out) == 0 {
		t.Fatal("check_claim without a token answers")
	}

	for _, name := range []string{ToolAssume, ToolLedger} {
		st, h, raw := d.tool("/mcp", "", name, map[string]any{})
		wa := h.Get("WWW-Authenticate")
		if st != http.StatusUnauthorized ||
			!strings.Contains(wa, `Bearer error="invalid_token"`) ||
			!strings.Contains(wa, `resource_metadata="`+publicURL+`/.well-known/oauth-protected-resource/mcp"`) ||
			!strings.Contains(wa, `scope="ledger:read ledger:write"`) ||
			!strings.Contains(string(raw), `"invalid_token"`) {
			t.Fatalf("%s without a token: %d %q %s", name, st, wa, raw)
		}
	}
	if st, h, _ := d.tool("/mcp", "not-a-grant", ToolLedger, nil); st != http.StatusUnauthorized || h.Get("WWW-Authenticate") == "" {
		t.Fatalf("a token that does not verify: %d", st)
	}
}

// Step-up: a read-only sign-in may read, and writing asks for more scope with
// 403 insufficient_scope.
func TestDoors_StepUp403(t *testing.T) {
	d := startDoors(t)
	ro := d.env.OAuthLogin(t, "review@example.test", "246810", "ledger:read")
	if st, _, raw := d.tool("/mcp", ro.AccessToken, ToolLedger, nil); st != http.StatusOK {
		t.Fatalf("read with a read-only token: %d %s", st, raw)
	}
	st, h, _ := d.tool("/mcp", ro.AccessToken, ToolAssume, map[string]any{"claim": "x"})
	if wa := h.Get("WWW-Authenticate"); st != http.StatusForbidden ||
		!strings.Contains(wa, `error="insufficient_scope"`) || !strings.Contains(wa, `scope="ledger:read ledger:write"`) {
		t.Fatalf("write with a read-only token: %d %q", st, wa)
	}
}

// The URL door is never advertised: a grant there that does not verify is a
// bare 404 with no WWW-Authenticate, and a good one works.
func TestDoors_URLDoorNeverChallenges(t *testing.T) {
	d := startDoors(t)
	for _, bad := range []string{"/mcp/t/not-a-grant", "/mcp/t/"} {
		st, h, _ := d.post(bad, "", map[string]any{"method": "tools/list"})
		if st != http.StatusNotFound || h.Get("WWW-Authenticate") != "" {
			t.Fatalf("%s: %d %q", bad, st, h.Get("WWW-Authenticate"))
		}
	}
	expired, _ := grant.Sign(d.env.Key, grant.Grant{UserID: "a@example.test", ActorID: "claude", NodeID: "t",
		Aud: testapi.Aud, Scope: []string{grant.ScopeRead}, ExpiresAt: 1})
	if st, h, _ := d.post("/mcp/t/"+expired, "", map[string]any{"method": "tools/list"}); st != http.StatusNotFound || h.Get("WWW-Authenticate") != "" {
		t.Fatalf("expired private link: %d", st)
	}
	good := d.env.Token(t, "a@example.test", "claude")
	if st, _, raw := d.post("/mcp/t/"+good, "", map[string]any{"method": "tools/list"}); st != http.StatusOK {
		t.Fatalf("good private link: %d %s", st, raw)
	}
}

// One store, three doors, three link forms (brief 2/10 V8) — and the grant of
// the URL door never appears in an answer through another door.
func TestDoors_ThreeDoorsThreeLinkForms(t *testing.T) {
	d := startDoors(t)
	const email = "review@example.test"
	oauth := d.env.OAuthLogin(t, email, "246810", "ledger:read ledger:write")
	claims, ok := grant.Claims(oauth.AccessToken)
	if !ok || claims.TenantID == "" || claims.Via != grant.ViaOAuth {
		t.Fatalf("access token: %+v", claims)
	}
	door := func() string {
		tok, err := grant.Sign(d.env.Key, grant.Grant{UserID: email, ActorID: "claude", NodeID: "tknowledge",
			Aud: testapi.Aud, TenantID: claims.TenantID, Scope: []string{grant.ScopeRead, grant.ScopeWrite, grant.ScopeDemo}})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	header, urlGrant := door(), door()

	opened := d.result(d.tool("/mcp", oauth.AccessToken, ToolThreadOpen, map[string]any{
		"title": "Interview — Northwind", "keywords": []string{"a42"}}))
	id, _ := opened["id"].(string)
	if id == "" {
		t.Fatalf("thread_open: %v", opened)
	}
	if opened["link"] != publicURL+"/mcp#"+id {
		t.Fatalf("oauth link: %v", opened["link"])
	}

	cases := []struct{ name, path, bearer, want string }{
		{"oauth", "/mcp", oauth.AccessToken, publicURL + "/mcp#" + id},
		{"header", "/mcp", header, publicURL + "/mcp#" + id},
		{"url", "/mcp/t/" + urlGrant, "", publicURL + "/mcp/t/" + urlGrant + "#" + id},
	}
	for _, c := range cases {
		st, h, raw := d.tool(c.path, c.bearer, ToolThread, map[string]any{"thread": id})
		got := d.result(st, h, raw)
		if got["link"] != c.want {
			t.Fatalf("%s door: link %v, want %v", c.name, got["link"], c.want)
		}
		if c.name == "url" {
			continue
		}
		for _, leak := range []string{"/mcp/t/", urlGrant, header, oauth.AccessToken} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("%s door answer carries %q", c.name, leak[:min(len(leak), 20)])
			}
		}
	}

	// Writes through the header and OAuth doors carry no grant either.
	for _, bearer := range []string{header, oauth.AccessToken} {
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{ToolAssume, map[string]any{"claim": "Northwind sends an offer before 2026-10-31", "type": "A-p", "source": "owner",
				"if": "an offer email arrives", "not": "no email by 2026-10-31", "thread": id, "confirm_new": true}},
			{ToolLedger, nil}, {ToolOpenItems, nil}, {ToolLookup, map[string]any{"q": "a42"}},
		} {
			st, _, raw := d.tool("/mcp", bearer, call.name, call.args)
			if st != http.StatusOK {
				t.Fatalf("%s: %d %s", call.name, st, raw)
			}
			for _, leak := range []string{"/mcp/t/", urlGrant, header, oauth.AccessToken} {
				if strings.Contains(string(raw), leak) {
					t.Fatalf("%s answer carries a grant or a private link", call.name)
				}
			}
		}
	}
}
