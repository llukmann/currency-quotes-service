// Package migrations embeds the SQL schema migrations into the binary, so that
// the runtime image needs no .sql files of its own.
//
// The declaration lives next to the SQL rather than in internal/storage/postgres
// because //go:embed cannot reach outside its own directory.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
