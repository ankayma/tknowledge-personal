package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A deployment with no mail key must end up with a Mailer that is actually nil,
// so the door can say MAIL_UNAVAILABLE instead of panicking on the first send.
//
// This is the typed-nil trap: a nil *Resend assigned to a Mailer field makes the
// field non-nil, `if Mail != nil` passes, and Send panics on a nil receiver.
// That is exactly what happened in production on 29/9, and the tests missed it
// because they always set a real mailer or left the field untouched.
func TestMailerFromEnv_NoKeyIsATrueNil(t *testing.T) {
	var d Demo
	d.Mail = MailerFromEnv("", "")
	if d.Mail != nil {
		t.Fatalf("a mailer built from no key must be nil in the field, got %#v", d.Mail)
	}
	if got := MailerFromEnv("re_test", ""); got == nil {
		t.Fatal("a key should produce a mailer")
	} else if r, ok := got.(*Resend); !ok || r.From == "" {
		t.Errorf("a mailer needs a from address: %#v", got)
	}
}

// Every message tk sends signs off with the operator's site when one is set
// (TK_SITE_URL), and with nothing when none is.
func TestMail_SignsOffWithTheOperatorsSite(t *testing.T) {
	_, link := linkMail("https://tknowledge.example", "tok", "demo-x", nil, "no end date, 100 writes a month, reads unlimited", "https://operator.example")
	_, otpText, page := otpMail("123456", "support@tknowledge.org", "https://operator.example")
	for name, body := range map[string]string{"otp": otpText, "link": link} {
		if !strings.HasSuffix(strings.TrimSpace(body), "— https://operator.example") {
			t.Fatalf("%s mail must sign off with the operator's site:\n%s", name, body)
		}
	}
	if !strings.Contains(page, `href="https://operator.example"`) {
		t.Fatal("the HTML part links the operator's site")
	}
	_, bare := linkMail("https://tknowledge.example", "tok", "demo-x", nil, "terms", "")
	_, bareOTP, barePage := otpMail("123456", "", "")
	if strings.Contains(bare, "— http") || strings.Contains(bareOTP, "— http") || strings.Contains(barePage, "operator.example") {
		t.Fatal("no site, no sign-off")
	}
}

// The sign-in code is in the subject, the text says why the mail came, and an
// HTML part goes with it; answers go to the contact address. A bare number in
// plain text was junked by iCloud on 3/10/2026 with DKIM, SPF and DMARC passing.
func TestOTPMail_SubjectContextHTMLAndReplyTo(t *testing.T) {
	subject, text, page := otpMail("151916", "support@tknowledge.org", "")
	if subject != "151916 is your T-knowledge sign-in code" {
		t.Fatalf("subject: %q", subject)
	}
	for _, w := range []string{"151916", "ten minutes", "support@tknowledge.org", "connect an assistant"} {
		if !strings.Contains(text, w) || !strings.Contains(page, w) {
			t.Fatalf("text and HTML must both carry %q", w)
		}
	}
	if !strings.HasPrefix(page, "<!doctype html>") {
		t.Fatal("HTML part must be a document")
	}

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	m := &Resend{APIKey: "k", From: "T-knowledge <no-reply@tknowledge.org>", ReplyTo: "support@tknowledge.org", HTTP: srv.Client(), Endpoint: srv.URL}
	if err := m.SendHTML(context.Background(), "a@b.example", subject, text, page); err != nil {
		t.Fatal(err)
	}
	if got["reply_to"] != "support@tknowledge.org" || got["html"] != page || got["text"] != text || got["subject"] != subject {
		t.Fatalf("payload: %v", got)
	}
	// A plain Send still carries Reply-To and no HTML.
	got = nil
	if err := m.Send(context.Background(), "a@b.example", "s", "t"); err != nil {
		t.Fatal(err)
	}
	if _, has := got["html"]; has || got["reply_to"] != "support@tknowledge.org" {
		t.Fatalf("plain payload: %v", got)
	}
}
