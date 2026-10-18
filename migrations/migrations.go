// Package migrations embeds the SQL files in this directory so internal/migrate
// can apply them without a separate migration tool.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
