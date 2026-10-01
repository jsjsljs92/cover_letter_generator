// Package migrations embeds the SQL schema migrations so the binary is
// self-contained: there is no separate migration step to run before starting
// the server.
package migrations

import "embed"

// FS holds the ordered .sql migration files.
//
//go:embed *.sql
var FS embed.FS
