// Package migrations embeds the SQL migration files into the binary.
package migrations

import "embed"

// FS contains all *.up.sql migration files.
//
//go:embed *.up.sql
var FS embed.FS
