package migrations

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/oujinhaoai/lantai/internal/contract/ownership"
)

func TestRegistry(t *testing.T) {
	for _, db := range Databases {
		list := For(db)
		if Latest(db) != len(list) {
			t.Fatalf("%s: Latest %d, %d migrations", db, Latest(db), len(list))
		}
		for i, m := range list {
			if m.Version != i+1 || m.DB != db {
				t.Fatalf("%s: bad entry %+v", db, m)
			}
			if strings.Contains(m.SQL, "\r") {
				t.Fatalf("%s: CRLF not normalized", m.ID())
			}
			if !m.Checksum().Valid() {
				t.Fatalf("%s: checksum", m.ID())
			}
		}
	}
	main := For(ownership.Main)
	if len(main) < 2 || main[0].Owner != "commands" || main[1].Owner != "identity" {
		t.Fatalf("main migrations: %+v", main)
	}
	// 返回副本，调用方修改不影响登记。
	main[0].SQL = "x"
	if For(ownership.Main)[0].SQL == "x" {
		t.Fatal("For must return a copy")
	}
}

func TestLoadRejectsBadLayouts(t *testing.T) {
	ok := "CREATE TABLE identity_x (id TEXT PRIMARY KEY) STRICT;"
	cases := map[string]fstest.MapFS{
		"bad name":      {"sql/main/2.identity.x.sql": {Data: []byte(ok)}},
		"gap":           {"sql/main/0003.identity.x.sql": {Data: []byte(ok)}},
		"unknown owner": {"sql/main/0002.nobody.x.sql": {Data: []byte(ok)}},
		"unknown db":    {"sql/other/0001.identity.x.sql": {Data: []byte(ok)}},
		"nested":        {"sql/main/sub/0002.identity.x.sql": {Data: []byte(ok)}},
		"empty":         {"sql/main/0002.identity.x.sql": {Data: []byte("  \n")}},
		"duplicate":     {"sql/main/0002.identity.x.sql": {Data: []byte(ok)}, "sql/main/0002.identity.y.sql": {Data: []byte(ok)}},
	}
	for name, fsys := range cases {
		if _, err := load(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := fstest.MapFS{
		"sql/main/0002.identity.x.sql":     {Data: []byte("CREATE TABLE identity_x (id TEXT)\r\n")},
		"sql/main/0003.identity.x.sql":     {Data: []byte(ok)},
		"sql/ledger/0002.ledger.x.sql":     {Data: []byte(ok)},
		"sql/ledger/0003.provenance.x.sql": {Data: []byte(ok)},
		"sql/runtime/0002.identity.x.sql":  {Data: []byte(ok)},
		"sql/runtime/0003.storage.x.sql":   {Data: []byte(ok)},
		"sql/index/0001.query.x.sql":       {Data: []byte(ok)},
	}
	r, err := load(good)
	if err != nil {
		t.Fatal(err)
	}
	if got := r[ownership.Main][1].SQL; got != "CREATE TABLE identity_x (id TEXT)\n" {
		t.Fatalf("CRLF must be normalized, got %q", got)
	}
}
