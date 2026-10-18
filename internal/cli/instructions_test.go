package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// tk instructions: the full list, the index an agent reads, help for one
// situation, and tags applied from a file line by line (owner 13/10/2026).
func TestInstructions_IndexHelpTagFile(t *testing.T) {
	ctx := context.Background()
	env := testapi.Start(t)
	var out bytes.Buffer
	app := &App{TK: tkclient.New(env.Server.URL).WithToken(env.Token(t, "bao", "tk-cli")), Out: &out}
	run := func(args ...string) map[string]any {
		t.Helper()
		out.Reset()
		if err := app.Run(ctx, args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		var m map[string]any
		_ = json.Unmarshal(out.Bytes(), &m)
		return m
	}
	if full := run("instructions"); full["directives"] == nil || full["index"] != nil {
		t.Fatalf("full list: %v", full)
	}
	if idx := run("instructions", "--index"); idx["index"] == nil || idx["directives"] != nil {
		t.Fatalf("index: %v", idx)
	}
	file := filepath.Join(t.TempDir(), "tags.json")
	if err := os.WriteFile(file, []byte(`[{"id":"12","what":["document"],"when":["before-write"],"why":"unbound is valid"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := app.Run(ctx, []string{"instructions", "tag", file}); err != nil || !strings.Contains(out.String(), "tagged 1") {
		t.Fatalf("tag file: %v %s", err, out.String())
	}
	got := run("instructions", "help", "record", "a", "screenshot")
	if ds, _ := got["directives"].([]any); len(ds) == 0 || !strings.Contains(out.String(), `"label": "12"`) {
		t.Fatalf("help after tagging: %s", out.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`[{"id":"12","when":["someday"]}]`), 0o600)
	if err := app.Run(ctx, []string{"instructions", "tag", bad}); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("a bad line is named: %v", err)
	}
}
