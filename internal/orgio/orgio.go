// Package orgio is org.export / org.import: a JSON dump of
// every table of the store, and its restore into an empty store.
//
// Import preserves ids, so it must run before tk-cdc creates its replication
// slot: rows inserted while the slot exists reach tk:ledger without causes and
// CJCM would rightly flag every one. Import refuses when the slot exists or
// the store holds data, rather than half-restore.
package orgio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Format names the dump layout.
const Format = "tk-export/1"

// Tables in restore order (referenced before referencing), with the column
// that orders the dump.
var Tables = []struct{ Name, Order, Seq string }{
	{"namespaces", "name", ""},
	{"users", "id", ""},
	{"org", "org_id", ""},
	{"teams", "id", "teams_id_seq"},
	{"team_members", "team_id, user_id", ""},
	{"org_config", "id", "org_config_id_seq"},
	{"persons", "id", "persons_id_seq"},
	{"conversations", "id", "conversations_id_seq"},
	{"events", "id", "events_id_seq"},
	{"docs", "id", "docs_id_seq"},
	{"ledger", "id", "ledger_id_seq"},
	{"flags", "id", "flags_id_seq"},
	{"violations_seen", "row_id", ""},
	{"revoked_grants", "grant_id", ""},
}

type Bundle struct {
	Format     string                     `json:"format"`
	ExportedAt time.Time                  `json:"exported_at"`
	Tables     map[string]json.RawMessage `json:"tables"`
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Export dumps every table as a JSON array of rows.
func Export(ctx context.Context, q querier) (Bundle, error) {
	b := Bundle{Format: Format, ExportedAt: time.Now().UTC(), Tables: map[string]json.RawMessage{}}
	for _, t := range Tables {
		var raw []byte
		if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(json_agg(t ORDER BY %s), '[]'::json) FROM %s t`, t.Order, t.Name)).Scan(&raw); err != nil {
			return Bundle{}, fmt.Errorf("export %s: %w", t.Name, err)
		}
		b.Tables[t.Name] = raw
	}
	return b, nil
}

var (
	ErrFormat     = errors.New("orgio: not a " + Format + " dump")
	ErrSlotExists = errors.New("orgio: replication slot tk_slot exists — import before tk-cdc first starts, or imported rows reach CJCM without causes")
	ErrNotEmpty   = errors.New("orgio: the store is not empty — import restores into an empty store only")
)

// Import restores b into the store behind pool (a superuser pool, migrations
// applied), in one transaction, and moves every sequence past the restored
// ids. Returns rows restored per table.
func Import(ctx context.Context, pool *pgxpool.Pool, b Bundle) (map[string]int64, error) {
	if b.Format != Format {
		return nil, ErrFormat
	}
	var slot bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name = 'tk_slot')`).Scan(&slot); err != nil {
		return nil, err
	}
	if slot {
		return nil, ErrSlotExists
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var rows int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM users) + (SELECT count(*) FROM org) + (SELECT count(*) FROM teams) +
		(SELECT count(*) FROM org_config) + (SELECT count(*) FROM persons) + (SELECT count(*) FROM conversations) +
		(SELECT count(*) FROM events) + (SELECT count(*) FROM docs) + (SELECT count(*) FROM ledger) +
		(SELECT count(*) FROM flags) + (SELECT count(*) FROM revoked_grants)`).Scan(&rows); err != nil {
		return nil, err
	}
	if rows != 0 {
		return nil, ErrNotEmpty
	}

	if err := upgradeBundle(&b); err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for _, t := range Tables {
		raw := b.Tables[t.Name]
		if len(raw) == 0 {
			raw = json.RawMessage("[]")
		}
		sql := fmt.Sprintf(`INSERT INTO %[1]s SELECT * FROM json_populate_recordset(NULL::%[1]s, $1::json)`, t.Name)
		if t.Name == "namespaces" {
			sql += ` ON CONFLICT (name) DO NOTHING` // the migration seeds the default catalogue
		}
		tag, err := tx.Exec(ctx, sql, string(raw))
		if err != nil {
			return nil, fmt.Errorf("import %s: %w", t.Name, err)
		}
		counts[t.Name] = tag.RowsAffected()
	}
	for _, t := range Tables {
		if t.Seq == "" {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`SELECT setval('%s', coalesce((SELECT max(id) FROM %s), 0) + 1, false)`, t.Seq, t.Name)); err != nil {
			return nil, fmt.Errorf("sequence %s: %w", t.Seq, err)
		}
	}
	return counts, tx.Commit(ctx)
}

// upgradeBundle reads a dump made before 1/10/2026, when a conversation was
// called a thread and people had no store: "threads" becomes "conversations",
// owner_of_thread and thread_id take their new names, and every row of the
// three tables goes to the default store. A current dump passes unchanged.
func upgradeBundle(b *Bundle) error {
	if _, ok := b.Tables["conversations"]; !ok {
		if raw, ok := b.Tables["threads"]; ok {
			b.Tables["conversations"] = raw
		}
	}
	renames := map[string][2]string{
		"conversations": {"owner_of_thread", "owner_of_conversation"},
		"events":        {"thread_id", "conversation_id"},
	}
	for _, table := range []string{"persons", "conversations", "events"} {
		raw, ok := b.Tables[table]
		if !ok || len(raw) == 0 {
			continue
		}
		var rows []map[string]any
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("import %s: %w", table, err)
		}
		for _, row := range rows {
			if r, ok := renames[table]; ok {
				if v, had := row[r[0]]; had {
					row[r[1]] = v
					delete(row, r[0])
				}
			}
			if _, had := row["tenant_id"]; !had {
				row["tenant_id"] = "default"
			}
		}
		fixed, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		b.Tables[table] = fixed
	}
	return nil
}
