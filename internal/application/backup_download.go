package application

import (
	"context"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/operations"
)

type backupPackageReader struct{ *os.File }

// OpenPackage derives its path only from a source-owned completed job. It
// revalidates root identity, privacy and bytes before releasing a descriptor.
func (b *BackupControl) OpenPackage(ctx context.Context, id ids.ID, expected string) (backupmgmt.PackageReader, backupmgmt.Package, error) {
	var zero backupmgmt.Package
	b.mu.Lock()
	j, ok := b.jobs[id]
	active := b.active != ""
	b.mu.Unlock()
	if !ok || !id.Valid() {
		return nil, zero, backupmgmt.ErrNotFound
	}
	if active {
		return nil, zero, backupmgmt.ErrBusy
	}
	if j.State != "complete" || j.Package == nil || expected != j.Package.SHA256 {
		return nil, zero, backupmgmt.ErrInvalid
	}
	if err := b.checkDestination(); err != nil {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	name := operations.RecoveryPackagePath(filepath.Join(b.root, string(id)))
	st, err := os.Lstat(name)
	if err != nil {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || !backupSingleLink(st) || !backupPrivateOwner(st) {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	good := false
	defer func() {
		if !good {
			_ = f.Close()
		}
	}()
	after, err := f.Stat()
	if err != nil || !os.SameFile(st, after) {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	info, err := operations.VerifyOpenRecoveryPackage(ctx, f)
	if err != nil {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	if info.SHA256 != expected || info.Size != j.Package.Size || info.Manifest.PackageID != j.Package.ID || info.Manifest.BackupID != j.BackupID {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	if _, err = f.Seek(0, 0); err != nil {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	good = true
	return backupPackageReader{f}, *j.Package, nil
}
