// Package psp embeds the mock PSP's migrations. They are applied by the mockpsp
// binary into its own schema, with their own bookkeeping table.
package psp

import "embed"

//go:embed *.sql
var FS embed.FS
