// Package sqlitero opens SQLite databases owned by other processes for
// read-only access, including databases in WAL mode with live writers.
package sqlitero

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DSN builds a read-only SQLite URI for path. The path is URL-escaped so that
// '?', '#' and '%' in it are not read as URI syntax.
func DSN(path string) string {
	p := filepath.ToSlash(path)
	if len(p) > 1 && p[1] == ':' {
		p = "/" + p // Windows drive letter
	}
	u := url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     p,
		RawQuery: url.Values{
			"mode":    {"ro"},
			"_pragma": {"busy_timeout(5000)"},
		}.Encode(),
	}
	return u.String()
}

// Open returns a read-only handle on path limited to one connection. The
// database is not touched until the first query.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("sqlitero: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
