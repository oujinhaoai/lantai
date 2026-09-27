package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/authz/authztest"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/commit/committest"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/install/installtest"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

type verifyFunc func(context.Context, authz.Context, commit.Prepared) error

func (f verifyFunc) VerifyAcceptance(ctx context.Context, who authz.Context, p commit.Prepared) error {
	return f(ctx, who, p)
}

type fixture struct {
	s       *Service
	db      *sql.DB
	path    string
	az      *authztest.Static
	in      *installtest.Memory
	rev     *committest.Revisions
	clk     *clock.Fake
	gate    *commands.Gate
	who     authz.Context
	project ids.ID
	deps    Deps
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{path: filepath.Join(t.TempDir(), "ledger.db"), clk: clock.NewFake(time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC))}
	var err error
	f.db, err = sqlite.Open(t.Context(), f.path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	for _, m := range migrations.For(ownership.Ledger) {
		if _, err := f.db.ExecContext(t.Context(), m.SQL); err != nil {
			t.Fatal(err)
		}
	}
	f.az = authztest.New(f.clk, ids.New())
	f.in = installtest.New(f.clk)
	f.rev = committest.NewRevisions(f.clk)
	f.gate = commands.NewGate(commands.NewCoordinator())
	f.gate.Open()
	f.deps = Deps{DB: f.db, Gate: f.gate, Authority: f.az, Clock: f.clk, IDs: &ids.Generator{Clock: f.clk, Rand: rand.Reader}, Installer: f.in, Revisions: f.rev, Acceptance: verifyFunc(func(context.Context, authz.Context, commit.Prepared) error { return nil })}
	f.s, err = New(f.deps)
	if err != nil {
		t.Fatal(err)
	}
	principal := ids.New()
	f.az.AddPrincipal(principal, authz.Human)
	f.az.Grant(principal, "", commit.ActionCreateProject)
	f.who = f.az.OpenSession(principal, time.Hour)
	f.project = f.newProject(t)
	f.az.Grant(principal, f.project, commit.ActionCommitVersion, commit.ActionReadVersion, "catalog.read", "catalog.patch_metadata", "catalog.patch_own_metadata", "catalog.patch_project")
	return f
}
func (f *fixture) newProject(t *testing.T) ids.ID {
	t.Helper()
	key := "p-" + fmt.Sprint(time.Now().UnixNano())
	cmd := f.command(commit.CommandRegisterProject, "", key, map[string]any{"key": key})
	p, err := f.s.RegisterProject(t.Context(), cmd, commit.ProjectRequest{Who: f.who, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return p.ProjectID
}
func (f *fixture) command(typ string, project ids.ID, key string, body any) commands.Context {
	raw, _ := json.Marshal(body)
	h, _ := commands.RequestHash(commands.HashInput{CommandType: typ, ProjectID: project, Body: raw})
	return commands.Context{OperationID: ids.New(), IdempotencyKey: key, RequestHash: h, CommandType: typ, ActorID: f.who.PrincipalID, SessionID: f.who.SessionID, ProjectID: project, RecoveryEpoch: f.who.RecoveryEpoch}
}
func (f *fixture) req(slug string) commit.PrepareRequest {
	files := []install.File{f.in.PutBlob("a.txt", []byte(slug))}
	return commit.PrepareRequest{Who: f.who, ProjectID: f.project, Slug: slug, Files: files, ManifestDigest: committest.ManifestDigest(files)}
}
func (f *fixture) prepare(t *testing.T, slug string) (commit.Prepared, install.Proof) {
	t.Helper()
	r := f.req(slug)
	p, err := f.s.Prepare(t.Context(), f.command(commit.CommandType, f.project, strings.ReplaceAll(slug, "/", "."), r), r)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := f.in.Install(t.Context(), p.InstallRequest())
	if err != nil {
		t.Fatal(err)
	}
	return p, proof
}
func (f *fixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = sqlite.Open(t.Context(), f.path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.deps.DB = f.db
	f.s, err = New(f.deps)
	if err != nil {
		t.Fatal(err)
	}
}
func wantCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if errcode.CodeOf(err) != code {
		t.Fatalf("want %s got %v", code, err)
	}
}

func TestPersistentLedgerContract(t *testing.T) {
	committest.RunLedgerContract(t, func(t *testing.T) committest.Harness {
		f := newFixture(t)
		cancel := func(id ids.ID) error {
			r, err := f.s.store.ReceiptByOperation(t.Context(), f.db, id)
			if err != nil {
				return err
			}
			return f.s.Cancel(t.Context(), id, f.az.OpenSession(r.Key.ActorID, time.Hour))
		}
		return committest.Harness{Ledger: f.s, Reader: f.s, Namespace: f.s, Metadata: f.s, Projects: f.s, Authz: f.az, Installer: f.in, Revisions: f.rev, Clock: f.clk, NewProject: func() ids.ID { return f.newProject(t) }, Cancel: cancel,
			Release: func(project ids.ID, slug string) error {
				// M2 name release is only a test fixture; no production release command.
				cl, err := f.s.Claim(t.Context(), project, slug)
				if err != nil {
					return err
				}
				cl.State = commit.ClaimReleased
				cl.Revision++
				_, err = f.db.ExecContext(t.Context(), `UPDATE ledger_namespace_claims SET record=? WHERE project_id=? AND slug_key=?`, encoded(cl), project, pathrule.Key(slug))
				return err
			}}
	})
}

func TestPreparedInstalledAndCommittedSurviveReopen(t *testing.T) {
	f := newFixture(t)
	r := f.req("recovery/one")
	cmd := f.command(commit.CommandType, f.project, "durable", r)
	p, err := f.s.Prepare(t.Context(), cmd, r)
	if err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	cmd.OperationID = ids.New()
	again, err := f.s.Prepare(t.Context(), cmd, r)
	if err != nil || again.VersionID != p.VersionID || again.VersionNumber != p.VersionNumber || again.OperationID != p.OperationID {
		t.Fatalf("prepare restart: %+v %v", again, err)
	}
	proof, err := f.in.Install(t.Context(), p.InstallRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.MarkInstalled(t.Context(), p.OperationID, f.who, proof); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	view, err := f.s.Operation(t.Context(), p.OperationID)
	if err != nil || view.Stage != commands.StageInstalled {
		t.Fatalf("installed restart: %+v %v", view, err)
	}
	_, err = f.s.Version(t.Context(), p.AssetID, p.VersionID)
	wantCode(t, err, errcode.NotFound)
	result, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	replay, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	if err != nil || replay != result {
		t.Fatalf("committed restart: %+v %v", replay, err)
	}
	for table, want := range map[string]int{"ledger_versions": 1, "ledger_version_states": 1} {
		var n int
		if err := f.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != want {
			t.Fatalf("%s rows=%d err=%v", table, n, err)
		}
	}
	var receipts, events int
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM command_receipts WHERE operation_id=? AND status='succeeded'`, p.OperationID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM outbox WHERE operation_id=?`, p.OperationID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || events != 1 {
		t.Fatalf("receipt/outbox %d/%d", receipts, events)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("injected event ID failure") }
func TestCommitTransactionRollsBackEverythingExceptInstalledEvidence(t *testing.T) {
	f := newFixture(t)
	p, proof := f.prepare(t, "atomic/one")
	f.s.ids = &ids.Generator{Clock: f.clk, Rand: failingReader{}}
	if _, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof); err == nil {
		t.Fatal("failure was not injected")
	}
	_, err := f.s.Version(t.Context(), p.AssetID, p.VersionID)
	wantCode(t, err, errcode.NotFound)
	for _, table := range []string{"ledger_versions", "ledger_version_states"} {
		var n int
		if err := f.db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s leaked rows: %d %v", table, n, err)
		}
	}
	view, err := f.s.Operation(t.Context(), p.OperationID)
	if err != nil || view.Stage != commands.StageInstalled {
		t.Fatalf("operation: %+v %v", view, err)
	}
	var n int
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM outbox WHERE operation_id=?`, p.OperationID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("outbox leaked: %d %v", n, err)
	}
	f.s.ids = f.deps.IDs
	if _, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentPrepareHasOneReservation(t *testing.T) {
	f := newFixture(t)
	r := f.req("race/one")
	cmd := f.command(commit.CommandType, f.project, "same", r)
	var wg sync.WaitGroup
	results := make(chan commit.Prepared, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			c := cmd
			c.OperationID = ids.New()
			p, err := f.s.Prepare(t.Context(), c, r)
			results <- p
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first commit.Prepared
	for p := range results {
		if first.OperationID == "" {
			first = p
		}
		if p.OperationID != first.OperationID || p.VersionID != first.VersionID {
			t.Fatal("race allocated duplicate reservation")
		}
	}
}

func TestAcceptanceUsesSharedGuardAndBlocksPublication(t *testing.T) {
	f := newFixture(t)
	p, proof := f.prepare(t, "rights/one")
	f.s.SetAcceptanceVerifier(verifyFunc(func(ctx context.Context, who authz.Context, p commit.Prepared) error {
		if _, err := f.s.Versions(ctx, "", 10); err != nil {
			return err
		}
		wait, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, held, err := f.gate.Coordinator().Acquire(wait, commands.Request{Security: commands.ModeExclusive})
		if err == nil {
			held.Release()
			t.Fatal("acceptance lacks security guard")
		}
		return errcode.New(errcode.UseRestricted, "frozen input rights tightened")
	}))
	_, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	wantCode(t, err, errcode.UseRestricted)
	f.reopen(t)
	view, err := f.s.Operation(t.Context(), p.OperationID)
	if err != nil || view.Stage != commands.StageBlocked {
		t.Fatalf("blocked not persisted: %+v %v", view, err)
	}
	if _, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownAndInactiveProjectsCannotReserveNames(t *testing.T) {
	f := newFixture(t)
	r := f.req("project/one")
	r.ProjectID = ids.New()
	f.az.Grant(f.who.PrincipalID, r.ProjectID, commit.ActionCommitVersion)
	_, err := f.s.Prepare(t.Context(), f.command(commit.CommandType, r.ProjectID, "missing", r), r)
	wantCode(t, err, errcode.NotFound)
	r.ProjectID = f.project
	p, err := f.s.Project(t.Context(), f.project)
	if err != nil {
		t.Fatal(err)
	}
	p.State = commit.ProjectArchived
	if _, err := f.db.ExecContext(t.Context(), `UPDATE ledger_projects SET record=? WHERE project_id=?`, encoded(p), p.ProjectID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Prepare(t.Context(), f.command(commit.CommandType, r.ProjectID, "inactive", r), r)
	wantCode(t, err, errcode.InvalidStateTransition)
}

func TestOrphanQuarantineDoesNotCreateCommittedVersion(t *testing.T) {
	f := newFixture(t)
	p, proof := f.prepare(t, "orphan/one")
	if err := f.s.QuarantineOrphan(t.Context(), p.OperationID); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	wantCode(t, err, errcode.InvalidStateTransition)
	_, err = f.s.Version(t.Context(), p.AssetID, p.VersionID)
	wantCode(t, err, errcode.NotFound)
	if _, ok := f.in.Quarantined(p.OperationID); !ok {
		t.Fatal("orphan bytes were not quarantined")
	}
	unknown := ids.New()
	if err := f.s.QuarantineOrphan(t.Context(), unknown); err != nil {
		t.Fatal(err)
	}
	ids, err := f.s.OpenOperations(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != p.OperationID {
		t.Fatalf("recovery hooks: %v %v", ids, err)
	}
}

func TestMissingAcceptanceVerifierFailsClosed(t *testing.T) {
	f := newFixture(t)
	p, proof := f.prepare(t, "unwired/one")
	f.s.SetAcceptanceVerifier(nil)
	_, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	wantCode(t, err, errcode.OperationNeedsReconciliation)
}

func TestContextAndMetadataAuthorizationCannotBeForged(t *testing.T) {
	f := newFixture(t)
	p, proof := f.prepare(t, "context/one")
	if _, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof); err != nil {
		t.Fatal(err)
	}
	r := commit.MetadataRequest{Who: f.who, Target: commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: f.project, ID: p.AssetID}, ContentDigest: digest.Of([]byte("x")), Action: commit.ActionReadVersion}
	_, err := f.s.PrepareMetadata(t.Context(), f.command(commit.CommandCommitMetadata, f.project, "wrong-action", r), r)
	wantCode(t, err, errcode.SchemaInvalid)
}

func TestCommittedReplayDuringMaintenanceStillChecksCurrentReader(t *testing.T) {
	f := newFixture(t)
	p, proof := f.prepare(t, "replay/one")
	result, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetAcceptanceVerifier(verifyFunc(func(context.Context, authz.Context, commit.Prepared) error {
		t.Fatal("committed replay reran acceptance")
		return nil
	}))
	f.gate.Close(commands.ReasonMaintenance)
	replayed, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	if err != nil || replayed != result {
		t.Fatalf("maintenance replay: %+v %v", replayed, err)
	}
	f.az.Revoke(f.who.PrincipalID, f.project)
	if _, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof); err == nil {
		t.Fatal("revoked actor replayed a result")
	}
}

func TestOperationIDCannotBeReboundAcrossKeys(t *testing.T) {
	f := newFixture(t)
	r := f.req("id/one")
	cmd := f.command(commit.CommandType, f.project, "id-one", r)
	if _, err := f.s.Prepare(t.Context(), cmd, r); err != nil {
		t.Fatal(err)
	}
	r = f.req("id/two")
	other := f.command(commit.CommandType, f.project, "id-two", r)
	other.OperationID = cmd.OperationID
	_, err := f.s.Prepare(t.Context(), other, r)
	wantCode(t, err, errcode.IdempotencyConflict)
	_, err = f.s.Claim(t.Context(), f.project, r.Slug)
	wantCode(t, err, errcode.NotFound)
}

func TestProjectMetadataUsesDedicatedActionAndStableProofReplay(t *testing.T) {
	f := newFixture(t)
	target := commit.MetadataTarget{Kind: commit.TargetProject, ProjectID: f.project, ID: f.project}
	r := commit.MetadataRequest{Who: f.who, Target: target, ContentDigest: digest.Of([]byte("project description")), Action: "catalog.patch_project"}
	p, err := f.s.PrepareMetadata(t.Context(), f.command(commit.CommandCommitMetadata, f.project, "project-description", r), r)
	if err != nil {
		t.Fatal(err)
	}
	proof := f.rev.Write(p)
	result, err := f.s.CommitMetadata(t.Context(), p.OperationID, f.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	proof.WrittenAt = proof.WrittenAt.Add(time.Minute)
	again, err := f.s.CommitMetadata(t.Context(), p.OperationID, f.who, proof)
	if err != nil || again != result {
		t.Fatalf("metadata proof reconstruction: %+v %v", again, err)
	}
	proof.FileRef += ".other"
	_, err = f.s.CommitMetadata(t.Context(), p.OperationID, f.who, proof)
	wantCode(t, err, errcode.OperationNeedsReconciliation)
	proof.FileRef = strings.TrimSuffix(proof.FileRef, ".other")
	f.az.Revoke(f.who.PrincipalID, f.project)
	f.az.Grant(f.who.PrincipalID, f.project, "catalog.read")
	readOnly := f.az.OpenSession(f.who.PrincipalID, time.Hour)
	f.gate.Close(commands.ReasonMaintenance)
	again, err = f.s.CommitMetadata(t.Context(), p.OperationID, readOnly, proof)
	if err != nil || again != result {
		t.Fatalf("read-only metadata replay under maintenance: %+v %v", again, err)
	}
	f.az.Revoke(f.who.PrincipalID, f.project)
	if _, err := f.s.CommitMetadata(t.Context(), p.OperationID, readOnly, proof); err == nil {
		t.Fatal("revoked reader replayed metadata")
	}
}

type unavailableInstaller struct{ install.Installer }

func (unavailableInstaller) Verify(context.Context, install.Proof) error {
	return errcode.New(errcode.StorageUnavailable, "injected temporary unavailability")
}

func TestTemporaryStorageFailureDoesNotQuarantineValidInstall(t *testing.T) {
	f := newFixture(t)
	p, proof := f.prepare(t, "temporary/one")
	f.s.SetInstaller(unavailableInstaller{f.in})
	_, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	wantCode(t, err, errcode.StorageUnavailable)
	if _, quarantined := f.in.Quarantined(p.OperationID); quarantined {
		t.Fatal("temporary storage failure quarantined valid bytes")
	}
	view, err := f.s.Operation(t.Context(), p.OperationID)
	if err != nil || view.Stage != commands.StagePrepared {
		t.Fatalf("temporary failure changed stage: %+v %v", view, err)
	}
	f.s.SetInstaller(f.in)
	if _, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentMetadataCompletionReplaysDurableProof(t *testing.T) {
	f := newFixture(t)
	r := commit.MetadataRequest{Who: f.who, Target: commit.MetadataTarget{Kind: commit.TargetProject, ProjectID: f.project, ID: f.project}, ContentDigest: digest.Of([]byte("concurrent description")), Action: "catalog.patch_project"}
	p, err := f.s.PrepareMetadata(t.Context(), f.command(commit.CommandCommitMetadata, f.project, "concurrent-description", r), r)
	if err != nil {
		t.Fatal(err)
	}
	proof := f.rev.Write(p)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			result, err := f.s.CommitMetadata(t.Context(), p.OperationID, f.who, proof)
			if err == nil && (result.OperationID != p.OperationID || result.Revision != 1) {
				err = errors.New("metadata replay returned another result")
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM outbox WHERE operation_id=?`, p.OperationID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("metadata emitted %d events: %v", n, err)
	}
}
