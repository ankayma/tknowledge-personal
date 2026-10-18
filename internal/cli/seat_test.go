package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/grant"
)

// tk seat (owner 13/10/2026): read where a person stands, move them with a
// reason; the store is found from the address that opened it.
func TestTk_Seat(t *testing.T) {
	app, _, out, env := newApp(t)
	if _, err := env.Admin.Exec(context.Background(), `
		INSERT INTO tenant (id, name, tier) VALUES ('demo-owner', 'trial demo-owner', 'trial');
		INSERT INTO trial_owner (email, tenant_id) VALUES ('owner@example.test', 'demo-owner')`); err != nil {
		t.Fatal(err)
	}
	admin := as(app, env.Token(t, "bao", "bao", grant.ScopeAdmin))

	if s := run(t, admin, out, "seat", "owner@example.test"); !strings.Contains(s, `"plan": "free"`) || !strings.Contains(s, `"tenant": "demo-owner"`) {
		t.Fatalf("seat read: %s", s)
	}
	if err := admin.Run(context.Background(), []string{"seat", "owner@example.test", "unlimited"}); err == nil {
		t.Fatal("a change without --reason must be refused")
	}
	s := run(t, admin, out, "seat", "owner@example.test", "unlimited", "--reason", "owner store")
	if !strings.Contains(s, `"plan": "unlimited"`) || !strings.Contains(s, `"by": "bao"`) || !strings.Contains(s, `"reason": "owner store"`) {
		t.Fatalf("seat change: %s", s)
	}
	if s := run(t, admin, out, "seat", "someone@example.test", "standard", "--tenant", "demo-owner", "--reason", "paid"); !strings.Contains(s, `"plan": "standard"`) {
		t.Fatalf("seat by --tenant: %s", s)
	}
	reader := as(app, env.Token(t, "owner@example.test", "owner@example.test"))
	if err := reader.Run(context.Background(), []string{"seat", "owner@example.test"}); err == nil {
		t.Fatal("tk seat needs tk:admin")
	}
}
