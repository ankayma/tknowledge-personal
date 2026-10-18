package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// Sending the link by mail. One provider, reached over HTTPS rather than SMTP —
// a VPS talking SMTP to the world is a deliverability problem, and the outbound
// port is one firewall change away from silently breaking.
//
// The sender is MAIL_FROM, an address on a domain verified with Resend. The
// fallback below is Resend's own test sender, which only delivers to the
// Resend account's own address. Every message signs off with the operator's
// site when TK_SITE_URL is set.

// Mailer sends one message. nil means this deployment cannot send, which is a
// state the door has to keep working in: a person who cannot be mailed still
// gets their link on the page.
type Mailer interface {
	Send(ctx context.Context, to, subject, text string) error
}

// Resend posts to the Resend API.
type Resend struct {
	APIKey string
	From   string // "T-knowledge <no-reply@tknowledge.org>"
	// ReplyTo is where an answer to any message goes (TK_CONTACT, e.g.
	// support@tknowledge.org); "" = no Reply-To header.
	ReplyTo string
	HTTP    *http.Client
	// Endpoint is the Resend API URL; "" = the real one. Tests point it at a
	// local server.
	Endpoint string
}

// htmlMailer is a Mailer that can also send an HTML alternative. A plain-text
// message that is nothing but a number scores as spam at some providers
// (iCloud, 3/10/2026), so the sign-in code goes out as both.
type htmlMailer interface {
	SendHTML(ctx context.Context, to, subject, text, html string) error
}

// MailerFromEnv returns the mailer for this deployment, or a true nil when no
// key is configured.
//
// The return type is the interface, not *Resend, on purpose. A nil *Resend put
// into a Mailer field makes the field non-nil — the interface holds a type and a
// nil pointer — so `if Mail != nil` passes and the first send panics on a nil
// receiver. Returning Mailer and a literal nil makes the caller's nil check mean
// what it looks like. This cost a panic in production on 29/9.
func MailerFromEnv(apiKey, from string) Mailer {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	if strings.TrimSpace(from) == "" {
		from = "T-knowledge <onboarding@resend.dev>"
	}
	return &Resend{APIKey: apiKey, From: from, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (m *Resend) Send(ctx context.Context, to, subject, text string) error {
	return m.SendHTML(ctx, to, subject, text, "")
}

func (m *Resend) SendHTML(ctx context.Context, to, subject, text, html string) error {
	msg := map[string]any{"from": m.From, "to": []string{to}, "subject": subject, "text": text}
	if html != "" {
		msg["html"] = html
	}
	if m.ReplyTo != "" {
		msg["reply_to"] = m.ReplyTo
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	endpoint := m.Endpoint
	if endpoint == "" {
		endpoint = "https://api.resend.com/emails"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("resend %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

// cleanEmail checks the shape and normalises the case. It does not check that
// the address exists — that is what sending finds out, and a confirmation loop
// would put one more wall between a stranger and the demo.
func cleanEmail(raw string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("that does not look like an email address")
	}
	at := strings.LastIndex(addr.Address, "@")
	if at < 1 || !strings.Contains(addr.Address[at:], ".") {
		return "", fmt.Errorf("that does not look like an email address")
	}
	return strings.ToLower(addr.Address), nil
}

// linkMail is what a person gets. Plain text on purpose: the link is the point,
// and a plain message survives every client without a rendering surprise.
func linkMail(base, token, store string, expires *time.Time, doorTerms, site string) (subject, text string) {
	url := base + "/mcp/t/" + token
	terms := "This is a trial store: " + doorTerms + ". When its writes are used up it stays readable."
	if expires != nil {
		terms = fmt.Sprintf("This is a trial store: %s. It closes on %s.",
			doorTerms, expires.UTC().Format("2 January 2006"))
	}
	subject = "Your T-knowledge store"
	text = strings.Join([]string{
		"Your store is open: " + store,
		"",
		"Three ways in — pick the one that fits:",
		"",
		"1. Claude (web, Desktop, mobile, Code): Settings, Connectors, Add custom connector,",
		"   paste " + base + "/mcp and sign in with this email address when Claude asks.",
		"",
		"2. Cursor, a script, or mcp-remote: connect to " + base + "/mcp and send this header",
		"   (not yet tried on Cursor itself):",
		"",
		"   Authorization: Bearer " + token,
		"",
		"3. A private link that carries the key on its own:",
		"",
		"   " + url,
		"",
		"The header value and the private link are the key to the store, so treat them like",
		"a password. Anyone who has them can read and write your store, and nobody else can.",
		"",
		"For Claude Desktop's config file with the private link:",
		"",
		`  {"mcpServers": {"t-knowledge": {"command": "npx",`,
		`    "args": ["-y", "mcp-remote", "` + url + `"]}}}`,
		"",
		terms,
		"Do not put anything confidential in it.",
		"",
		"Three things worth trying first:",
		"  1. \"What can this store do? Read me its definition of a claim.\"",
		"  2. \"Record that our biggest customer is renewing this quarter.\" — watch it refuse, twice.",
		"  3. \"What do we actually know, and what are we only assuming?\"",
	}, "\n") + signoff(site)
	return subject, text
}

// signoff closes a message with the operator's site (TK_SITE_URL), or with
// nothing when the deployment has none.
func signoff(site string) string {
	if site == "" {
		return ""
	}
	return "\n\n— " + site
}
