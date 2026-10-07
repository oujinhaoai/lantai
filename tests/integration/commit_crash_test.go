package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// These flags exist only in the test executable. No production switch, protocol
// field or runtime hook is added. Each child pauses at a real boundary; only the
// parent terminates it, without running Go defers or instance shutdown.
var crashChild = flag.Bool("commit-crash-child", false, "run the private commit crash child")
var crashPoint = flag.String("commit-crash-point", "", "private child checkpoint")
var crashReady = flag.String("commit-crash-ready", "", "private child handshake path")
var crashEvidence = flag.String("commit-crash-evidence", "", "optional directory for redacted matrix JSON and child logs")

var commitCrashPoints = []string{
	"before_frozen_write", "frozen_temp_synced", "frozen_before_prepared",
	"prepared", "install_partial", "assembled_before_rename",
	"renamed_before_sync", "placement_synced", "storage_installed",
	"ledger_installed", "accepted_before_commit", "ledger_committed", "response_lost",
}

type crashObservation struct {
	Stage    commands.Stage       `json:"ledger_stage,omitempty"`
	Install  storage.InstallState `json:"storage"`
	Visible  bool                 `json:"visible"`
	ReadCode errcode.Code         `json:"read_code,omitempty"`
	Versions []commit.Committed   `json:"versions"`
	Orphans  []storage.Orphan     `json:"orphans,omitempty"`
	Files    map[string]string    `json:"file_sha256"`
	Frozen   map[string]string    `json:"frozen_sha256"`
}

type crashHandshake struct {
	Home        string
	Now         time.Time
	Request     catalog.VersionRequest
	OperationID ids.ID
	Prepared    commit.Prepared
	Observation crashObservation
}

type crashReport struct {
	Point          string                     `json:"point"`
	Platform       string                     `json:"platform"`
	ChildExit      string                     `json:"child_exit"`
	BinarySHA256   string                     `json:"test_binary_sha256"`
	Termination    string                     `json:"termination"`
	OperationID    ids.ID                     `json:"operation_id"`
	Prepared       commit.Prepared            `json:"prepared"`
	Before         crashObservation           `json:"before_kill"`
	Durable        crashObservation           `json:"after_kill_before_recovery"`
	Recovery       application.RecoveryReport `json:"recovery"`
	Result         catalog.VersionResult      `json:"result"`
	After          crashObservation           `json:"after_replay"`
	FileSystem     fsutil.FSInfo              `json:"filesystem"`
	DownloadSHA256 map[string]string          `json:"download_sha256"`
}

func writeCrashJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = (fileop.FS{}).ReplaceAtomic(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func observeCrash(t *testing.T, a *application.App, ctx context.Context, h crashHandshake, readGrant bool) crashObservation {
	t.Helper()
	out := crashObservation{Files: map[string]string{}, Frozen: map[string]string{}}
	view, err := a.Ledger.Operation(ctx, h.OperationID)
	if err == nil {
		out.Stage = view.Stage
	} else if errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal(err)
	}
	out.Install, err = a.Storage.InstallStatus(ctx, h.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := a.Ledger.RecoveryInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out.Versions = inv.Versions
	out.Orphans, err = a.Storage.ScanOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Prepared.VersionID != "" {
		_, err = a.Catalog.ResolvePermanent(ctx, h.Request.Who, ids.PermanentRef{AssetID: h.Prepared.AssetID, VersionID: h.Prepared.VersionID})
		out.Visible = err == nil
		if err != nil && errcode.CodeOf(err) != errcode.NotFound {
			t.Fatal(err)
		}
		if readGrant {
			_, err = a.Storage.IssueReadGrant(ctx, storage.ReadRequest{Who: h.Request.Who, AssetID: h.Prepared.AssetID, VersionID: h.Prepared.VersionID, Path: "a.md", Purpose: authz.PurposeReference})
			if err != nil {
				out.ReadCode = errcode.CodeOf(err)
			}
			if (err == nil) != out.Visible {
				t.Fatalf("catalog/read visibility disagrees: %v %v", out.Visible, err)
			}
		}
		dir := a.Storage.Layout().VersionDir(h.Prepared.ProjectID, h.Prepared.AssetID, h.Prepared.VersionNumber)
		for _, f := range h.Prepared.Files {
			sha, _, err := fileop.HashFile(filepath.Join(dir, storage.FilesDir, filepath.FromSlash(f.Path)))
			if err == nil {
				out.Files[f.Path] = sha
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
	frozen := filepath.Join(a.Instance.Layout().Home, "staging", "frozen", string(h.OperationID))
	entries, err := os.ReadDir(frozen)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		sha, _, err := fileop.HashFile(filepath.Join(frozen, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out.Frozen[e.Name()] = sha
	}
	return out
}

type crashController struct {
	t *testing.T
	e *env
	h crashHandshake
}

func (c *crashController) stop(point string) {
	if point != *crashPoint {
		return
	}
	c.h.Now = c.e.clk.Now()
	// IssueReadGrant is a write. Inside a file/ledger callback the existing gate
	// context is not available; only observe domain reads here. The independent
	// offline observation after kill checks the download gate as well.
	c.h.Observation = observeCrash(c.t, &application.App{Instance: c.e.inst, Ledger: c.e.ledger, Storage: c.e.storage, Catalog: c.e.catalog}, c.t.Context(), c.h, false)
	writeCrashJSON(c.t, *crashReady, c.h)
	<-make(chan struct{})
}

type crashLedger struct {
	*ledger.Service
	controller *crashController
}

func (l crashLedger) Prepare(ctx context.Context, cmd commands.Context, req commit.PrepareRequest) (commit.Prepared, error) {
	l.controller.stop("frozen_before_prepared")
	p, err := l.Service.Prepare(ctx, cmd, req)
	if err == nil {
		l.controller.h.Prepared = p
		l.controller.stop("prepared")
	}
	return p, err
}
func (l crashLedger) Commit(ctx context.Context, op ids.ID, who authz.Context, proof install.Proof) (commit.Committed, error) {
	l.controller.stop("storage_installed")
	v, err := l.Service.Commit(ctx, op, who, proof)
	if err == nil {
		l.controller.stop("ledger_committed")
	}
	return v, err
}

type crashAcceptance struct {
	*catalog.Service
	controller *crashController
}

func (v crashAcceptance) VerifyAcceptance(ctx context.Context, who authz.Context, p commit.Prepared) error {
	v.controller.stop("ledger_installed")
	err := v.Service.VerifyAcceptance(ctx, who, p)
	if err == nil {
		v.controller.stop("accepted_before_commit")
	}
	return err
}

func crashWire(t *testing.T, e *env, faults *fileop.Faults, l catalog.Ledger) {
	t.Helper()
	k, err := masterkey.Load(e.inst.Config().SecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	readKey, err := k.Derive("storage/read-grant/v1")
	if err != nil {
		t.Fatal(err)
	}
	gen := &ids.Generator{Clock: e.clk, Rand: rand.Reader}
	e.storage, err = storage.New(storage.Deps{Runtime: e.inst.DB(ownership.Runtime), Home: e.inst.Layout().Home, Gate: e.inst.Gate(), Clock: e.clk, IDs: gen, Authz: e.id, Reads: e.id, Ledger: e.ledger, Rights: e.rights, ReadGrantKey: readKey, InstanceID: e.inst.InstanceID(), FS: fileop.FS{Faults: faults}}, storage.Config{MinFreeBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	e.catalog, err = catalog.New(catalog.Deps{Home: e.inst.Layout().Home, Gate: e.inst.Gate(), Storage: e.storage, Ledger: l, Authz: e.id, Rights: e.rights, Clock: e.clk, IDs: gen, InstanceID: e.inst.InstanceID(), FS: fileop.FS{Faults: faults}})
	if err != nil {
		t.Fatal(err)
	}
	e.ledger.SetInstaller(e.storage)
	e.ledger.SetRevisionVerifier(e.catalog)
	e.ledger.SetAcceptanceVerifier(e.catalog)
	e.rights.SetFiles(e.storage)
	e.rights.SetCatalog(e.catalog)
}

func crashRequest(t *testing.T, e *env) (catalog.VersionRequest, ids.ID) {
	t.Helper()
	_, session := e.agent("crash@synthetic", identity.RoleContributor)
	bodies := [][]byte{[]byte("synthetic primary\n"), []byte("synthetic attachment\n"), []byte("synthetic nested\n")}
	paths := []string{"a.md", "attachments/b.txt", "nested/c.txt"}
	specs := make([]storage.FileSpec, len(bodies))
	files := make([]manifest.InputFile, len(bodies))
	for i, b := range bodies {
		specs[i] = storage.FileSpec{SHA256: shaOf(b), Size: int64(len(b))}
		role := "doc"
		if i == 0 {
			role = "primary"
		}
		files[i] = manifest.InputFile{Path: paths[i], Role: role, SHA256: shaOf(b), Size: int64(len(b))}
	}
	u, err := e.storage.CreateUpload(t.Context(), storage.CreateUploadRequest{Who: session.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID, Files: specs})
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range bodies {
		if _, err = e.storage.PutPart(t.Context(), storage.PartRequest{Who: session.Context, UploadID: u.UploadID, SHA256: specs[i].SHA256, PartNumber: 1, PartSHA256: specs[i].SHA256, Size: specs[i].Size, Body: bytes.NewReader(b)}); err != nil {
			t.Fatal(err)
		}
		if _, err = e.storage.CompleteFile(t.Context(), session.Context, u.UploadID, specs[i].SHA256); err != nil {
			t.Fatal(err)
		}
	}
	return catalog.VersionRequest{Who: session.Context, IdempotencyKey: e.key(), UploadID: u.UploadID, Slug: "crash/matrix", Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: files}}, u.OperationID
}

func TestCommitCrashChild(t *testing.T) {
	if !*crashChild {
		t.Skip("private child, invoked by TestCommitCrashMatrix")
	}
	if *crashReady == "" {
		t.Fatal("missing child handshake path")
	}
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	req, op := crashRequest(t, e)
	c := &crashController{t: t, e: e, h: crashHandshake{Home: e.inst.Layout().Home, Request: req, OperationID: op}}
	faults := &fileop.Faults{
		Write: func(path string) error {
			if strings.Contains(filepath.ToSlash(path), "/staging/frozen/") {
				c.stop("before_frozen_write")
			}
			if strings.Contains(filepath.ToSlash(path), "/staging/install/") && filepath.Base(path) == storage.ManifestFile {
				c.stop("install_partial")
			}
			return nil
		},
		Link: func(old, new string) error {
			if strings.Contains(filepath.ToSlash(new), "/staging/frozen/") {
				c.stop("frozen_temp_synced")
			}
			return nil
		},
		Rename: func(old, new string) error {
			if old == filepath.Join(c.h.Home, "staging", "install", string(op)) {
				c.stop("assembled_before_rename")
			}
			return nil
		},
		Sync: func(path string) error {
			p := c.h.Prepared
			if p.VersionID == "" {
				return nil
			}
			final := e.storage.Layout().VersionDir(p.ProjectID, p.AssetID, p.VersionNumber)
			if _, err := os.Stat(final); err == nil {
				if path == filepath.Dir(final) {
					c.stop("renamed_before_sync")
				}
				if path == filepath.Join(c.h.Home, "staging", "install") && *crashPoint == "placement_synced" {
					if err := (fileop.FS{}).SyncDir(path); err != nil {
						return err
					}
					c.stop("placement_synced")
				}
			}
			return nil
		},
	}
	crashWire(t, e, faults, crashLedger{Service: e.ledger, controller: c})
	e.ledger.SetAcceptanceVerifier(crashAcceptance{Service: e.catalog, controller: c})
	if _, err := e.catalog.CommitVersion(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	c.stop("response_lost")
	t.Fatalf("checkpoint %q was not reached", *crashPoint)
}

func TestCommitCrashMatrix(t *testing.T) {
	for _, point := range commitCrashPoints {
		t.Run(point, func(t *testing.T) {
			root := t.TempDir()
			ready := filepath.Join(root, "ready.json")
			childTemp := filepath.Join(root, "child")
			if err := os.Mkdir(childTemp, 0700); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			binarySHA, _, err := fileop.HashFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestCommitCrashChild$", "-test.v", "-commit-crash-child", "-commit-crash-point="+point, "-commit-crash-ready="+ready)
			cmd.Env = append(os.Environ(), "TMPDIR="+childTemp, "TEMP="+childTemp, "TMP="+childTemp)
			var childLog bytes.Buffer
			cmd.Stdout = &childLog
			cmd.Stderr = &childLog
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			killed := false
			defer func() {
				if !killed {
					_ = cmd.Process.Kill()
					<-done
				}
				if *crashEvidence != "" {
					if err := os.MkdirAll(*crashEvidence, 0700); err != nil {
						t.Error(err)
						return
					}
					if err := os.WriteFile(filepath.Join(*crashEvidence, point+".child.log"), childLog.Bytes(), 0600); err != nil {
						t.Error(err)
					}
				}
			}()

			deadline := time.NewTimer(45 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			var h crashHandshake
		waitReady:
			for {
				select {
				case err := <-done:
					killed = true
					t.Fatalf("child exited before checkpoint: %v\n%s", err, childLog.String())
				case <-deadline.C:
					t.Fatalf("checkpoint timed out: %s", point)
				case <-tick.C:
					b, err := os.ReadFile(ready)
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					if err = json.Unmarshal(b, &h); err != nil {
						t.Fatal(err)
					}
					break waitReady
				}
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = <-done
			killed = true
			if err == nil || cmd.ProcessState.Success() {
				t.Fatal("child was not forcibly terminated")
			}
			rel, err := filepath.Rel(childTemp, h.Home)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatal("child data root escaped owned temporary directory")
			}
			clk := clock.NewFake(h.Now)
			opts := application.Options{Instance: operations.Options{Home: h.Home, Clock: clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}}
			offline, err := application.OpenOffline(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			// Start supplies the live common maintenance context while the gate
			// is closed. Observe before dispatch; do not infer durable state from the
			// child handshake or let normal startup recover it before the observation.
			t.Cleanup(func() { _ = offline.Close(context.Background()) })
			var durable crashObservation
			var recovery application.RecoveryReport
			err = offline.Instance.Start(t.Context(), operations.Hook{Name: "identity", Run: offline.Identity.CheckStartup}, operations.Hook{Name: "observe-and-recover", Run: func(ctx context.Context) error {
				durable = observeCrash(t, offline, ctx, h, true)
				if *crashEvidence != "" {
					writeCrashJSON(t, filepath.Join(*crashEvidence, point+".durable.json"), crashReport{BinarySHA256: binarySHA, ChildExit: cmd.ProcessState.String(), Point: point, Platform: runtime.GOOS + "/" + runtime.GOARCH, Termination: "Process.Kill (no graceful shutdown)", OperationID: h.OperationID, Prepared: h.Prepared, Before: h.Observation, Durable: durable})
				}
				assertCrashBoundary(t, point, durable)
				if durable.Stage != h.Observation.Stage || durable.Install.State != h.Observation.Install.State || durable.Visible != h.Observation.Visible {
					t.Fatalf("state differs after kill: %+v / %+v", durable, h.Observation)
				}
				var err error
				recovery, err = offline.Recover(ctx)
				return err
			}})
			if err != nil || len(recovery.NeedsReconciliation) != 0 {
				t.Fatalf("recovery: %+v %v", recovery, err)
			}

			if err = offline.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			a, err := application.Open(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := a.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// Startup repeats the dispatcher, then the original request is replayed.

			result, err := a.Catalog.CommitVersion(t.Context(), h.Request)
			if err != nil {
				t.Fatal(err)
			}
			if result.OperationID != h.OperationID || result.VersionNumber != 1 {
				t.Fatalf("changed operation/number: %+v", result)
			}
			if h.Prepared.VersionID != "" && (result.AssetID != h.Prepared.AssetID || result.VersionID != h.Prepared.VersionID || result.ManifestDigest != h.Prepared.ManifestDigest) {
				t.Fatal("recovery changed prepared identity")
			}
			for range 2 {
				replay, err := a.Catalog.CommitVersion(t.Context(), h.Request)
				if err != nil || replay.OperationID != result.OperationID || replay.VersionID != result.VersionID || replay.VersionNumber != 1 {
					t.Fatalf("idempotent replay: %+v %v", replay, err)
				}
			}
			h.Prepared, err = a.Ledger.PreparedOperation(t.Context(), h.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			after := observeCrash(t, a, t.Context(), h, true)
			if !after.Visible || len(after.Versions) != 1 || len(after.Orphans) != 0 {
				t.Fatalf("incomplete/duplicate result: %+v", after)
			}
			downloads := map[string]string{}
			for _, f := range h.Prepared.Files {
				g, err := a.Storage.IssueReadGrant(t.Context(), storage.ReadRequest{Who: h.Request.Who, AssetID: result.AssetID, VersionID: result.VersionID, Path: f.Path, Purpose: authz.PurposeReference})
				if err != nil {
					t.Fatal(err)
				}
				u, err := url.Parse(g.URL)
				if err != nil {
					t.Fatal(err)
				}
				handle, err := a.Storage.OpenRead(t.Context(), h.Request.Who, g.GrantID, u.Query().Get("sig"))
				if err != nil {
					t.Fatal(err)
				}
				b, err := io.ReadAll(handle.File)
				closeErr := handle.File.Close()
				if err != nil || closeErr != nil || shaOf(b) != f.SHA256 || int64(len(b)) != f.Size || after.Files[f.Path] != f.SHA256 {
					t.Fatalf("download digest %s: %v %v", f.Path, err, closeErr)
				}
				downloads[f.Path] = shaOf(b)
			}
			check, err := a.FSCK(t.Context(), true)
			if err != nil || check.Err() != nil {
				t.Fatalf("deep fsck: %+v %v", check.Findings, err)
			}
			fsinfo, err := fsutil.Inspect(h.Home)
			if err != nil {
				t.Fatal(err)
			}
			report := crashReport{BinarySHA256: binarySHA, ChildExit: cmd.ProcessState.String(), FileSystem: fsinfo, Point: point, Platform: runtime.GOOS + "/" + runtime.GOARCH, Termination: "Process.Kill (no graceful shutdown)", OperationID: h.OperationID, Prepared: h.Prepared, Before: h.Observation, Durable: durable, Recovery: recovery, Result: result, After: after, DownloadSHA256: downloads}
			if *crashEvidence != "" {
				writeCrashJSON(t, filepath.Join(*crashEvidence, point+".json"), report)
			}
			t.Logf("%s: %s/%s -> %s; operation=%s version=%s number=1 files=%d", point, durable.Stage, durable.Install.State, after.Stage, h.OperationID, result.VersionID, len(downloads))
		})
	}
}

func assertCrashBoundary(t *testing.T, point string, o crashObservation) {
	t.Helper()
	stage := commands.StagePrepared
	switch point {
	case "before_frozen_write", "frozen_temp_synced", "frozen_before_prepared":
		stage = ""
	case "ledger_installed", "accepted_before_commit":
		stage = commands.StageInstalled
	case "ledger_committed", "response_lost":
		stage = commands.StageCommitted
	}
	installed := stage == commands.StageInstalled || stage == commands.StageCommitted || point == "storage_installed"
	if o.Stage != stage || (o.Install.State == "installed") != installed {
		t.Fatalf("wrong persistence boundary %s: %+v", point, o)
	}
	visible := stage == commands.StageCommitted
	versions := 0
	if visible {
		versions = 1
	}
	if o.Visible != visible || len(o.Versions) != versions {
		t.Fatalf("wrong visibility at %s: %+v", point, o)
	}
	if stage != "" && !visible && o.ReadCode != errcode.NotFound {
		t.Fatalf("pending bytes downloadable: %+v", o)
	}
	finalFiles := 0
	if installed || point == "renamed_before_sync" || point == "placement_synced" {
		finalFiles = 3
	}
	if len(o.Files) != finalFiles {
		t.Fatalf("unexpected installed bytes at %s: %+v", point, o.Files)
	}
	frozen := 1
	if point == "before_frozen_write" || point == "response_lost" {
		frozen = 0
	}
	if len(o.Frozen) != frozen {
		t.Fatalf("wrong frozen boundary at %s: %+v", point, o.Frozen)
	}
}
