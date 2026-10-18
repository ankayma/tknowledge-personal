package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// fakeCC records the causes tk would issue on cause-channel. tk-api is real
// (testapi); cause-channel is another module, so its POST /causes is stubbed
// with the same body contract.
type fakeCC struct {
	mu     sync.Mutex
	causes []map[string]any
}

func (f *fakeCC) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/causes" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		f.causes = append(f.causes, body)
		n := len(f.causes)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "01CAUSE" + strconv.Itoa(n)})
	})
}

// newApp runs tk as CRO: TK_TOKEN is a grant in CRO's own name.
func newApp(t *testing.T) (*App, *fakeCC, *bytes.Buffer, *testapi.Env) {
	t.Helper()
	env := testapi.Start(t)
	cc := &fakeCC{}
	ccSrv := httptest.NewServer(cc.handler())
	t.Cleanup(ccSrv.Close)
	out := &bytes.Buffer{}
	app := &App{TK: tkclient.New(env.Server.URL).WithToken(env.Token(t, "CRO", "CRO")), CC: NewCauseChannel(ccSrv.URL), Out: out}
	return app, cc, out, env
}

// as returns the same CLI running under another grant.
func as(app *App, token string) *App {
	cp := *app
	cp.TK = app.TK.WithToken(token)
	return &cp
}

func run(t *testing.T, app *App, out *bytes.Buffer, args ...string) string {
	t.Helper()
	out.Reset()
	if err := app.Run(context.Background(), args); err != nil {
		t.Fatalf("tk %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func claimIDs(t *testing.T, importOut string) map[string]int64 {
	t.Helper()
	ids := map[string]int64{}
	for _, line := range strings.Split(importOut, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "claim" && f[2] == "→" {
			id, err := strconv.ParseInt(strings.TrimPrefix(f[3], "claim_id="), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			ids[f[1]] = id
		}
	}
	return ids
}

func showEff(t *testing.T, app *App, out *bytes.Buffer, id int64) (bool, string, int) {
	t.Helper()
	var v struct {
		Eff     bool   `json:"eff"`
		Mark    string `json:"mark"`
		History []any  `json:"history"`
	}
	if err := json.Unmarshal([]byte(run(t, app, out, "show", strconv.FormatInt(id, 10))), &v); err != nil {
		t.Fatal(err)
	}
	return v.Eff, v.Mark, len(v.History)
}

func writeTokens(t *testing.T, tokens map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.json")
	raw, _ := json.Marshal(tokens)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The AlphaCorp steps 1, 3, 5 through the CLI (script.md), with every move
// preceded by its cause on cause-channel and every row under its signer's grant.
func TestTk_AlphaCorpThroughCLI(t *testing.T) {
	app, cc, out, env := newApp(t)
	ctx := context.Background()

	// Step 1 — import: T lines (CRO) become propose + promote under CRO's
	// grant; 905 is proposed under loan-agent's own grant from --tokens.
	tokens := writeTokens(t, map[string]string{"loan-agent": env.Token(t, "loan-agent", "loan-agent")})
	ids := claimIDs(t, run(t, app, out, "import", "../../fixtures/alphacorp", "--tokens", tokens))
	c102, c880, c905 := ids["102"], ids["880"], ids["905"]
	if c102 == 0 || c880 == 0 || c905 == 0 {
		t.Fatalf("import did not report all three claims: %v", ids)
	}
	for _, id := range []int64{c102, c880} {
		eff, mark, rows := showEff(t, app, out, id)
		if !eff || mark != "T" || rows != 2 {
			t.Fatalf("claim %d after import: eff=%v mark=%s rows=%d", id, eff, mark, rows)
		}
	}
	if eff, mark, rows := showEff(t, app, out, c905); eff || mark != "A" || rows != 1 {
		t.Fatalf("905 after import: eff=%v mark=%s rows=%d", eff, mark, rows)
	}
	var producer905 string
	if err := env.Admin.QueryRow(ctx, `SELECT producer FROM ledger WHERE id = $1`, c905).Scan(&producer905); err != nil || producer905 != "loan-agent" {
		t.Fatalf("905 producer: %q %v", producer905, err)
	}
	if len(cc.causes) != 2 {
		t.Fatalf("import of two T claims should issue two causes, got %d", len(cc.causes))
	}

	// Step 3 — CRO promotes 905 on 102 and 880.
	run(t, app, out, "promote", strconv.FormatInt(c905, 10), "--reason", "facility approved",
		"--basis-kind", "derivation", "--refs", strconv.FormatInt(c102, 10)+","+strconv.FormatInt(c880, 10))
	if eff, mark, _ := showEff(t, app, out, c905); !eff || mark != "T" {
		t.Fatalf("905 after promote: eff=%v mark=%s", eff, mark)
	}

	// The cause tk issued for that promote; approver
	// is the grant's user.
	last := cc.causes[len(cc.causes)-1]
	if last["effect_type"] != "ledger_move" || last["scope"] != "claim:"+strconv.FormatInt(c905, 10) ||
		last["approver"] != "CRO" || last["producer"] != "CRO" || last["actor"] != "tk-cli" ||
		last["window_seconds"] != float64(60) {
		t.Fatalf("promote cause: %v", last)
	}

	// Step 5 — Alice, under her own grant, demotes 880 → eff(905) = A.
	alice := as(app, env.Token(t, "Alice", "Alice"))
	demoteOut := run(t, alice, out, "demote", strconv.FormatInt(c880, 10), "--reason", "valuation revised down")
	if !strings.Contains(demoteOut, "signer=Alice") {
		t.Fatalf("demote should be signed by Alice: %s", demoteOut)
	}
	if eff, mark, _ := showEff(t, app, out, c905); eff || mark != "T" {
		t.Fatalf("905 after demote of 880: eff=%v mark=%s (mark must stay T, eff must fall)", eff, mark)
	}
	var audit struct {
		Eff   bool `json:"eff"`
		Truth struct {
			OK        bool   `json:"ok"`
			FailingID *int64 `json:"failing_id"`
		} `json:"truth"`
	}
	if err := json.Unmarshal([]byte(run(t, app, out, "audit", strconv.FormatInt(c905, 10))), &audit); err != nil {
		t.Fatal(err)
	}
	if audit.Eff || audit.Truth.OK || audit.Truth.FailingID == nil || *audit.Truth.FailingID != c880 {
		t.Fatalf("audit 905: %+v (want truth failing_id %d)", audit, c880)
	}

	// Stats and flags round-trip (flags are opened by tk-checker, not by reads).
	var stats map[string]int64
	if err := json.Unmarshal([]byte(run(t, app, out, "stats")), &stats); err != nil {
		t.Fatal(err)
	}
	// 102 T/eff, 880 demoted (A), 905 still marked T but eff fell → t_marked 2, t_eff 1.
	if stats["claims"] != 3 || stats["t_marked"] != 2 || stats["t_eff"] != 1 || stats["orphans"] != 1 {
		t.Fatalf("stats: %v", stats)
	}
	if s := strings.TrimSpace(run(t, app, out, "flags")); s != "[]" {
		t.Fatalf("flags before any sweep should be empty, got %s", s)
	}
	if len(cc.causes) != 4 {
		t.Fatalf("expected 4 causes (2 import, promote, demote), got %d", len(cc.causes))
	}
}

// A fixture line cannot be signed by someone whose grant we don't hold.
func TestTk_ImportRefusesSignerWithoutGrant(t *testing.T) {
	app, cc, out, env := newApp(t)
	bob := as(app, env.Token(t, "Bob", "Bob"))
	err := bob.Run(context.Background(), []string{"import", "../../fixtures/alphacorp"})
	if err == nil || !strings.Contains(err.Error(), `no grant for "CRO"`) {
		t.Fatalf("expected refusal naming CRO, got %v\n%s", err, out.String())
	}
	if len(cc.causes) != 0 {
		t.Fatalf("no cause may be issued without the signer's grant: %d", len(cc.causes))
	}
}

// A refused move leaves its cause without a row — reported honestly, and the
// ledger untouched.
func TestTk_RefusedMoveKeepsLedgerUntouched(t *testing.T) {
	app, cc, out, _ := newApp(t)
	ids := claimIDs(t, run(t, app, out, "import", "../../fixtures/alphacorp"))
	// reapprove requires a prior demote → 409.
	err := app.Run(context.Background(), []string{"reapprove", strconv.FormatInt(ids["905"], 10),
		"--reason", "x", "--basis-kind", "fact", "--note", "x"})
	if err == nil || !strings.Contains(err.Error(), "invalid_move_for_state") || !strings.Contains(err.Error(), "no row written") {
		t.Fatalf("expected a 409 with the cause named, got %v", err)
	}
	if _, _, rows := showEff(t, app, out, ids["905"]); rows != 1 {
		t.Fatalf("905 should still have one row, got %d", rows)
	}
	if len(cc.causes) != 3 {
		t.Fatalf("the cause was still issued before the refusal: %d", len(cc.causes))
	}
}

// A move without a grant fails before any cause is issued.
func TestTk_MoveWithoutGrantIssuesNoCause(t *testing.T) {
	app, cc, _, _ := newApp(t)
	anon := as(app, "")
	err := anon.Run(context.Background(), []string{"demote", "1", "--reason", "x"})
	if err == nil || !strings.Contains(err.Error(), "TK_TOKEN") {
		t.Fatalf("expected a no-grant error, got %v", err)
	}
	if len(cc.causes) != 0 {
		t.Fatalf("cause issued without a grant: %d", len(cc.causes))
	}
}

// keygen → issue → whoami → revoke, the dev stand-in for Ankayma's control plane.
func TestTk_TokenIssueWhoamiRevoke(t *testing.T) {
	app, _, out, env := newApp(t)
	dir := t.TempDir()

	// keygen writes a seed, prints a usable public key, and never overwrites.
	seedPath := filepath.Join(dir, "dev.seed")
	pub := strings.TrimSpace(run(t, app, out, "token", "keygen", "--out", seedPath))
	if _, err := grant.ParsePublicKeys(pub); err != nil {
		t.Fatalf("keygen public key: %v", err)
	}
	if err := app.Run(context.Background(), []string{"token", "keygen", "--out", seedPath}); err == nil {
		t.Fatal("keygen must refuse to overwrite a seed")
	}

	// issue with the key tk-api trusts (the test env's), then whoami.
	envSeed := filepath.Join(dir, "env.seed")
	if err := os.WriteFile(envSeed, []byte(grant.EncodeKey(env.Key.Seed())), 0o600); err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(run(t, app, out, "token", "issue", "--key", envSeed, "--user", "Bob", "--actor", "cursor", "--ttl", "1h"))
	bob := as(app, tok)
	var me grant.Grant
	if err := json.Unmarshal([]byte(run(t, bob, out, "whoami")), &me); err != nil {
		t.Fatal(err)
	}
	if me.UserID != "Bob" || me.ActorID != "cursor" || me.ExpiresAt == 0 || !me.Has(grant.ScopeWrite) {
		t.Fatalf("whoami: %+v", me)
	}
	run(t, bob, out, "propose", "before revoke")

	admin := as(app, env.Token(t, "CRO", "CRO", grant.ScopeAdmin))
	run(t, admin, out, "token", "revoke", me.GrantID, "--reason", "laptop lost")
	err := bob.Run(context.Background(), []string{"propose", "after revoke"})
	if err == nil || !strings.Contains(err.Error(), "grant_revoked") {
		t.Fatalf("revoked grant must be refused, got %v", err)
	}
}

func TestTk_Usage(t *testing.T) {
	app, _, out, _ := newApp(t)
	if err := app.Run(context.Background(), nil); !IsUsage(err) {
		t.Fatalf("no args: %v", err)
	}
	if err := app.Run(context.Background(), []string{"promote", "x"}); !IsUsage(err) {
		t.Fatalf("bad promote: %v", err)
	}
	if !strings.Contains(out.String(), "TK_TOKEN") || strings.Contains(out.String(), "--signer") {
		t.Fatalf("usage must name TK_TOKEN and no --signer:\n%s", out.String())
	}
}
