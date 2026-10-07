package integration

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Error injection is intentionally separate from TestCommitCrashMatrix and
// native file-system tests: returning ENOSPC is not a real full disk or a kill.
func TestCommitFileFaultMatrix(t *testing.T) {
	for _, mode := range []string{"copy_full", "rename_busy", "file_sync", "directory_sync", "no_hardlinks"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
			req, op := crashRequest(t, e)
			stage := filepath.Join(e.inst.Layout().Home, "staging", "install", string(op))
			within := func(path string) bool {
				rel, err := filepath.Rel(stage, path)
				return err == nil && filepath.IsLocal(rel)
			}
			active := true
			fs := &fileop.Faults{
				Link: func(_, dst string) error {
					if active && within(dst) && (mode == "copy_full" || mode == "no_hardlinks") {
						return syscall.EXDEV
					}
					return nil
				},
				Write: func(path string) error {
					if active && within(path) && mode == "copy_full" {
						return syscall.ENOSPC
					}
					return nil
				},
				Rename: func(src, _ string) error {
					if active && src == stage && mode == "rename_busy" {
						return syscall.EBUSY
					}
					return nil
				},
				Sync: func(path string) error {
					if active && mode == "file_sync" && filepath.Dir(path) == stage && filepath.Base(path) != "files" {
						if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
							return syscall.EIO
						}
					}
					if active && mode == "directory_sync" && path == filepath.Dir(stage) {
						if _, err := os.Stat(stage); errors.Is(err, os.ErrNotExist) {
							return syscall.EIO
						}
					}
					return nil
				},
			}
			crashWire(t, e, fs, e.ledger)
			v, err := e.catalog.CommitVersion(t.Context(), req)
			p, prepErr := e.ledger.PreparedOperation(t.Context(), op)
			if prepErr != nil {
				t.Fatal(prepErr)
			}
			if mode == "no_hardlinks" {
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range p.Files {
					src, err := os.Stat(e.storage.Layout().BlobPath(f.SHA256))
					if err != nil {
						t.Fatal(err)
					}
					dst, err := os.Stat(filepath.Join(e.storage.Layout().VersionDir(p.ProjectID, p.AssetID, p.VersionNumber), storage.FilesDir, filepath.FromSlash(f.Path)))
					if err != nil {
						t.Fatal(err)
					}
					if os.SameFile(src, dst) {
						t.Fatal("fallback did not copy")
					}
				}
			} else {
				want := errcode.StorageUnavailable
				if mode == "copy_full" {
					want = errcode.StorageFull
				}
				if errcode.CodeOf(err) != want {
					t.Fatalf("fault %s: %v", mode, err)
				}
				if _, err = e.ledger.Version(t.Context(), p.AssetID, p.VersionID); errcode.CodeOf(err) != errcode.NotFound {
					t.Fatalf("failed install visible: %v", err)
				}
				view, err := e.ledger.Operation(t.Context(), op)
				if err != nil || view.Stage != commands.StagePrepared {
					t.Fatalf("fault lost resumable operation: %+v %v", view, err)
				}
			}
			active = false
			a := reopenApplication(t, e)
			replay, err := a.Catalog.CommitVersion(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if replay.OperationID != op || replay.VersionID != p.VersionID || replay.VersionNumber != p.VersionNumber {
				t.Fatal("recovery changed reservation")
			}
			if mode == "no_hardlinks" && replay.VersionID != v.VersionID {
				t.Fatal("successful fallback replay changed version")
			}
			inv, err := a.Ledger.RecoveryInventory(t.Context())
			if err != nil || len(inv.Versions) != 1 {
				t.Fatalf("duplicate version: %+v %v", inv.Versions, err)
			}
			if err = a.Storage.VerifyDeep(t.Context(), op); err != nil {
				t.Fatal(err)
			}
			t.Logf("error-injection %s: original operation=%s version=%s number=%d; deep hashes verified", mode, op, p.VersionID, p.VersionNumber)
		})
	}
}

func TestCommitRejectsUnsafeManifestBeforeWriting(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	req, op := crashRequest(t, e)
	outside := filepath.Join(t.TempDir(), "outside.md")
	cases := map[string][]string{
		"absolute": {outside}, "traversal": {"../escape.md"}, "windows_absolute": {"C:/escape.md"},
		"reserved": {"CON.txt"}, "case_conflict": {"a.md", "A.md"}, "parent_conflict": {"a.md", "a.md/child"},
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			bad := req
			bad.Content.Files = append([]manifest.InputFile(nil), req.Content.Files...)
			for i, path := range paths {
				bad.Content.Files[i].Path = path
			}
			if _, err := e.catalog.CommitVersion(t.Context(), bad); errcode.CodeOf(err) != errcode.SchemaInvalid && errcode.CodeOf(err) != errcode.PathConflict {
				t.Fatalf("unsafe path accepted: %v", err)
			}
			if _, err := e.ledger.PreparedOperation(t.Context(), op); errcode.CodeOf(err) != errcode.NotFound {
				t.Fatalf("unsafe input reserved: %v", err)
			}
			for _, path := range []string{outside, filepath.Join(e.inst.Layout().Home, "escape.md"), filepath.Join(e.inst.Layout().Home, "staging", "frozen", string(op)), filepath.Join(e.inst.Layout().Home, "staging", "install", string(op))} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsafe input wrote bytes: %v", err)
				}
			}
		})
	}
}

func TestCommitOrphansNeverCreateAuthority(t *testing.T) {
	for _, mode := range []string{"no_marker", "unknown_operation"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
			asset, op := ids.New(), ids.New()
			dir := e.storage.Layout().VersionDir(e.project.ProjectID, asset, 7)
			if err := os.MkdirAll(filepath.Join(dir, storage.FilesDir), 0700); err != nil {
				t.Fatal(err)
			}
			body := []byte("synthetic orphan, no ledger evidence")
			if err := os.WriteFile(filepath.Join(dir, storage.FilesDir, "a.md"), body, 0600); err != nil {
				t.Fatal(err)
			}
			reason := "no_install_marker"
			if mode == "unknown_operation" {
				writeCrashJSON(t, filepath.Join(dir, ".install.json"), map[string]any{"contract": "lantai.install-marker/v1", "operation_id": op, "request_digest": digest.Of([]byte("synthetic unknown request")), "installed_at": e.clk.Now().Format("2006-01-02T15:04:05.000Z")})
				reason = "no_install_record"
			}
			before, err := e.storage.ScanOrphans(t.Context())
			if err != nil || len(before) != 1 || before[0].Reason != reason {
				t.Fatalf("orphan reason: %+v %v", before, err)
			}
			a := reopenApplication(t, e)
			check, err := a.FSCK(t.Context(), true)
			if err != nil || check.Err() != nil || len(check.Residuals) != 1 || check.Residuals[0].Reason != reason {
				t.Fatalf("fsck: %+v %v", check.Residuals, err)
			}
			inv, err := a.Ledger.RecoveryInventory(t.Context())
			if err != nil || len(inv.Versions) != 0 || len(inv.Open) != 0 {
				t.Fatalf("orphan manufactured authority: %+v %v", inv, err)
			}
			if _, err = a.Ledger.Asset(t.Context(), asset); errcode.CodeOf(err) != errcode.NotFound {
				t.Fatal("orphan became an asset", err)
			}
			path := filepath.Join(dir, storage.FilesDir, "a.md")
			sha, _, err := fileop.HashFile(path)
			if err != nil || sha != shaOf(body) {
				t.Fatal("read-only recovery lost evidence", err)
			}
			if mode == "unknown_operation" {
				if err = a.Ledger.QuarantineOrphan(t.Context(), op); err != nil {
					t.Fatal(err)
				}
				status, err := a.Storage.InstallStatus(t.Context(), op)
				if err != nil || status.State != "quarantined" || status.QuarantineReason != errcode.OperationNeedsReconciliation {
					t.Fatalf("quarantine: %+v %v", status, err)
				}
				sha, _, err = fileop.HashFile(filepath.Join(a.Instance.Layout().Home, "quarantine", string(op), storage.FilesDir, "a.md"))
				if err != nil || sha != shaOf(body) {
					t.Fatal("quarantine lost bytes", err)
				}
				inv, err = a.Ledger.RecoveryInventory(t.Context())
				if err != nil || len(inv.Versions) != 0 {
					t.Fatal("quarantine created a version", err)
				}
			}
			t.Logf("%s: reason=%s sha256=%s; no version allocated", mode, reason, shaOf(body))
		})
	}
}
