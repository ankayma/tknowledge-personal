// Package grant signs and verifies the caller's grant.
//
// The wire shape is Ankayma's existing grant token — base64(json).base64(Ed25519
// signature) with a `purpose` that keeps one grant kind from being replayed as
// another — so a privUrl grant minted by Ankayma's control plane and a dev
// grant minted by `tk token issue` verify through the same code; only the
// public key differs. Verification is offline (no call to the issuer).
package grant

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Purpose is the only purpose tk accepts.
const Purpose = "mcp-access-v1"

// Scopes tk understands.
const (
	ScopeRead  = "tk:read"
	ScopeWrite = "tk:write"
	ScopeAdmin = "tk:admin"
	// ScopeDemo marks a grant handed to a stranger trying the store: it sees
	// and reaches only the T/A functions, never the owner's own tools.
	ScopeDemo = "tk:demo"
)

// Grant is the signed payload. The first six fields are Ankayma's session
// grant fields; the rest are what tk needs to name a signer and a producer.
type Grant struct {
	Purpose     string   `json:"purpose"`
	GrantID     string   `json:"grant_id"`
	NodeID      string   `json:"node_id"`
	ActorID     string   `json:"actor_id"`
	IssuedAt    int64    `json:"issued_at"`
	ExpiresAt   int64    `json:"expires_at"` // unix seconds; 0 = no expiry (dev)
	UserID      string   `json:"user_id"`
	Aud         string   `json:"aud"`
	Scope       []string `json:"scope"`
	TenantID    string   `json:"tenant_id,omitempty"`
	ProductLine string   `json:"product_line,omitempty"`
	AnchorRef   string   `json:"anchor_ref,omitempty"`
	// Via names the door a grant was minted for: "oauth" for an access token
	// the authorization server issued after sign-in; empty for a grant handed
	// over as a link or a header value. tk-mcp reads it to answer in the form
	// that door can open (brief 2/10 V8).
	Via string `json:"via,omitempty"`
}

// ViaOAuth marks an access token minted by tk's authorization server.
const ViaOAuth = "oauth"

// IsHuman reports whether the actor is the user acting for themself. Only
// then may a request name a different producer (a person signing content an
// agent drafted); an agent's grant always produces as that agent.
func (g Grant) IsHuman() bool { return g.ActorID == g.UserID }

func (g Grant) Has(scope string) bool { return slices.Contains(g.Scope, scope) }

var (
	ErrMalformed  = errors.New("grant: malformed token")
	ErrSignature  = errors.New("grant: signature not valid for any trusted key")
	ErrPurpose    = errors.New("grant: wrong purpose")
	ErrAudience   = errors.New("grant: wrong audience")
	ErrExpired    = errors.New("grant: expired")
	ErrIncomplete = errors.New("grant: missing grant_id, user_id or actor_id")
)

// Sign returns the token for g. IssuedAt, GrantID and Purpose are filled in
// when empty.
func Sign(key ed25519.PrivateKey, g Grant) (string, error) {
	if g.Purpose == "" {
		g.Purpose = Purpose
	}
	if g.IssuedAt == 0 {
		g.IssuedAt = time.Now().Unix()
	}
	if g.GrantID == "" {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		g.GrantID = "gr_" + base64.RawURLEncoding.EncodeToString(b)
	}
	payload, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(key, payload)
	// URL-safe: the token rides in a URL path. Verify accepts both alphabets.
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verifier checks tokens against trusted keys and this deployment's audience.
type Verifier struct {
	Keys []ed25519.PublicKey
	Aud  string
	Now  func() time.Time
}

// Verify checks signature first, so an unsigned caller learns nothing from the
// later checks. Revocation is not checked here — it needs the database.
func (v *Verifier) Verify(token string) (Grant, error) {
	p, s, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || p == "" || s == "" {
		return Grant{}, ErrMalformed
	}
	payload, err := decode(p)
	if err != nil {
		return Grant{}, ErrMalformed
	}
	sig, err := decode(s)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Grant{}, ErrMalformed
	}
	trusted := false
	for _, k := range v.Keys {
		if ed25519.Verify(k, payload, sig) {
			trusted = true
			break
		}
	}
	if !trusted {
		return Grant{}, ErrSignature
	}
	var g Grant
	if err := json.Unmarshal(payload, &g); err != nil {
		return Grant{}, ErrMalformed
	}
	if g.Purpose != Purpose {
		return Grant{}, ErrPurpose
	}
	if g.Aud != v.Aud {
		return Grant{}, ErrAudience
	}
	if g.GrantID == "" || g.UserID == "" || g.ActorID == "" {
		return Grant{}, ErrIncomplete
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	if g.ExpiresAt != 0 && now().Unix() >= g.ExpiresAt {
		return Grant{}, ErrExpired
	}
	return g, nil
}

// Claims reads a grant's payload without checking the signature. tk-mcp uses
// it to shape what a caller sees; every write still goes through Verify at
// tk-api, so a forged payload buys nothing but a longer tool list it cannot use.
func Claims(token string) (Grant, bool) {
	payload, _, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || payload == "" {
		return Grant{}, false
	}
	raw, err := decode(payload)
	if err != nil {
		return Grant{}, false
	}
	var g Grant
	if err := json.Unmarshal(raw, &g); err != nil || g.Purpose != Purpose {
		return Grant{}, false
	}
	return g, true
}

func decode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// ParsePublicKeys reads a comma-separated list of base64 Ed25519 public keys.
func ParsePublicKeys(list string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, part := range strings.Split(list, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		b, err := decodeKey(part)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("grant: %q is not a base64 Ed25519 public key", part)
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	if len(keys) == 0 {
		return nil, errors.New("grant: no public keys")
	}
	return keys, nil
}

// ParseSeed reads a base64 32-byte Ed25519 seed (the key file format of
// `tk token keygen`, same as Ankayma's `from_seed`).
func ParseSeed(s string) (ed25519.PrivateKey, error) {
	b, err := decodeKey(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("grant: seed must be base64 of 32 bytes")
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// EncodeKey is the base64 form used for seeds and public keys.
func EncodeKey(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}
