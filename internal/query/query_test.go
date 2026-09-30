package query

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/authz/authztest"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/rights/rightstest"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

type authority struct {
	commit.Reader
	assets   map[ids.ID]catalog.AssetInfo
	versions map[ids.ID]catalog.VersionInfo
	onList   func()
	failure  error
	// listed 统计全库版本枚举次数；onAsset 在按资产读取时调用一次。
	listed  atomic.Int64
	onAsset func()
}

func (a *authority) ReadProjection(_ context.Context, id ids.ID) (catalog.AssetInfo, error) {
	if a.failure != nil {
		return catalog.AssetInfo{}, a.failure
	}
	v, ok := a.assets[id]
	if !ok {
		return v, errcode.New(errcode.NotFound, "")
	}
	return v, nil
}
func (a *authority) ReadProjectionVersion(_ context.Context, asset, version ids.ID) (catalog.VersionInfo, error) {
	v, ok := a.versions[version]
	if !ok {
		return v, errcode.New(errcode.NotFound, "")
	}
	if v.Version.AssetID != asset {
		return v, errcode.New(errcode.RefMismatch, "")
	}
	return v, nil
}
func (a *authority) Version(ctx context.Context, asset, version ids.ID) (commit.Committed, error) {
	v, err := a.ReadProjectionVersion(ctx, asset, version)
	return v.Version, err
}
func (a *authority) Versions(_ context.Context, after ids.ID, limit int) ([]commit.Committed, error) {
	a.listed.Add(1)
	var all []commit.Committed
	for id, v := range a.versions {
		if id > after {
			all = append(all, v.Version)
		}
	}
	slices.SortFunc(all, func(a, b commit.Committed) int { return strings.Compare(string(a.VersionID), string(b.VersionID)) })
	if len(all) > limit {
		all = all[:limit]
	}
	if a.onList != nil {
		fn := a.onList
		a.onList = nil
		fn()
	}
	return all, nil
}
func (a *authority) AssetVersions(_ context.Context, asset ids.ID, afterNumber int64, limit int) ([]commit.Committed, error) {
	if a.onAsset != nil {
		fn := a.onAsset
		a.onAsset = nil
		fn()
	}
	all := []commit.Committed{}
	for _, v := range a.versions {
		if v.Version.AssetID == asset && v.Version.VersionNumber > afterNumber {
			all = append(all, v.Version)
		}
	}
	slices.SortFunc(all, func(a, b commit.Committed) int { return int(a.VersionNumber - b.VersionNumber) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

type eventLog struct {
	entries []events.Entry
	pruned  int64
}

func (e *eventLog) HighWater(context.Context) (int64, error) { return int64(len(e.entries)), nil }
func (e *eventLog) Read(_ context.Context, after int64, limit int) (events.Page, error) {
	if after < e.pruned {
		return events.Page{}, errcode.New(errcode.CursorExpired, "")
	}
	end := min(int64(len(e.entries)), after+int64(limit))
	return events.Page{Entries: slices.Clone(e.entries[after:end]), HighWater: end}, nil
}
func (e *eventLog) add(kind string, asset ids.ID, rev int64) {
	raw, _ := json.Marshal(map[string]any{"asset_id": asset})
	e.entries = append(e.entries, events.Entry{GlobalSeq: int64(len(e.entries) + 1), Envelope: event.Envelope{EventID: ids.New(), EventType: kind, AggregateType: "asset", AggregateID: asset, AggregateRevision: rev, SchemaVersion: 1, Payload: raw}})
}

type fixture struct {
	t                 *testing.T
	db                *sql.DB
	path              string
	s                 *Service
	source            *authority
	log               *eventLog
	az                *authztest.Static
	rights            *rightstest.Static
	who               authz.Context
	gate              *commands.Gate
	instance, project ids.ID
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, path: filepath.Join(t.TempDir(), "index.db"), instance: ids.New(), project: ids.New(), source: &authority{assets: map[ids.ID]catalog.AssetInfo{}, versions: map[ids.ID]catalog.VersionInfo{}}, log: &eventLog{}, rights: rightstest.New()}
	clk := clock.NewFake(time.Now())
	f.az = authztest.New(clk, f.instance)
	p := ids.New()
	f.az.AddPrincipal(p, authz.Human)
	f.az.Grant(p, f.project, catalog.ActionRead)
	f.who = f.az.OpenSession(p, time.Hour)
	f.gate = commands.NewGate(commands.NewCoordinator())
	f.gate.Open()
	f.open()
	t.Cleanup(func() { f.db.Close() })
	return f
}
func (f *fixture) open() {
	f.t.Helper()
	var err error
	f.db, err = sqlite.Open(f.t.Context(), f.path, sqlite.Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	if err = EnsureSchema(f.t.Context(), f.db); err != nil {
		f.t.Fatal(err)
	}
	f.s, err = New(Deps{DB: f.db, Gate: f.gate, Reader: f.source, Catalog: f.source, Events: f.log, Authz: f.az, Rights: f.rights, InstanceID: f.instance})
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) add(project ids.ID, title string) catalog.AssetInfo {
	a, v := ids.New(), ids.New()
	c := commit.Committed{ProjectID: project, AssetID: a, VersionID: v, VersionNumber: 1}
	i := catalog.AssetInfo{Asset: commit.Asset{AssetID: a, ProjectID: project, Slug: title}, Description: catalog.AssetDescription{AssetID: a, ProjectID: project, AssetType: manifest.TypeImage, Slug: title, Title: title, Tags: []string{"sample"}, Subjects: []string{}, Sensitivity: "normal"}, Latest: c}
	f.source.assets[a] = i
	f.source.versions[v] = catalog.VersionInfo{Version: c, Manifest: manifest.Document{InstanceID: f.instance, ProjectID: project, AssetID: a, VersionID: v, Content: manifest.Content{Uses: []manifest.Use{}}}}
	return i
}
func (f *fixture) build() State {
	f.t.Helper()
	st, err := f.s.Rebuild(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}
func (f *fixture) search(filter Filter) Page {
	f.t.Helper()
	p, err := f.s.Search(f.t.Context(), SearchRequest{Who: f.who, Filter: filter, Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}
func code(t *testing.T, err error, want errcode.Code) {
	t.Helper()
	if err == nil || errcode.CodeOf(err) != want {
		t.Fatalf("got %v, want %s", err, want)
	}
}

func TestDeleteIndexRebuildAndCurrentAuthorization(t *testing.T) {
	f := setup(t)
	a := f.add(f.project, "目录 雪山")
	f.log.add("version.committed", a.Asset.AssetID, 1)
	st := f.build()
	if st.HighWater != 1 || st.RebuildStart != 1 {
		t.Fatalf("bad watermarks: %+v", st)
	}
	before := f.search(Filter{Text: "雪山"})
	if len(before.Items) != 1 {
		t.Fatal(before)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	f.open()
	f.build()
	after := f.search(Filter{Text: "雪山"})
	aJSON, _ := json.Marshal(before.Items)
	bJSON, _ := json.Marshal(after.Items)
	if string(aJSON) != string(bJSON) {
		t.Fatalf("rebuilt items differ: %s %s", aJSON, bJSON)
	}
	f.az.Revoke(f.who.PrincipalID, f.project)
	f.who = f.az.OpenSession(f.who.PrincipalID, time.Hour)
	if p := f.search(Filter{}); len(p.Items) != 0 {
		t.Fatal("stale projection exposed revoked object", p)
	}
	f.az.Grant(f.who.PrincipalID, f.project, catalog.ActionRead)
	f.rights.Restrict(a.Latest.VersionID, authz.PurposeArchiveReview)
	if p := f.search(Filter{}); len(p.Items) != 0 {
		t.Fatal("stale projection exposed newly restricted object", p)
	}
	f.rights.Clear(a.Latest.VersionID)
	current := f.source.assets[a.Asset.AssetID]
	current.Description.Sensitivity = "personal"
	f.source.assets[a.Asset.AssetID] = current
	if p := f.search(Filter{}); len(p.Items) != 0 {
		t.Fatal("stale projection ignored personal description", p)
	}
	f.az.Grant(f.who.PrincipalID, f.project, authz.Action("personal.read"))
	if p := f.search(Filter{}); len(p.Items) != 1 {
		t.Fatal("authorized personal object missing", p)
	}
}

func TestRelationsNeverExposeHiddenSources(t *testing.T) {
	f := setup(t)
	a := f.add(f.project, "derived")
	secret := f.add(ids.New(), "TOP-SECRET")
	v := f.source.versions[a.Latest.VersionID]
	v.Manifest.Content.Uses = []manifest.Use{{InstanceID: f.instance, AssetID: secret.Asset.AssetID, VersionID: secret.Latest.VersionID, Relation: "derived_from", Declared: "private/source/path"}}
	f.source.versions[a.Latest.VersionID] = v
	f.build()
	related, err := f.s.Relations(t.Context(), f.who, a.Latest.Ref(f.instance), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 0 {
		t.Fatal("hidden source exposed", related)
	}
	raw, _ := json.Marshal(f.search(Filter{}))
	for _, hidden := range []string{string(secret.Asset.AssetID), "TOP-SECRET", "private/source/path"} {
		if strings.Contains(string(raw), hidden) {
			t.Fatalf("response exposed %s", hidden)
		}
	}
	f.az.Grant(f.who.PrincipalID, secret.Asset.ProjectID, catalog.ActionRead)
	related, err = f.s.Relations(t.Context(), f.who, a.Latest.Ref(f.instance), false)
	if err != nil || len(related) != 1 {
		t.Fatalf("visible relation missing: %+v %v", related, err)
	}
	f.rights.Pending(secret.Latest.VersionID, authz.PurposeArchiveReview)
	related, err = f.s.Relations(t.Context(), f.who, a.Latest.Ref(f.instance), false)
	if err != nil || len(related) != 0 {
		t.Fatalf("pending source exposed: %+v %v", related, err)
	}
}

func TestFilteredChangesAdvanceAndExpiredCursorReplacesScope(t *testing.T) {
	f := setup(t)
	a := f.add(f.project, "visible")
	f.build()
	f.log.add("version.committed", a.Asset.AssetID, 1)
	f.az.Revoke(f.who.PrincipalID, f.project)
	f.who = f.az.OpenSession(f.who.PrincipalID, time.Hour)
	changes, err := f.s.Changes(t.Context(), f.who, 0, 10)
	if err != nil || len(changes.Items) != 0 || changes.HighWater != 1 {
		t.Fatalf("filtered high water: %+v %v", changes, err)
	}
	f.log.add("project.members_changed", a.Asset.AssetID, 2)
	changes, err = f.s.Changes(t.Context(), f.who, 1, 10)
	if err != nil || !changes.ResyncRequired || changes.HighWater != 2 {
		t.Fatalf("no permission resync: %+v %v", changes, err)
	}
	f.log.pruned = 2
	_, err = f.s.Changes(t.Context(), f.who, 1, 10)
	code(t, err, errcode.CursorExpired)
	_, err = f.s.CatchUp(t.Context())
	code(t, err, errcode.CursorExpired)
	f.build()
	snapshot, err := f.s.Snapshot(t.Context(), f.who, Filter{ProjectID: f.project})
	if err != nil || !snapshot.ReplaceScope || len(snapshot.Items) != 0 || snapshot.State.HighWater < snapshot.Start {
		t.Fatalf("invalid replacement snapshot: %+v %v", snapshot, err)
	}
}

// 增量追赶只按资产读取权威版本、不枚举全库，代价与全库规模无关；追赶读取
// 权威事实期间搜索不被阻塞（BUG-20260930-06）。
func TestCatchUpRefreshesOnlyAffectedAssetsWithoutBlockingSearch(t *testing.T) {
	f := setup(t)
	var target catalog.AssetInfo
	for i := range 50 {
		a := f.add(f.project, fmt.Sprintf("asset %02d", i))
		if i == 7 {
			target = a
		}
	}
	f.build()
	f.source.listed.Store(0)
	target.Description.Title = "refreshed title"
	target.Description.Revision = 2
	f.source.assets[target.Asset.AssetID] = target
	f.log.add("ledger.metadata_committed", target.Asset.AssetID, 2)
	f.log.add("ledger.metadata_committed", target.Asset.AssetID, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.source.onAsset = func() {
		close(entered)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.s.CatchUp(t.Context())
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("catch-up finished without a per-asset authoritative read (err %v, whole-ledger enumerations %d)", err, f.source.listed.Load())
	case <-time.After(10 * time.Second):
		t.Fatal("catch-up did not reach the per-asset authoritative read")
	}
	searched := make(chan error, 1)
	go func() {
		_, err := f.s.Search(t.Context(), SearchRequest{Who: f.who, Limit: 10})
		searched <- err
	}()
	select {
	case err := <-searched:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search was blocked while catch-up read authoritative facts")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := f.source.listed.Load(); n != 0 {
		t.Fatalf("catch-up enumerated the whole ledger %d times", n)
	}
	if p := f.search(Filter{Text: "refreshed title"}); len(p.Items) != 1 {
		t.Fatal(p)
	}
}

func TestOutOfOrderRevisionReconcilesAuthority(t *testing.T) {
	f := setup(t)
	a := f.add(f.project, "old")
	f.build()
	a.Description.Title = "authoritative revision 3"
	a.Description.Revision = 3
	f.source.assets[a.Asset.AssetID] = a
	f.log.add("ledger.metadata_committed", a.Asset.AssetID, 3)
	f.log.add("ledger.metadata_committed", a.Asset.AssetID, 2)
	if _, err := f.s.CatchUp(t.Context()); err != nil {
		t.Fatal(err)
	}
	p := f.search(Filter{Text: "revision 3"})
	if len(p.Items) != 1 || p.Items[0].Revision != 3 {
		t.Fatal("projection went backwards", p)
	}
	var rev int64
	if err := f.db.QueryRow(`SELECT revision FROM query_revisions WHERE aggregate_type='ledger.metadata_committed:asset'`).Scan(&rev); err != nil || rev != 3 {
		t.Fatalf("revision=%d err=%v", rev, err)
	}
	if _, err := f.s.CatchUp(t.Context()); err != nil {
		t.Fatal(err)
	}
	var seen int
	if err := f.db.QueryRow(`SELECT count(*) FROM query_processed_events`).Scan(&seen); err != nil || seen != 2 {
		t.Fatalf("dedup count=%d err=%v", seen, err)
	}
}

func TestRebuildReplaysCommitBehindScanCursor(t *testing.T) {
	f := setup(t)
	f.add(f.project, "first")
	f.source.onList = func() {
		a := f.add(f.project, "committed during scan")
		v := f.source.versions[a.Latest.VersionID]
		delete(f.source.versions, a.Latest.VersionID)
		// A prepared ID can commit after the scanner passed its lexicographic position.
		earlier := ids.MustParse("00000000000000000000000001")
		v.Version.VersionID = earlier
		v.Manifest.VersionID = earlier
		a.Latest.VersionID = earlier
		f.source.versions[earlier] = v
		f.source.assets[a.Asset.AssetID] = a
		f.log.add("version.committed", a.Asset.AssetID, 1)
	}
	st := f.build()
	if st.RebuildStart != 0 || st.HighWater != 1 {
		t.Fatal(st)
	}
	if p := f.search(Filter{}); len(p.Items) != 2 {
		t.Fatal("scan/replay lost a concurrent commit", p)
	}
}

func TestAtomicConsumerAndFailedRebuildPreserveVisibleGeneration(t *testing.T) {
	f := setup(t)
	a := f.add(f.project, "old")
	initial := f.build()
	a.Description.Title = "new"
	f.source.assets[a.Asset.AssetID] = a
	f.log.add("ledger.metadata_committed", a.Asset.AssetID, 1)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_projection BEFORE INSERT ON query_assets BEGIN SELECT RAISE(ABORT,'injected crash before commit'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CatchUp(t.Context()); err == nil {
		t.Fatal("expected write failure")
	}
	st, err := f.s.State(t.Context())
	if err != nil || st.HighWater != 0 {
		t.Fatalf("cursor escaped failed transaction: %+v %v", st, err)
	}
	if p := f.search(Filter{Text: "old"}); len(p.Items) != 1 {
		t.Fatal("partial mutation escaped rollback", p)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_projection`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CatchUp(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.source.failure = errors.New("injected missing authoritative file")
	if _, err := f.s.Rebuild(t.Context()); err == nil {
		t.Fatal("expected rebuild failure")
	}
	st, err = f.s.State(t.Context())
	if err != nil || st.Generation != initial.Generation || st.HighWater != 1 {
		t.Fatalf("failed rebuild replaced generation: %+v %v", st, err)
	}
	f.source.failure = nil
	if p := f.search(Filter{Text: "new"}); len(p.Items) != 1 {
		t.Fatal(p)
	}
}

func TestPaginationBindsScopeAndHidesSkippedIDs(t *testing.T) {
	f := setup(t)
	for i := 0; i < 4; i++ {
		f.add(f.project, "blue")
	}
	f.build()
	p, err := f.s.Search(t.Context(), SearchRequest{Who: f.who, Limit: 1})
	if err != nil || len(p.Items) != 1 || p.NextCursor == "" {
		t.Fatalf("first page: %+v %v", p, err)
	}
	decoded, _ := base64.RawURLEncoding.DecodeString(p.NextCursor)
	if strings.Contains(string(decoded), string(p.Items[0].AssetID)) {
		t.Fatal("cursor is not opaque")
	}
	next, err := f.s.Search(t.Context(), SearchRequest{Who: f.who, Limit: 1, Cursor: p.NextCursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].AssetID <= p.Items[0].AssetID {
		t.Fatalf("second page: %+v %v", next, err)
	}
	_, err = f.s.Search(t.Context(), SearchRequest{Who: f.who, Limit: 1, Filter: Filter{Text: "blue"}, Cursor: p.NextCursor})
	code(t, err, errcode.CursorExpired)
	f.build()
	_, err = f.s.Search(t.Context(), SearchRequest{Who: f.who, Limit: 1, Cursor: p.NextCursor})
	code(t, err, errcode.CursorExpired)
	f.gate.Close(commands.ReasonMaintenance)
	_, err = f.s.CatchUp(t.Context())
	code(t, err, errcode.MaintenanceMode)
	// Reads remain available during maintenance.
	f.search(Filter{})
}

type retainedLog struct {
	*eventLog
	gate              *commands.Gate
	db                *sql.DB
	registered, acked bool
	through           int64
	failAck           bool
}

func (r *retainedLog) RegisterConsumer(ctx context.Context, _ string, required bool) error {
	lctx, h, err := r.gate.Acquire(ctx, commands.Request{})
	if err != nil {
		return err
	}
	defer h.Release()
	if err = lctx.Err(); err != nil {
		return err
	}
	r.registered = required
	return nil
}
func (r *retainedLog) AcknowledgeConsumer(ctx context.Context, _ string, through int64) error {
	lctx, h, err := r.gate.Acquire(ctx, commands.Request{})
	if err != nil {
		return err
	}
	defer h.Release()
	st, err := readState(lctx, r.db)
	if err != nil {
		return err
	}
	if st.HighWater < through {
		return errors.New("acknowledged uncommitted index state")
	}
	if r.failAck {
		return errors.New("injected retention acknowledgement failure")
	}
	r.acked = true
	r.through = through
	return nil
}
func TestRetentionAcknowledgesOnlyCommittedStateAndRetriesEmptyCatchUp(t *testing.T) {
	f := setup(t)
	a := f.add(f.project, "retained")
	f.log.add("version.committed", a.Asset.AssetID, 1)
	retainer := &retainedLog{eventLog: f.log, gate: f.gate, db: f.db, failAck: true}
	f.s.events = retainer
	st, err := f.s.Rebuild(t.Context())
	var pending *RetentionPendingError
	if !errors.As(err, &pending) || st.Generation != 1 || st.HighWater != 1 || !retainer.registered || retainer.acked {
		t.Fatalf("postcommit failure lost state: %+v %v retainer=%+v", st, err, retainer)
	}
	if len(f.search(Filter{}).Items) != 1 {
		t.Fatal("index was not committed before acknowledgement")
	}
	retainer.failAck = false
	again, err := f.s.CatchUp(t.Context())
	if err != nil || again != st || !retainer.acked || retainer.through != 1 {
		t.Fatalf("zero-event retry failed: %+v %v retainer=%+v", again, err, retainer)
	}
}
