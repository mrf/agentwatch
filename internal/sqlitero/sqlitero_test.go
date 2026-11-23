package sqlitero

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDSN_EscapesURISyntax(t *testing.T) {
	got := DSN("/tmp/a?b#c%d/x.db")
	if !strings.HasPrefix(got, "file:/tmp/a%3Fb%23c%25d/x.db?") {
		t.Errorf("DSN did not escape path: %q", got)
	}
	if !strings.Contains(got, "mode=ro") {
		t.Errorf("DSN missing mode=ro: %q", got)
	}
}

func TestOpen_ReadsUncheckpointedWAL(t *testing.T) {
	name := "odd?#%dir.db"
	if runtime.GOOS == "windows" {
		name = "odd#%dir.db" // '?' is not a legal Windows filename character
	}
	path := filepath.Join(t.TempDir(), name)
	w, err := sql.Open("sqlite", strings.Replace(DSN(path), "mode=ro", "mode=rwc", 1))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() { _ = w.Close() }()
	w.SetMaxOpenConns(1)
	stmts := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE t (v integer)",
		"INSERT INTO t VALUES (42)",
	}
	for i := 0; i < len(stmts); i++ {
		if _, err := w.Exec(stmts[i]); err != nil {
			t.Fatalf("%s: %v", stmts[i], err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("writer did not create %q: %v", path, err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRowContext(context.Background(), "SELECT v FROM t").Scan(&v); err != nil {
		t.Fatalf("query: %v", err)
	}
	if v != 42 {
		t.Errorf("v = %d, want 42", v)
	}
	if _, err := db.Exec("INSERT INTO t VALUES (1)"); err == nil {
		t.Error("write through read-only handle succeeded")
	}
}
