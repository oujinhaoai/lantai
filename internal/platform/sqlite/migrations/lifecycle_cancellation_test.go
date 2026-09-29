package migrations

import (
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

func TestLifecycleCancellationMigrationPreservesOldIntents(t *testing.T) {
	ctx := t.Context()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list := For(ownership.Ledger)
	for _, m := range list {
		if m.Version >= 12 {
			break
		}
		if _, err = db.ExecContext(ctx, m.SQL); err != nil {
			t.Fatal(m.ID(), err)
		}
	}
	for _, state := range []string{"pending", "trashed", "restored", "purged"} {
		if _, err = db.ExecContext(ctx, `INSERT INTO ledger_trash_entries VALUES(?, 'project','asset',?,3,123,1,'{"synthetic":"immutable history"}')`, state, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO ledger_lifecycle_ops VALUES('op','pending','trash','{}','{}','accepted')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, list[11].SQL); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_trash_entries WHERE revision=3 AND due_at=123 AND hold=1 AND record='{"synthetic":"immutable history"}'`).Scan(&n); err != nil || n != 4 {
		t.Fatal("migration changed retained history", n, err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE ledger_lifecycle_ops SET state='cancelled' WHERE operation_id='op'`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE ledger_trash_entries SET state='cancelled' WHERE trash_id='pending'`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE ledger_trash_entries SET state='unsupported' WHERE trash_id='pending'`); err == nil {
		t.Fatal("state constraint lost")
	}
}
