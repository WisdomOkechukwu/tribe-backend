// Package migrations holds the SQL files that build the database.
//
// Each file has an "Up" part (create things) and a "Down" part (undo them),
// just like a Laravel migration's up() and down() methods. Files run in
// number order and each runs only once.
package migrations

import "embed"

// Files contains every .sql file in this folder, built into the program so
// the server binary can migrate without the source code next to it.
//
//go:embed *.sql
var Files embed.FS
