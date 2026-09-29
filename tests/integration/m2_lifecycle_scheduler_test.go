package integration

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// The T08 scheduler drives the real T03 lifecycle and T02 bytes: T03 decides
// whether a due purge is allowed, T02 collects only after the 24-hour wait and
// a full authoritative root and pin check. Content shared with a surviving
// version stays; restart keeps the same job registrations.
func TestM2LifecycleSchedulerDuePurgeAndGC(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	app := reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	ctx := t.Context()
	who := e.login().Context
	unique := []byte("scheduler unique bytes")
	shared := []byte("scheduler shared bytes")
	a := e.ingest(who, "sched-unique", unique, *rightsOwned())
	b := e.ingest(who, "sched-shared", shared, *rightsOwned())
	survivor := e.ingest(who, "sched-survivor", shared, *rightsOwned())
	entryA := trashForTest(t, e, app.Lifecycle, who, a.AssetID)
	entryB := trashForTest(t, e, app.Lifecycle, who, b.AssetID)
	// The later of the two boundaries makes both entries due together.
	due, err := clock.Parse(max(entryA.DueAt, entryB.DueAt))
	if err != nil {
		t.Fatal(err)
	}
	run := func() map[string]any {
		t.Helper()
		r, err := app.RunLifecycle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		raw, _ := json.Marshal(r)
		_ = json.Unmarshal(raw, &out)
		return out
	}
	if r := run(); r["reminders"].(float64) != 0 || r["purges"].(float64) != 0 {
		t.Fatal("nothing is due yet", r)
	}
	e.clk.Set(due.Add(-72 * time.Hour))
	if r := run(); r["reminders"].(float64) != 2 {
		t.Fatal(r)
	}
	var reminders int
	if err = e.inst.DB(ownership.Ledger).QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE event_type='trash.purge_due'`).Scan(&reminders); err != nil || reminders != 2 {
		t.Fatal(reminders, err)
	}
	// The project disables automatic purge: T03 refuses, the job waits.
	off, _ := json.Marshal(false)
	e.sudo(&identity.SetPolicy{ProjectID: e.project.ProjectID, Key: "trash.auto_purge", Value: off})
	e.clk.Set(due)
	if r := run(); r["purges"].(float64) != 0 || r["deferred"].(float64) != 2 {
		t.Fatal("auto purge disabled by policy", r)
	}
	if current, err := app.Lifecycle.Entry(ctx, e.login().Context, e.project.ProjectID, entryA.ID); err != nil || current.State != "trashed" {
		t.Fatal("purged despite policy", current.State, err)
	}
	on, _ := json.Marshal(true)
	e.sudo(&identity.SetPolicy{ProjectID: e.project.ProjectID, Key: "trash.auto_purge", Value: on, ExpectedRevision: 1})
	e.clk.Advance(time.Hour)
	if r := run(); r["purges"].(float64) != 2 {
		t.Fatal(r)
	}
	if current, err := app.Lifecycle.Entry(ctx, e.login().Context, e.project.ProjectID, entryA.ID); err != nil || current.State != "purged" {
		t.Fatal(current.State, err)
	}
	if err = e.ledger.CheckVersionRead(ctx, a.AssetID, a.VersionID); errcode.CodeOf(err) != errcode.AssetPurged {
		t.Fatal(err)
	}
	blob := func(data []byte) bool {
		_, err := os.Stat(app.Storage.Layout().BlobPath(shaOf(data)))
		return err == nil
	}
	// Candidates wait at least 24 hours after the purge.
	if r := run(); r["collected"].(float64) != 0 || !blob(unique) {
		t.Fatal("collected before the 24-hour wait", r)
	}
	e.clk.Advance(24 * time.Hour)
	r := run()
	if r["collected"].(float64) != 1 || blob(unique) || !blob(shared) {
		t.Fatal("GC must remove only unreferenced content", r, blob(unique), blob(shared))
	}
	if r["retained"].(float64) != 1 {
		t.Fatal("shared content must be retained for the survivor", r)
	}
	v, err := e.ledger.Version(ctx, survivor.AssetID, survivor.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.storage.ReadManifest(ctx, v); err != nil {
		t.Fatal(err)
	}
	// Re-running is idempotent and does not widen the collection.
	if r = run(); r["collected"].(float64) != 0 || r["purges"].(float64) != 0 || !blob(shared) {
		t.Fatal(r)
	}
	var jobs int
	if err = e.inst.DB(ownership.Runtime).QueryRowContext(ctx, `SELECT count(*) FROM operations_lifecycle_jobs`).Scan(&jobs); err != nil || jobs != 6 {
		t.Fatal("reminder, purge and gc registrations", jobs, err)
	}
}
