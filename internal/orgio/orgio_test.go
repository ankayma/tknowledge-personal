package orgio_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/ankayma/tknowledge-personal/internal/grant"
	"github.com/ankayma/tknowledge-personal/internal/orgio"
	"github.com/ankayma/tknowledge-personal/internal/testapi"
	"github.com/ankayma/tknowledge-personal/internal/tkclient"
)

// export → import into a fresh store → export again gives the same dump;
// sequences move past the restored ids; import refuses a non-empty store, a
// store whose CDC slot exists, and a foreign file.
func TestExportImport_RoundTrip(t *testing.T) {
	ctx := context.Background()
	env := testapi.Start(t)
	tk := tkclient.New(env.Server.URL).WithToken(env.Token(t, "bao", "bao", grant.ScopeRead, grant.ScopeWrite, grant.ScopeAdmin))
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tk.OrgInit(ctx, "Bao", ""))
	must(tk.OrgConfigSet(ctx, "open.window_days", json.RawMessage("3")))
	must(tk.OrgNamespaceAdd(ctx, "vendor", "nhà cung cấp"))
	must(tk.Admit(ctx, []tkclient.AdmitItem{
		{Subject: "tracker:d4.band", Statement: "50–60k", Label: "A"},
		{Subject: "person:Paul Okafor.tier", Statement: "thesis-ally", Label: "A-b"},
	}))
	must(tk.Store(ctx, tkclient.StoreReq{Title: "rules", Kind: "rule", Body: "A1", Refs: []string{"Paul Okafor"}}))
	must(tk.Interact(ctx, tkclient.ConversationKey{Kind: "dm", Person: "Paul Okafor"}, []tkclient.EventReq{
		{Actor: "Paul Okafor", Type: "dm", Body: "hi"}, {Actor: "me", Type: "dm", Body: "hello", InReplyTo: "#0"},
	}, ""))
	must(tk.SetupApply(ctx, "demo", ""))

	raw, err := tk.OrgExport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var dump orgio.Bundle
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatal(err)
	}

	fresh, _, _ := testapi.StartPostgres(t)
	counts, err := orgio.Import(ctx, fresh, dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"ledger", "docs", "persons", "conversations", "events", "org", "org_config", "users"} {
		var rows []any
		_ = json.Unmarshal(dump.Tables[table], &rows)
		if int(counts[table]) != len(rows) || len(rows) == 0 {
			t.Errorf("%s: restored %d of %d", table, counts[table], len(rows))
		}
	}

	again, err := orgio.Export(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	// Compare values, not bytes: the dump went through tk-api's JSON encoder.
	// The migration's seed namespaces exist in both stores before import, so
	// their created_at is each store's own migration time.
	rowsOf := func(raw json.RawMessage, table string) []map[string]any {
		t.Helper()
		var rows []map[string]any
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if table == "namespaces" {
			for _, r := range rows {
				if r["added_by"] == nil {
					delete(r, "created_at")
				}
			}
		}
		return rows
	}
	for _, table := range orgio.Tables {
		if !reflect.DeepEqual(rowsOf(dump.Tables[table.Name], table.Name), rowsOf(again.Tables[table.Name], table.Name)) {
			t.Errorf("%s differs after the round trip:\n%s\n%s", table.Name, dump.Tables[table.Name], again.Tables[table.Name])
		}
	}

	var maxID, next int64
	if err := fresh.QueryRow(ctx, `SELECT max(id) FROM ledger`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	if err := fresh.QueryRow(ctx, `SELECT nextval('ledger_id_seq')`).Scan(&next); err != nil || next != maxID+1 {
		t.Fatalf("ledger sequence: next %d after max %d (%v)", next, maxID, err)
	}

	if _, err := orgio.Import(ctx, fresh, dump); !errors.Is(err, orgio.ErrNotEmpty) {
		t.Fatalf("second import: %v", err)
	}
	withSlot, _, _ := testapi.StartPostgres(t)
	if _, err := withSlot.Exec(ctx, `SELECT pg_create_physical_replication_slot('tk_slot')`); err != nil {
		t.Fatal(err)
	}
	if _, err := orgio.Import(ctx, withSlot, dump); !errors.Is(err, orgio.ErrSlotExists) {
		t.Fatalf("slot guard: %v", err)
	}
	if _, err := orgio.Import(ctx, fresh, orgio.Bundle{Format: "something/else"}); !errors.Is(err, orgio.ErrFormat) {
		t.Fatalf("format guard: %v", err)
	}
}
