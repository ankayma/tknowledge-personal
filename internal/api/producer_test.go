package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/grant"
)

func TestProducerOf(t *testing.T) {
	for label, want := range map[string]string{
		"claude.ai": "claude", "Claude": "claude", "Anthropic MCP": "claude",
		"ChatGPT": "chatgpt", "chatgpt.com": "chatgpt", "OpenAI Connector": "chatgpt",
		"Gemini": "gemini", "gemini.google.com": "gemini",
		"Cursor": "cursor", "My Script v2!": "my-script-v2", "": "agent", "—": "agent",
	} {
		if got := producerOf(label); got != want {
			t.Errorf("producerOf(%q) = %q, want %q", label, got, want)
		}
	}
}

// A grant names the AI that signed in, not always Claude: ChatGPT's writes
// were stored as claude's (D-130, 14/10/2026).
func TestOAuth_GrantNamesTheClient(t *testing.T) {
	env := newTestServer(t)
	sent := env.enableOAuth(t)
	const cb = "https://chatgpt.com/connector_platform_oauth_redirect"
	for i, c := range []struct{ name, want string }{
		{"ChatGPT", "chatgpt"}, {"", "agent"}, {"owner", "agent"},
		{"p3@example.test", "agent"}, // a client named after the person is not the person
	} {
		name, want, email := c.name, c.want, fmt.Sprintf("p%d@example.test", i)
		resp, err := http.Post(env.URL+"/oauth/register", "application/json",
			strings.NewReader(`{"redirect_uris":["`+cb+`"],"client_name":"`+name+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		var reg struct {
			ClientID string `json:"client_id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&reg)
		resp.Body.Close()
		code := signIn(t, env, sent, authzValues(reg.ClientID, cb, "ledger:read ledger:write"), email)
		st, tok := token(t, env, url.Values{"grant_type": {"authorization_code"}, "code": {code},
			"code_verifier": {testVerifier}, "client_id": {reg.ClientID}})
		g, ok := grant.Claims(tok.AccessToken)
		if st != http.StatusOK || !ok || g.ActorID != want {
			t.Fatalf("client %q: %d actor %q, want %q", name, st, g.ActorID, want)
		}
	}
}
