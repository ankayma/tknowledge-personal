package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/setup"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
)

func TestTk_OrgCommands(t *testing.T) {
	app, _, out, env := newApp(t)
	admin := as(app, env.Token(t, "CRO", "CRO", grant.ScopeRead, grant.ScopeWrite, grant.ScopeAdmin))

	if s := run(t, admin, out, "org", "init", "--name", "AlphaCorp"); !strings.Contains(s, `"org_id": "org_`) {
		t.Fatalf("init: %s", s)
	}
	run(t, admin, out, "org", "config", "set", "open.window_days", "3")
	if s := strings.TrimSpace(run(t, admin, out, "org", "config", "get", "open.window_days")); s != "3" {
		t.Fatalf("config get: %q", s)
	}
	run(t, admin, out, "org", "namespaces", "add", "vendor", "--description", "nhà cung cấp")
	if s := run(t, admin, out, "org", "namespaces", "list"); !strings.Contains(s, `"vendor"`) {
		t.Fatalf("namespaces: %s", s)
	}

	tplDir := t.TempDir()
	if s := run(t, admin, out, "org", "template", "export", "--id", "alpha", "--name", "AlphaCorp", "--out", tplDir); !strings.Contains(s, "template alpha@1") {
		t.Fatalf("template export: %s", s)
	}
	reg, err := setup.Load(os.DirFS(tplDir))
	if err != nil {
		t.Fatal(err)
	}
	if alpha, ok := reg.Get("alpha"); !ok || alpha.Name != "AlphaCorp" || len(alpha.Namespaces) != 1 || alpha.Namespaces[0].Name != "vendor" || len(alpha.Missing) != 0 {
		t.Fatalf("exported template: %+v", alpha)
	}

	dump := filepath.Join(t.TempDir(), "dump.json")
	run(t, admin, out, "org", "export", "--out", dump)

	fresh, freshDSN, _ := testapi.StartPostgres(t)
	importer := *admin
	importer.AdminDSN = freshDSN
	if s := run(t, &importer, out, "org", "import", "--file", dump); !regexp.MustCompile(`(?m)^org\s+1$`).MatchString(s) {
		t.Fatalf("import: %s", s)
	}
	var cfgRows int
	if err := fresh.QueryRow(context.Background(), `SELECT count(*) FROM org_config`).Scan(&cfgRows); err != nil || cfgRows != 1 {
		t.Fatalf("restored config rows: %d %v", cfgRows, err)
	}

	noDSN := *admin
	noDSN.AdminDSN = ""
	if err := noDSN.Run(context.Background(), []string{"org", "import", "--file", dump}); err == nil || !strings.Contains(err.Error(), "TK_ADMIN_DSN") {
		t.Fatalf("import without TK_ADMIN_DSN: %v", err)
	}
	if err := app.Run(context.Background(), []string{"org", "init", "--name", "x"}); err == nil || !strings.Contains(err.Error(), "insufficient_scope") {
		t.Fatalf("init without tk:admin: %v", err)
	}
}
