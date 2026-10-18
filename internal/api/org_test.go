package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/setup"
)

func getAs[T any](t *testing.T, url, token string) (int, T) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, decode[T](t, resp)
}

// spec §5.17: org.init (free license, once), config as an append-only log
// that the store obeys, the namespace catalogue, export — all tk:admin.
func TestOrg_InitConfigNamespacesExport(t *testing.T) {
	env := newTestServer(t)
	ctx := context.Background()
	admin := env.token(t, "bao", "bao", grant.ScopeRead, grant.ScopeWrite, grant.ScopeAdmin)
	writer := env.token(t, "bao", "claude-desktop")

	expectErr(t, "init without tk:admin", postJSON(t, env.URL+"/v1/org/init", writer, map[string]string{"name": "Bao"}), 403, "insufficient_scope")
	expectErr(t, "init without name", postJSON(t, env.URL+"/v1/org/init", admin, map[string]string{}), 400, "empty_name")
	expectErr(t, "paid license", postJSON(t, env.URL+"/v1/org/init", admin, map[string]string{"name": "Bao", "license_key": "lic_x"}), 501, "not_implemented")
	resp := postJSON(t, env.URL+"/v1/org/init", admin, map[string]string{"name": "Bao"})
	org := decode[struct {
		OrgID       string `json:"org_id"`
		AdminUserID string `json:"admin_user_id"`
		License     struct {
			Plan string `json:"plan"`
		} `json:"license"`
	}](t, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(org.OrgID, "org_") || org.AdminUserID != "bao" || org.License.Plan != "free" {
		t.Fatalf("init: %d %+v", resp.StatusCode, org)
	}
	expectErr(t, "init twice", postJSON(t, env.URL+"/v1/org/init", admin, map[string]string{"name": "Bao"}), 409, "already_initialized")
	if _, st := getAs[setupStatus](t, env.URL+"/v1/setup/status", writer); !st.UserRegistered {
		t.Fatal("the admin is a registered user after init")
	}

	_, cfg := getAs[map[string]any](t, env.URL+"/v1/org/config", admin)
	if cfg["open.window_days"] != float64(7) || cfg["license.grace_days"] != float64(30) || cfg["namespaces.closed"] != true {
		t.Fatalf("defaults: %v", cfg)
	}
	set := func(key string, value any) *http.Response {
		t.Helper()
		return postJSON(t, env.URL+"/v1/org/config", admin, map[string]any{"key": key, "value": value})
	}
	expectErr(t, "unknown key", set("open.days", 3), 400, "unknown_config_key")
	expectErr(t, "not an int", set("open.window_days", "x"), 400, "bad_config_value")
	expectErr(t, "out of range", set("open.window_days", 0), 400, "bad_config_value")
	expectErr(t, "config without tk:admin", postJSON(t, env.URL+"/v1/org/config", writer, map[string]any{"key": "open.window_days", "value": 3}), 403, "insufficient_scope")

	// open() obeys open.window_days.
	if _, err := env.Admin.Exec(ctx, `
		WITH n AS (SELECT nextval('ledger_id_seq') AS id)
		INSERT INTO ledger (id, claim_id, statement, mark, producer, move, subject, ts)
		SELECT n.id, n.id, 'five days old', 'A', 'test', 'propose', 'tracker:w.old', now() - interval '5 days' FROM n`); err != nil {
		t.Fatal(err)
	}
	openCount := func() int {
		t.Helper()
		_, o := getJSON[openResp](t, env, env.URL+"/v1/open")
		return o.Count
	}
	if openCount() != 1 {
		t.Fatal("5 days old is inside the default 7-day window")
	}
	if r := set("open.window_days", 3); r.StatusCode != 200 {
		t.Fatalf("set window: %d", r.StatusCode)
	}
	if openCount() != 0 {
		t.Fatal("5 days old is outside a 3-day window")
	}

	// The namespace catalogue.
	addNS := func(name, desc string) *http.Response {
		return postJSON(t, env.URL+"/v1/org/namespaces", admin, map[string]string{"name": name, "description": desc})
	}
	for i, want := range []string{"new", "duplicate"} {
		resp := addNS("Vendor", "nhà cung cấp")
		if out := decode[map[string]string](t, resp); out["outcome"] != want || out["name"] != "vendor" {
			t.Fatalf("add #%d: %v", i, out)
		}
	}
	expectErr(t, "bad name", addNS("Bad Name!", "x"), 400, "invalid_namespace")
	expectErr(t, "no description", addNS("misc2", " "), 400, "empty_description")
	if r := admit(t, env, writer, admitItem{Subject: "vendor:acme.status", Statement: "signed", Label: "A"})[0]; r.Outcome != "new" {
		t.Fatalf("admit on an added namespace: %+v", r)
	}

	// namespaces.closed = false lets admit add a namespace; a ref never adds one.
	if r := admit(t, env, writer, admitItem{Subject: "misc:x.y", Statement: "z", Label: "A"})[0]; r.Error == nil || r.Error.Code != "UNKNOWN_NAMESPACE" {
		t.Fatalf("closed catalogue: %+v", r)
	}
	set("namespaces.closed", false)
	if r := admit(t, env, writer, admitItem{Subject: "misc:x.y", Statement: "z", Label: "A"})[0]; r.Outcome != "new" {
		t.Fatalf("open catalogue: %+v", r)
	}
	if out := store(t, env, writer, storeReq{Title: "refs-open", Kind: "plan", Body: "x", Refs: []string{"other:a.b"}}); out.Outcome != "new" {
		t.Fatalf("open catalogue accepts a ref: %+v", out)
	}
	_, list := getAs[[]map[string]any](t, env.URL+"/v1/org/namespaces", admin)
	names := []string{}
	for _, ns := range list {
		names = append(names, ns["name"].(string))
	}
	if !slices.Contains(names, "misc") || !slices.Contains(names, "vendor") || slices.Contains(names, "other") {
		t.Fatalf("catalogue: %v", names)
	}

	// Export: every table, admin only.
	code, dump := getAs[struct {
		Format string                     `json:"format"`
		Tables map[string]json.RawMessage `json:"tables"`
	}](t, env.URL+"/v1/org/export", admin)
	var ledgerRows, orgRows []any
	_ = json.Unmarshal(dump.Tables["ledger"], &ledgerRows)
	_ = json.Unmarshal(dump.Tables["org"], &orgRows)
	if code != 200 || dump.Format != "tk-export/1" || len(dump.Tables) != 14 || len(ledgerRows) != count(t, env, "ledger") || len(orgRows) != 1 {
		t.Fatalf("export: %d %s tables=%d ledger=%d org=%d", code, dump.Format, len(dump.Tables), len(ledgerRows), len(orgRows))
	}
	if code, _ := getAs[apiErr](t, env.URL+"/v1/org/export", writer); code != 403 {
		t.Fatalf("export without tk:admin: %d", code)
	}

	for _, sql := range []string{`UPDATE org_config SET value = '1'`, `DELETE FROM namespaces WHERE name = 'misc'`} {
		_, err := env.Admin.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: want insufficient_privilege, got %v", sql, err)
		}
	}
}

// The store as a template: a user fills the store through setup and MCP, then
// the admin exports it for other users — namespaces added after the seed and
// the latest rule documents, loadable as a complete template.
func TestOrg_TemplateFromStore(t *testing.T) {
	env := newTestServer(t)
	admin := env.token(t, "bao", "bao", grant.ScopeRead, grant.ScopeWrite, grant.ScopeAdmin)
	writer := env.token(t, "bao", "claude-desktop")

	apply(t, env, writer, map[string]any{"template_id": "incomplete"})
	store := func(title, kind, body string) {
		t.Helper()
		if resp := postJSON(t, env.URL+"/v1/docs", writer, map[string]any{"title": title, "kind": kind, "body": body}); resp.StatusCode != 200 {
			t.Fatalf("store %s: %d", title, resp.StatusCode)
		}
	}
	store("missing-rules", "rule", "# Rules\n\nA1. v1\n")
	store("Missing Rules", "rule", "# Rules\n\nA1. v2\n")
	store("D4 plan", "plan", "not a rule")
	store("principle-list", "rule", "# Principle List\n\n1. one\n2. two\n")

	type tplOut struct {
		ID      string            `json:"id"`
		Version int               `json:"version"`
		Files   map[string]string `json:"files"`
	}
	code, out := getAs[tplOut](t, env.URL+"/v1/org/template?id=starter&description=from+my+store", admin)
	if code != 200 || out.ID != "starter" || out.Version != 1 || len(out.Files) != 3 {
		t.Fatalf("template: %d %+v", code, out)
	}
	fsys := fstest.MapFS{}
	for name, body := range out.Files {
		fsys["starter/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	reg, err := setup.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	tpl, _ := reg.Get("starter")
	docs := tpl.Docs()
	if len(tpl.Missing) != 0 || tpl.Name != "starter" || tpl.Description != "from my store" ||
		len(tpl.Namespaces) != 1 || tpl.Namespaces[0].Name != "never-added" ||
		len(docs) != 2 || docs[0].Title != "Missing Rules" || !strings.Contains(docs[0].Body, "A1. v2") ||
		docs[1].Title != "principle-list" || tpl.PrinciplesCount() != 2 {
		t.Fatalf("loaded template: %+v", tpl)
	}

	if _, again := getAs[tplOut](t, env.URL+"/v1/org/template?id=demo", admin); again.Version != 2 {
		t.Fatalf("an existing id gets the next version: %d", again.Version)
	}
	expectErr(t, "bad id", getRaw(t, env.URL+"/v1/org/template?id=../x", admin), 400, "invalid_template_id")
	expectErr(t, "no tk:admin", getRaw(t, env.URL+"/v1/org/template?id=starter", writer), 403, "insufficient_scope")
}

func getRaw(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
