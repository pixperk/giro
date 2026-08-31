// Package giro embeds the sql migrations so the binary and its schema cannot
// drift apart.
//
// this lives at the repo root because //go:embed cannot reach outside its own
// package directory, and migrations belong at the top level rather than buried
// under cmd/.
package giro

import "embed"

// all: also matches dotfiles, which keeps an empty migrations dir embeddable.
//
//go:embed all:migrations
var MigrationsFS embed.FS

// where migrate new writes. the embedded copy is read only, so the generator
// needs the real path.
const MigrationsDir = "migrations"
