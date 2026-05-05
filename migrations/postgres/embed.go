// Package pgmigrations embeds the postgres migration SQL files.
package pgmigrations

import "embed"

//go:embed *.sql
var FS embed.FS
