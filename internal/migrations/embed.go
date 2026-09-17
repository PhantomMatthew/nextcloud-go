package migrations

import "embed"

//go:embed sql/postgres/*.sql sql/mysql/*.sql sql/sqlite/*.sql
var sqlFS embed.FS
