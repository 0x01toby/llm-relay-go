// Package migrations embeds the SQL migration files so the binary is
// self-contained (no external filesystem needed at runtime). golang-migrate's
// iofs source reads from this FS.
package migrations

import "embed"

// FS holds all *.up.sql migration files. The migration runner treats this as
// the migration source.
//
//go:embed *.up.sql
var FS embed.FS
