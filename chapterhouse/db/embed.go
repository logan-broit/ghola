// Package db embeds the admin schema (users, API keys, admin sessions,
// audit log) so ch-init can apply it. The same files are sqlc's schema input.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
