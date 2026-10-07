package integration

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestM2TrashProtectionsAtThreeHourBoundary(t *testing.T) {
	for _, protection := range []string{"lock", "incoming_use", "published_reviewed"} {
		for _, delta := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
			t.Run(fmt.Sprintf("%s/%dms", protection, delta.Milliseconds()), func(t *testing.T) {
				var e *env
				var v catalog.VersionResult
				var who authz.Context
				if protection == "published_reviewed" {
					f := newFlowEnv(t)
					e = f.env
					who = f.maker
					v = f.approvedContext("protected-rules", []byte("synthetic reviewed published rules"))
				} else {
					e = newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
					e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
					_, maker := e.agent("protected-maker@node", identity.RoleContributor)
					who = maker.Context
					v = e.ingest(who, "protected", []byte("synthetic protected content"), *rightsOwned())
				}
				app := reopenApplication(t, e)
				if protection == "lock" {
					if _, err := e.ledger.Lock(t.Context(), e.login().Context, e.key(), ledger.LockRequest{ProjectID: v.ProjectID, AssetID: v.AssetID, Reason: "synthetic boundary lock"}); err != nil {
						t.Fatal(err)
					}
				} else if protection == "incoming_use" {
					e.ingest(e.login().Context, "dependent", []byte("synthetic derived bytes"), *rightsOwned(), catalog.DeclaredUse{AssetID: v.AssetID, VersionID: v.VersionID, Relation: "derived_from"})
				}
				committed, err := e.ledger.Version(t.Context(), v.AssetID, v.VersionID)
				if err != nil {
					t.Fatal(err)
				}
				e.clk.Set(committed.CommittedAt.Add(3*time.Hour + delta))
				req, err := app.Lifecycle.PreviewTrash(t.Context(), who, ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, Reason: "synthetic protected boundary"})
				if protection == "lock" {
					if errcode.CodeOf(err) != errcode.AssetLocked {
						t.Fatal(err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if _, err = app.Lifecycle.TrashOwn(t.Context(), who, e.key(), req); errcode.CodeOf(err) != errcode.AssetInUse {
						t.Fatal("protected content was accepted", err)
					}
				}
				state, err := e.ledger.VersionControl(t.Context(), v.VersionID)
				if err != nil || state.Lifecycle != "active" || state.PendingOperationID != "" {
					t.Fatal("rejection changed authority", state, err)
				}
			})
		}
	}
}

func TestM2DueHoldUsesRealScheduler(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	app := reopenApplication(t, e)
	who := e.login().Context
	v := e.ingest(who, "held-due", []byte("synthetic due hold bytes"), *rightsOwned())
	entry := trashForTest(t, e, app.Lifecycle, who, v.AssetID)
	held, err := mutateTrashForTest(t, e, app.Lifecycle, who, entry, identity.ActHold)
	if err != nil {
		t.Fatal(err)
	}
	due, err := clock.Parse(held.DueAt)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Set(due.Add(time.Millisecond))
	if r, err := app.RunLifecycle(t.Context()); err != nil || r.Purges != 0 || r.Failed != 0 {
		t.Fatal("registered job bypassed Hold", r, err)
	}
	who = e.login().Context
	current, err := app.Lifecycle.Entry(t.Context(), who, v.ProjectID, held.ID)
	if err != nil || current.State != "trashed" || !current.Hold {
		t.Fatal(current, err)
	}
	if _, err = mutateTrashForTest(t, e, app.Lifecycle, who, current, identity.ActUnhold); err != nil {
		t.Fatal(err)
	}
	if r, err := app.RunLifecycle(t.Context()); err != nil || r.Purges != 1 {
		t.Fatal(r, err)
	}
}

func TestM2OrdinaryFiftyOneRequiresExactHumanBatch(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	_, maker := e.agent("large-batch-maker@node", identity.RoleContributor)
	app := reopenApplication(t, e)
	v := e.ingest(maker.Context, "ordinary-large", []byte("synthetic large batch 0"), *rightsOwned())
	for i := 1; i < 51; i++ {
		v = appendHistoryVersion(t, e, maker.Context, v, []byte(fmt.Sprintf("synthetic large batch %d", i)))
	}
	e.clk.Advance(4 * time.Hour)
	sel := ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, WholeAsset: true, Reason: "synthetic 51 ordinary versions"}
	req, err := app.Lifecycle.PreviewTrash(t.Context(), maker.Context, sel)
	if err != nil || len(req.Targets) != 51 {
		t.Fatal(len(req.Targets), err)
	}
	if _, err = app.Lifecycle.TrashOwn(t.Context(), maker.Context, e.key(), req); errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatal("51 ordinary versions bypassed confirmation", err)
	}
	who := e.login().Context
	actions, err := app.Lifecycle.TrashBatchHumanActions(t.Context(), []ledger.TrashRequest{req}, false)
	if err != nil {
		t.Fatal(err)
	}
	grant, _ := grantDomainForTest(t, e, who, actions, app.Lifecycle)
	results, err := app.Lifecycle.TrashHumanBatch(t.Context(), who, grant, e.id)
	if err != nil || len(results) != 1 || results[0].Receipt == nil || results[0].ErrorCode != "" {
		t.Fatal(results, err)
	}
	var entry ledger.TrashEntry
	if err = json.Unmarshal(results[0].Receipt.ResponseSummary, &entry); err != nil || len(entry.VersionIDs) != 51 || entry.Grace || entry.RetentionDays != 30 {
		t.Fatal(entry, err)
	}
}

// These tests exercise the actual T01/T02/T03/T05/T08 assembly. Only the clock
// is controlled; server commit and trash timestamps remain authoritative.
func TestM2TrashThreeHourObjectBoundaries(t *testing.T) {
	for _, shape := range []string{"version", "new_asset", "old_intermediate", "old_asset"} {
		for _, delta := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
			t.Run(fmt.Sprintf("%s/%dms", shape, delta.Milliseconds()), func(t *testing.T) {
				e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
				_, session := e.agent("boundary-maker@node", identity.RoleContributor)
				app := reopenApplication(t, e)
				v := e.ingest(session.Context, "boundary", []byte("synthetic boundary bytes"), *rightsOwned())
				latest := e.clk.Now()
				if shape == "old_intermediate" || shape == "old_asset" {
					e.clk.Advance(4 * time.Hour)
					appendHistoryVersion(t, e, session.Context, v, []byte("synthetic fresh latest"))
					latest = e.clk.Now()
				}
				e.clk.Set(latest.Add(3*time.Hour + delta))
				whole := shape == "new_asset" || shape == "old_asset"
				selector := ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, WholeAsset: whole, Reason: "synthetic exact boundary"}
				if !whole {
					selector.VersionID = v.VersionID
				}
				req, err := app.Lifecycle.PreviewTrash(t.Context(), session.Context, selector)
				if err != nil {
					t.Fatal(err)
				}
				r, err := app.Lifecycle.TrashOwn(t.Context(), session.Context, e.key(), req)
				if whole && (shape == "old_asset" || delta >= 0) {
					if errcode.CodeOf(err) != errcode.HumanProofRequired {
						t.Fatal("whole asset bypassed its first-commit grace boundary", err)
					}
					state, e2 := e.ledger.VersionControl(t.Context(), v.VersionID)
					if e2 != nil || state.Lifecycle != "active" || state.PendingOperationID != "" {
						t.Fatal("rejection changed version state", state, e2)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var entry ledger.TrashEntry
				if err = json.Unmarshal(r.ResponseSummary, &entry); err != nil {
					t.Fatal(err)
				}
				want := 30
				if delta < 0 {
					want = 7
				}
				if entry.Grace != (delta < 0) || entry.RetentionDays != want {
					t.Fatal("server commit grace boundary differs", entry)
				}
				t.Logf("shape=%s delta_ms=%d grace=%v retention_days=%d operation=%s", shape, delta.Milliseconds(), entry.Grace, entry.RetentionDays, r.OperationID)
			})
		}
	}
}

func TestM2RetentionRestoreAndPurgeBoundaries(t *testing.T) {
	for _, days := range []int{7, 30} {
		for _, delta := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
			for _, action := range []string{"restore", "purge"} {
				t.Run(fmt.Sprintf("%dd/%s/%dms", days, action, delta.Milliseconds()), func(t *testing.T) {
					e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
					e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
					app := reopenApplication(t, e)
					who := e.login().Context
					v := e.ingest(who, "retention", []byte("synthetic retention bytes"), *rightsOwned())
					if days == 30 {
						e.clk.Advance(4 * time.Hour)
						who = e.login().Context
					}
					entry := trashForTest(t, e, app.Lifecycle, who, v.AssetID)
					if entry.RetentionDays != days {
						t.Fatal(entry)
					}
					created, _ := clock.Parse(entry.CreatedAt)
					due, _ := clock.Parse(entry.DueAt)
					if due.Sub(created) != time.Duration(days)*24*time.Hour {
						t.Fatal("retention does not start at accepted trash time")
					}
					// Renew the real session before setting the exact tested instant.
					e.clk.Set(due.Add(-time.Minute))
					who = e.login().Context
					e.clk.Set(due.Add(delta))
					if action == "restore" {
						if _, err := app.Lifecycle.Restore(t.Context(), who, e.key(), ledger.RestoreRequest{ProjectID: entry.ProjectID, TrashID: entry.ID, ExpectedRevision: entry.Revision, Reason: "synthetic boundary restore"}); err != nil {
							t.Fatal("unpurged content must remain restorable after its due time", err)
						}
						if err := e.ledger.CheckVersionRead(t.Context(), v.AssetID, v.VersionID); err != nil {
							t.Fatal(err)
						}
					} else {
						r, err := app.RunLifecycle(t.Context())
						if err != nil || r.Failed != 0 || r.Purges != map[bool]int{false: 0, true: 1}[delta >= 0] {
							t.Fatal("registered server purge boundary differs", r, err)
						}
						current, err := app.Lifecycle.Entry(t.Context(), who, entry.ProjectID, entry.ID)
						want := "trashed"
						if delta >= 0 {
							want = "purged"
						}
						if err != nil || current.State != want {
							t.Fatal(current, err)
						}
					}
					t.Logf("days=%d action=%s delta_ms=%d due=%s", days, action, delta.Milliseconds(), entry.DueAt)
				})
			}
		}
	}
}

func TestM2AgentRealRollingQuotaBoundaries(t *testing.T) {
	for _, limit := range []int{20, 200} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
			_, agent := e.agent("quota-boundary-maker@node", identity.RoleContributor)
			app := reopenApplication(t, e)
			v := e.ingest(agent.Context, "quota-boundary", []byte("synthetic quota 0"), *rightsOwned())
			versions := []ledger.TrashSelector{{ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, Reason: "synthetic quota boundary"}}
			for i := 1; i <= limit; i++ {
				v = appendHistoryVersion(t, e, agent.Context, v, []byte(fmt.Sprintf("synthetic quota %d", i)))
				versions = append(versions, ledger.TrashSelector{ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, Reason: "synthetic quota boundary"})
			}
			if limit == 20 {
				e.clk.Advance(4 * time.Hour)
			}
			at := e.clk.Now()
			var first ledger.TrashRequest
			var firstKey string
			for i, sel := range versions[:limit] {
				req, err := app.Lifecycle.PreviewTrash(t.Context(), agent.Context, sel)
				if err != nil {
					t.Fatal(err)
				}
				key := e.key()
				if _, err = app.Lifecycle.TrashOwn(t.Context(), agent.Context, key, req); err != nil {
					t.Fatal(i, err)
				}
				if i == 0 {
					first, firstKey = req, key
				}
			}
			pending, err := app.Lifecycle.PreviewTrash(t.Context(), agent.Context, versions[limit])
			if err != nil {
				t.Fatal(err)
			}
			checkQuota := func() {
				t.Helper()
				var count, units int
				if err := e.inst.DB(ownership.Ledger).QueryRowContext(t.Context(), `SELECT count(*),sum(units) FROM ledger_trash_quota WHERE principal_id=?`, agent.Context.PrincipalID).Scan(&count, &units); err != nil || count != limit || units != limit {
					t.Fatal("failure or replay charged quota twice", count, units, err)
				}
			}
			for _, instant := range []time.Time{at, at.Add(time.Hour - time.Millisecond)} {
				e.clk.Set(instant)
				if _, err := app.Lifecycle.TrashOwn(t.Context(), agent.Context, e.key(), pending); errcode.CodeOf(err) != errcode.QuotaExceeded {
					t.Fatal("quota released before rolling-hour boundary", err)
				}
				if _, err := app.Lifecycle.TrashOwn(t.Context(), agent.Context, firstKey, first); err != nil {
					t.Fatal(err)
				}
				checkQuota()
			}
			e.clk.Set(at.Add(time.Hour))
			if _, err = app.Lifecycle.TrashOwn(t.Context(), agent.Context, e.key(), pending); err != nil {
				t.Fatal("quota failed to release at exact rolling-hour boundary", err)
			}
			t.Logf("quota=%d accepted=%d rejected_before_hour=true replay_did_not_charge=true exact_hour_released=true", limit, limit+1)
		})
	}
}
