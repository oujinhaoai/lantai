package commands

import (
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

// SchemaSQL 创建的表必须符合所有权登记，且只允许在业务库出现。
func TestSchemaMatchesOwnershipRegistry(t *testing.T) {
	ctx := t.Context()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) == 0 {
		t.Fatal("no tables created")
	}
	for _, name := range tables {
		for _, d := range []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime} {
			if _, err := ownership.CheckTable(d, name); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		if name != "sqlite_sequence" {
			if _, err := ownership.CheckTable(ownership.Events, name); err == nil {
				t.Errorf("%s must not be allowed in events.db", name)
			}
		}
	}
}
