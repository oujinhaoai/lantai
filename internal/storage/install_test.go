package storage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/install/installtest"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// 真实安装器通过与内存桩相同的契约套件。
func TestInstallerSatisfiesContract(t *testing.T) {
	installtest.RunInstallerContract(t, func(t *testing.T) installtest.Harness {
		f := newFixture(t, testConfig(), nil)
		return installtest.Harness{
			Installer: f.svc,
			Put: func(op, project ids.ID, path string, content []byte) install.File {
				f.grantForOperation(op, project, content)
				return install.File{Path: path, SHA256: shaOf(content), Size: int64(len(content))}
			},
			Corrupt: func(op ids.ID) {
				row, err := f.svc.installRow(t.Context(), f.svc.db, op)
				if err != nil || row == nil {
					t.Fatalf("corrupt: %v", err)
				}
				files, _ := f.svc.filesOf(t.Context(), f.svc.db, row.VersionID)
				p := filepath.Join(f.svc.layout.VersionDir(row.ProjectID, row.AssetID, row.VersionNumber), FilesDir, filepath.FromSlash(files[0].Path))
				os.Remove(p) // 断开硬链接后写入不同内容，模拟被绕过服务端改动
				if err := os.WriteFile(p, []byte("tampered bytes of another length"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}

func installFixture(t *testing.T, faults *fileop.Faults) (*fixture, install.Request) {
	t.Helper()
	f := newFixture(t, testConfig(), faults)
	content := map[string][]byte{"README.md": []byte("# asset\n"), "model/body.glb": synthetic("body", 3000)}
	var blobs [][]byte
	for _, c := range content {
		blobs = append(blobs, c)
	}
	u := f.uploadAll(f.who, f.project, blobs...)
	fs := files(content)
	req := install.Request{OperationID: u.OperationID, ProjectID: f.project, AssetID: ids.New(), VersionID: ids.New(),
		VersionNumber: 1, ManifestDigest: f.manifestDigest(fs), Files: fs, Manifest: testManifest(fs)}
	return f, req
}

func TestInstallLayoutAndReadOnlyVersions(t *testing.T) {
	f, req := installFixture(t, nil)
	p, err := f.svc.Install(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	dir := f.svc.layout.VersionDir(req.ProjectID, req.AssetID, 1)
	if filepath.Base(dir) != "v001" {
		t.Fatalf("version dir = %s", dir)
	}
	body := filepath.Join(dir, FilesDir, "model", "body.glb")
	st, err := os.Stat(body)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := os.Stat(f.svc.layout.BlobPath(shaOf(synthetic("body", 3000))))
	if !os.SameFile(st, blob) {
		t.Fatal("version files should be hard links to the content store")
	}
	if manifest, _ := os.ReadFile(filepath.Join(dir, ManifestFile)); string(manifest) != string(req.Manifest) {
		t.Fatal("manifest file differs")
	}
	if p.InstallRef != f.svc.layout.ref(dir) || p.ManifestSHA256 != shaOf(req.Manifest) {
		t.Fatalf("proof = %+v", p)
	}
	if _, err := os.Stat(f.svc.layout.installStaging(req.OperationID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("private install area left behind")
	}
	// 授权已被本操作消费：会话到期后同一操作重入仍返回原证明。
	p2, err := f.svc.Install(t.Context(), req)
	if err != nil || p2.InstalledAt != p.InstalledAt {
		t.Fatalf("replay = %+v %v", p2, err)
	}
}

// 崩溃发生在整体改名之后、记录之前：重入接管带相同标记的目录，不产生第二个
// 版本目录；标记属于别的操作时拒绝，不覆盖。
func TestInstallAdoptsItsOwnDirectoryAfterCrash(t *testing.T) {
	f, req := installFixture(t, nil)
	rd, _ := requestDigest(req, shaOf(req.Manifest))
	final := f.svc.layout.VersionDir(req.ProjectID, req.AssetID, req.VersionNumber)
	at := clock.Truncate(f.clk.Now())
	if _, err := f.svc.assemble(req, rd, at); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.place(req.OperationID, final); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(1000)
	f.restart()
	p, err := f.svc.Install(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !p.InstalledAt.Equal(at) {
		t.Fatalf("adopted install time = %s, want the time recorded in the marker %s", p.InstalledAt, at)
	}
	if err := f.svc.Verify(t.Context(), p); err != nil {
		t.Fatal(err)
	}

	other := req
	other.OperationID = ids.New()
	other.VersionID = ids.New()
	f.grantForOperation(other.OperationID, other.ProjectID, []byte("# asset\n"))
	f.grantForOperation(other.OperationID, other.ProjectID, synthetic("body", 3000))
	_, err = f.svc.Install(t.Context(), other) // 同一版本号的目录已属于别的操作
	wantReason(t, err, errcode.OperationNeedsReconciliation, "version_already_installed")
}

func TestInstallFallsBackToVerifiedCopies(t *testing.T) {
	var blockLinks bool
	faults := &fileop.Faults{Link: func(string, string) error {
		if blockLinks {
			return syscall.EXDEV
		}
		return nil
	}}
	f, req := installFixture(t, faults)
	blockLinks = true
	p, err := f.svc.Install(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := f.svc.installRow(t.Context(), f.svc.db, req.OperationID)
	if row.LinkMode != string(fileop.ModeCopy) {
		t.Fatalf("link mode = %s", row.LinkMode)
	}
	body := filepath.Join(f.svc.layout.VersionDir(req.ProjectID, req.AssetID, 1), FilesDir, "model", "body.glb")
	got, _, err := fileop.HashFile(body)
	if err != nil || got != shaOf(synthetic("body", 3000)) {
		t.Fatalf("copied file hash = %s %v", got, err)
	}
	if err := f.svc.Verify(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.VerifyDeep(t.Context(), req.OperationID); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFailuresAreStableAndRecoverable(t *testing.T) {
	var mode string
	faults := &fileop.Faults{
		Link: func(string, string) error {
			if mode == "full" {
				return syscall.EXDEV // 不支持硬链接，退回复制
			}
			return nil
		},
		Write: func(string) error {
			if mode == "full" {
				return syscall.ENOSPC
			}
			return nil
		},
		Rename: func(string, string) error {
			if mode == "busy" {
				return syscall.EBUSY
			}
			return nil
		},
	}
	f, req := installFixture(t, faults)
	mode = "full"
	_, err := f.svc.Install(t.Context(), req)
	wantCode(t, err, errcode.StorageFull)
	mode = "busy"
	_, err = f.svc.Install(t.Context(), req)
	wantCode(t, err, errcode.StorageUnavailable)
	if row, _ := f.svc.installRow(t.Context(), f.svc.db, req.OperationID); row != nil {
		t.Fatal("a failed install must not be recorded")
	}
	if _, err := os.Stat(f.svc.layout.VersionDir(req.ProjectID, req.AssetID, 1)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a failed install exposed a version directory")
	}
	mode = ""
	p, err := f.svc.Install(t.Context(), req)
	if err != nil {
		t.Fatalf("retry after the fault cleared: %v", err)
	}
	if err := f.svc.Verify(t.Context(), p); err != nil {
		t.Fatal(err)
	}
}

func TestInstallNeedsGrantsOfThisOperation(t *testing.T) {
	f, req := installFixture(t, nil)
	stolen := req
	stolen.OperationID = ids.New() // 同一项目、同样的哈希，但不是授权的操作
	_, err := f.svc.Install(t.Context(), stolen)
	wantCode(t, err, errcode.BlobGrantRequired)
	elsewhere := req
	elsewhere.ProjectID = ids.New()
	_, err = f.svc.Install(t.Context(), elsewhere)
	wantCode(t, err, errcode.BlobGrantRequired)
	// 会话取消后未消费的授权失效。
	u, err := f.svc.UploadForOperation(t.Context(), f.who, req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CancelUpload(t.Context(), f.who, u.UploadID); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Install(t.Context(), req)
	wantCode(t, err, errcode.BlobGrantRequired)
}

func TestQuarantineKeepsBytesAndScanReportsOrphans(t *testing.T) {
	f, req := installFixture(t, nil)
	p, err := f.svc.Install(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Quarantine(t.Context(), req.OperationID, errcode.Forbidden); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Quarantine(t.Context(), req.OperationID, errcode.Forbidden); err != nil {
		t.Fatal("quarantine must be idempotent:", err)
	}
	q := f.svc.layout.quarantineDir(req.OperationID)
	if _, err := os.Stat(filepath.Join(q, FilesDir, "model", "body.glb")); err != nil {
		t.Fatal("quarantine must keep the bytes")
	}
	if _, err := os.Stat(f.svc.layout.VersionDir(req.ProjectID, req.AssetID, 1)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("quarantined version directory is still in place")
	}
	if err := f.svc.Verify(t.Context(), p); err == nil {
		t.Fatal("a quarantined install still verifies")
	}
	st, _ := f.svc.InstallStatus(t.Context(), req.OperationID)
	if st.State != "quarantined" || st.QuarantineReason != errcode.Forbidden {
		t.Fatalf("status = %+v", st)
	}

	// 没有安装记录的版本目录与残留的私有安装区只被报告，不被补记或删除。
	orphan := f.svc.layout.VersionDir(f.project, ids.New(), 7)
	if err := os.MkdirAll(filepath.Join(orphan, FilesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	stage := f.svc.layout.installStaging(ids.New())
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	orphans, err := f.svc.ScanOrphans(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]bool{}
	for _, o := range orphans {
		reasons[o.Reason] = true
	}
	if len(orphans) != 2 || !reasons["no_install_marker"] || !reasons["unplaced_install_staging"] {
		t.Fatalf("orphans = %+v", orphans)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("the scan must not delete anything")
	}
}

func TestInstallAdoptionRetriesDirectorySync(t *testing.T) {
	for _, parent := range []string{"version", "staging"} {
		t.Run(parent, func(t *testing.T) {
			var final, syncDir string
			failed := true
			faults := &fileop.Faults{Sync: func(path string) error {
				if failed && path == syncDir {
					if _, err := os.Stat(final); err == nil {
						return syscall.EIO
					}
				}
				return nil
			}}
			f, req := installFixture(t, faults)
			final = f.svc.layout.VersionDir(req.ProjectID, req.AssetID, req.VersionNumber)
			syncDir = filepath.Dir(final)
			if parent == "staging" {
				syncDir = filepath.Dir(f.svc.layout.installStaging(req.OperationID))
			}
			for i := 0; i < 2; i++ {
				_, err := f.svc.Install(t.Context(), req)
				wantCode(t, err, errcode.StorageUnavailable)
				if row, err := f.svc.installRow(t.Context(), f.svc.db, req.OperationID); err != nil || row != nil {
					t.Fatalf("attempt %d accepted an unsynced directory: %+v %v", i, row, err)
				}
				f.restart()
			}
			failed = false
			p, err := f.svc.Install(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.svc.Verify(t.Context(), p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestQuarantineAfterPlacementBeforeRecord(t *testing.T) {
	f, req := installFixture(t, nil)
	rd, _ := requestDigest(req, shaOf(req.Manifest))
	final := f.svc.layout.VersionDir(req.ProjectID, req.AssetID, req.VersionNumber)
	if _, err := f.svc.assemble(req, rd, f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.place(req.OperationID, final); err != nil {
		t.Fatal(err)
	}
	f.restart()
	orphans, err := f.svc.ScanOrphans(t.Context())
	if err != nil || len(orphans) != 1 || orphans[0].directory != final {
		t.Fatalf("orphan scan must preserve original ULID path casing: %+v %v", orphans, err)
	}
	if err := f.svc.Quarantine(t.Context(), req.OperationID, errcode.Forbidden); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(final); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unrecorded version was not moved into quarantine: %v", err)
	}
	qdir := f.svc.layout.quarantineDir(req.OperationID)
	if data, err := os.ReadFile(filepath.Join(qdir, ManifestFile)); err != nil || string(data) != string(req.Manifest) {
		t.Fatalf("quarantine lost the manifest: %v", err)
	}
	if _, err := f.svc.Install(t.Context(), req); errcode.CodeOf(err) != errcode.OperationNeedsReconciliation {
		t.Fatalf("quarantined operation installed again: %v", err)
	}
}

func TestQuarantineDoesNotIgnoreOccupiedDestination(t *testing.T) {
	f, req := installFixture(t, nil)
	p, err := f.svc.Install(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	qdir := f.svc.layout.quarantineDir(req.OperationID)
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(qdir, "preserved"), []byte("prior evidence"), 0o444); err != nil {
		t.Fatal(err)
	}
	err = f.svc.Quarantine(t.Context(), req.OperationID, errcode.Forbidden)
	wantReason(t, err, errcode.OperationNeedsReconciliation, "quarantine_dir_occupied")
	if err := f.svc.Verify(t.Context(), p); err != nil {
		t.Fatalf("failed quarantine altered the install record: %v", err)
	}
}

func TestQuarantineRetriesSyncAndKeepsItsOriginalReason(t *testing.T) {
	var qparent string
	failed := true
	faults := &fileop.Faults{Sync: func(path string) error {
		if failed && path == qparent {
			return syscall.EIO
		}
		return nil
	}}
	f, req := installFixture(t, faults)
	if _, err := f.svc.Install(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	qdir := f.svc.layout.quarantineDir(req.OperationID)
	qparent = filepath.Dir(qdir)
	// 先创建父目录，使故障准确落在移动与标记后的目录刷盘。
	if err := os.MkdirAll(qparent, 0o755); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.svc.Quarantine(t.Context(), req.OperationID, errcode.Forbidden), errcode.StorageUnavailable)
	state, err := f.svc.InstallStatus(t.Context(), req.OperationID)
	if err != nil || state.State != "installed" {
		t.Fatalf("quarantine was recorded before directory sync: %+v %v", state, err)
	}
	f.restart()
	f.clk.Advance(time.Second)
	failed = false
	if err := f.svc.Quarantine(t.Context(), req.OperationID, errcode.HashMismatch); err != nil {
		t.Fatal(err)
	}
	state, err = f.svc.InstallStatus(t.Context(), req.OperationID)
	if err != nil || state.State != "quarantined" || state.QuarantineReason != errcode.Forbidden {
		t.Fatalf("retry changed the immutable quarantine evidence: %+v %v", state, err)
	}
}

func TestCASPublicationRetriesDirectorySyncBeforeGrant(t *testing.T) {
	var blobDir string
	failed := true
	faults := &fileop.Faults{Sync: func(path string) error {
		if failed && path == blobDir {
			return syscall.EIO
		}
		return nil
	}}
	f := newFixture(t, testConfig(), faults)
	content := []byte("verified content whose directory must be durable")
	u := f.createUpload(f.who, f.project, content)
	f.putAll(f.who, u, content)
	sha := shaOf(content)
	blobDir = filepath.Dir(f.svc.layout.BlobPath(sha))
	for i := 0; i < 2; i++ {
		_, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, sha)
		wantCode(t, err, errcode.StorageUnavailable)
		current, err := f.svc.GetUpload(t.Context(), f.who, u.UploadID)
		if err != nil || current.Files[0].State != FilePending {
			t.Fatalf("attempt %d accepted content before sync: %+v %v", i, current, err)
		}
	}
	failed = false
	if _, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, sha); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.VerifyBlob(t.Context(), "invalid"); errcode.CodeOf(err) != errcode.SchemaInvalid {
		t.Fatalf("invalid content hash: %v", err)
	}
}
