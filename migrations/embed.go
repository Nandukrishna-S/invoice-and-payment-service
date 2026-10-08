// Package migrations embeds the API's SQL migrations so they ship in the binary.
package migrations

import "embed"

// FS holds every entry in this directory; the pattern narrows to *.sql once the
// first migration lands.
//
//go:embed *
var FS embed.FS
