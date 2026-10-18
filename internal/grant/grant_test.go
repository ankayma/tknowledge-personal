package grant

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func base() Grant {
	return Grant{UserID: "bao", ActorID: "claude-desktop", NodeID: "dev", Aud: "tk", Scope: []string{ScopeWrite}}
}

func TestSignVerify_RoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	tok, err := Sign(priv, base())
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("token must be URL-safe: %s", tok)
	}
	g, err := (&Verifier{Keys: []ed25519.PublicKey{pub}, Aud: "tk"}).Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if g.Purpose != Purpose || g.UserID != "bao" || g.ActorID != "claude-desktop" || g.GrantID == "" || g.IssuedAt == 0 {
		t.Fatalf("grant: %+v", g)
	}
	if g.IsHuman() || !g.Has(ScopeWrite) || g.Has(ScopeAdmin) {
		t.Fatalf("helpers: %+v", g)
	}
}

// A token in Ankayma's current alphabet (STANDARD_NO_PAD) verifies too.
func TestVerify_AcceptsAnkaymaStandardAlphabet(t *testing.T) {
	pub, priv := newKey(t)
	g := base()
	g.Purpose, g.GrantID, g.IssuedAt = Purpose, "g1", 1
	payload, _ := json.Marshal(g)
	tok := base64.RawStdEncoding.EncodeToString(payload) + "." + base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, payload))
	if _, err := (&Verifier{Keys: []ed25519.PublicKey{pub}, Aud: "tk"}).Verify(tok); err != nil {
		t.Fatal(err)
	}
}

func TestVerify_Rejections(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	v := &Verifier{Keys: []ed25519.PublicKey{pub}, Aud: "tk", Now: func() time.Time { return now }}
	sign := func(k ed25519.PrivateKey, mut func(*Grant)) string {
		g := base()
		mut(&g)
		tok, err := Sign(k, g)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	good := sign(priv, func(*Grant) {})
	p, s, _ := strings.Cut(good, ".")
	tampered, _ := base64.RawURLEncoding.DecodeString(p)
	tampered = []byte(strings.Replace(string(tampered), `"bao"`, `"eve"`, 1))

	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"empty", "", ErrMalformed},
		{"no dot", "abc", ErrMalformed},
		{"short sig", p + ".AAAA", ErrMalformed},
		{"other key", sign(otherPriv, func(*Grant) {}), ErrSignature},
		{"tampered payload", base64.RawURLEncoding.EncodeToString(tampered) + "." + s, ErrSignature},
		{"wrong purpose", sign(priv, func(g *Grant) { g.Purpose = "agent-session-v1" }), ErrPurpose},
		{"wrong aud", sign(priv, func(g *Grant) { g.Aud = "other" }), ErrAudience},
		{"no user", sign(priv, func(g *Grant) { g.UserID = "" }), ErrIncomplete},
		{"no actor", sign(priv, func(g *Grant) { g.ActorID = "" }), ErrIncomplete},
		{"expired", sign(priv, func(g *Grant) { g.ExpiresAt = now.Unix() }), ErrExpired},
	}
	for _, c := range cases {
		if _, err := v.Verify(c.token); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}

	// Not yet expired, and no expiry at all, both pass.
	if _, err := v.Verify(sign(priv, func(g *Grant) { g.ExpiresAt = now.Unix() + 1 })); err != nil {
		t.Errorf("unexpired: %v", err)
	}
	// Several trusted keys (key rotation, dev + Ankayma): any one suffices.
	v.Keys = append(v.Keys, otherPub)
	if _, err := v.Verify(sign(otherPriv, func(*Grant) {})); err != nil {
		t.Errorf("second key: %v", err)
	}
}

func TestKeys_Parse(t *testing.T) {
	pub, priv := newKey(t)
	keys, err := ParsePublicKeys(" " + EncodeKey(pub) + ", ")
	if err != nil || len(keys) != 1 || !keys[0].Equal(pub) {
		t.Fatalf("ParsePublicKeys: %v %v", keys, err)
	}
	if _, err := ParsePublicKeys(""); err == nil {
		t.Fatal("empty key list must fail (fail-closed)")
	}
	if _, err := ParsePublicKeys("nope"); err == nil {
		t.Fatal("garbage key must fail")
	}
	seed, err := ParseSeed(EncodeKey(priv.Seed()))
	if err != nil || !seed.Equal(priv) {
		t.Fatalf("ParseSeed: %v", err)
	}
}
