// Package migrations embeds the schema migrations, so the migrate command
// carries them and runs anywhere DATABASE_URL reaches.
package migrations

import "embed"

// Up holds the forward migrations, applied in file-name order.
//
//go:embed *.sql
var Up embed.FS

// Down holds the rollbacks that exist, by the same file name as the
// migration they undo. Only the newest release's migrations have one; an
// older schema is restored from a backup instead.
//
//go:embed down/*.sql
var Down embed.FS
