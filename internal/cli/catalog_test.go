package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// The catalog through tk-api and the CLI (owner 14/10/2026): anyone reads the
// approved entries without a grant; the maintainer proposes from a file and
// approves; a person takes an entry with a translation, or declines one.
func TestCatalog_PublicReadMaintainerAndAdopt(t *testing.T) {
	ctx := context.Background()
	env := testapi.Start(t)
	var out bytes.Buffer
	admin := &App{TK: tkclient.New(env.Server.URL).WithToken(env.Token(t, "owner", "tk-cli", grant.ScopeAdmin)), Out: &out}
	person := &App{TK: tkclient.New(env.Server.URL).WithToken(env.Token(t, "bao", "tk-cli")), Out: &out}
	run := func(app *App, args ...string) string {
		t.Helper()
		out.Reset()
		if err := app.Run(ctx, args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	type listOut struct {
		Entries []struct {
			ID         string  `json:"id"`
			Status     string  `json:"status"`
			ReviewNote *string `json:"review_note"`
		} `json:"entries"`
		Count int `json:"count"`
	}

	// No grant at all.
	resp, err := http.Get(env.Server.URL + "/v1/catalog")
	if err != nil {
		t.Fatal(err)
	}
	var pub listOut
	_ = json.NewDecoder(resp.Body).Decode(&pub)
	resp.Body.Close()
	// Public: only what passes the gate (A-157) — today the definitions.
	if resp.StatusCode != 200 || pub.Count != 12 || pub.Entries[0].ID != "BP-16" || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("public read: %d %+v", resp.StatusCode, pub)
	}
	if resp, _ := http.Get(env.Server.URL + "/v1/catalog?status=proposed"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("proposals are not public: %d", resp.StatusCode)
	}

	file := filepath.Join(t.TempDir(), "bp.json")
	_ = os.WriteFile(file, []byte(`[{"text": "When asked to choose between options, choose one and say why; never hand back a menu.",
		"tags": {"who": ["agent"], "what": ["analysis"], "when": ["before-analysis"], "where": ["chat"], "why": "a menu hands the decision back"},
		"evidence": "A person asked for a choice and got three options back, twice in one session.",
		"review_note": "rewritten from a private directive"}]`), 0o600)
	if o := run(admin, "catalog", "add", file); !strings.Contains(o, "proposed 1: BP-28") {
		t.Fatalf("add: %s", o)
	}
	out.Reset()
	if err := person.Run(ctx, []string{"catalog", "add", file}); err == nil {
		t.Fatal("a person's grant cannot propose")
	}
	var mine listOut
	_ = json.Unmarshal([]byte(run(admin, "catalog", "list", "--status", "proposed")), &mine)
	if mine.Count != 1 || mine.Entries[0].ReviewNote == nil {
		t.Fatalf("the maintainer's read: %+v", mine)
	}
	out.Reset()
	if err := admin.Run(ctx, []string{"catalog", "approve", "BP-28"}); err == nil || !strings.Contains(err.Error(), "CATALOG_GATE") {
		t.Fatalf("approve without source and date: %v", err)
	}
	approved := run(admin, "catalog", "approve", "BP-28", "--note", "anonymised", "--source", "a founder's store, UAE", "--on", "2026-10-13")
	if !strings.Contains(approved, `"public": true`) || !strings.Contains(approved, `"incident_on": "2026-10-13"`) {
		t.Fatalf("approve: %s", approved)
	}

	adopted := run(person, "instructions", "adopt", "BP-28", "--translation", "Khi được hỏi chọn, chọn một và nói vì sao.", "--lang", "vi")
	if !strings.Contains(adopted, `"catalog_ref": "BP-28"`) || !strings.Contains(adopted, `"lang": "vi"`) {
		t.Fatalf("adopt: %s", adopted)
	}
	declined := run(person, "instructions", "decline", "BP-15", "--reason", "not needed in this store")
	if !strings.Contains(declined, "declined: not needed") && !strings.Contains(declined, `"status": "retired"`) {
		t.Fatalf("decline: %s", declined)
	}
	if o := run(admin, "catalog", "withdraw", "BP-28", "--reason", "merged"); !strings.Contains(o, `"status": "withdrawn"`) {
		t.Fatalf("withdraw: %s", o)
	}
}
