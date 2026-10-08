// Package migrations embeds the API's SQL migrations so they ship in the binary.
package migrations

import "embed"

// FS holds the API's SQL migrations; the mock PSP's live in the psp subdirectory.
//
//go:embed *.sql
var FS embed.FS
