// Package migrate applies the SQL files embedded by migrations/, in filename order.
// No external migration tool: files are idempotent (DO blocks, IF NOT EXISTS),
// safe to re-run against a database that already has them applied.
package migrate

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ankayma/tknowledge-personal/migrations"
)

// Apply runs every embedded .sql file, in filename order, against pool.
// pool must be connected as a role with DDL privileges (e.g. the Postgres superuser).
func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return fmt.Errorf("migrate: read embedded dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migrate: apply %s: %w", name, err)
		}
	}
	return nil
}
