package application

import (
	"context"
	"errors"
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
	f, info, err := operations.OpenExportRecoveryPackage(ctx, name)
	if err != nil {
		var access *operations.RecoveryPackageAccessError
		if errors.As(err, &access) {
			return nil, zero, &backupmgmt.PackageUnavailableError{Reason: access.Reason}
		}
		return nil, zero, backupmgmt.ErrUnavailable
	}
	good := false
	defer func() {
		if !good {
			_ = f.Close()
		}
	}()
	if info.SHA256 != expected || info.Size != j.Package.Size || info.Manifest.PackageID != j.Package.ID || info.Manifest.BackupID != j.BackupID {
		return nil, zero, &backupmgmt.PackageUnavailableError{Reason: "backup_package_identity"}
	}
	if _, err = f.Seek(0, 0); err != nil {
		return nil, zero, backupmgmt.ErrUnavailable
	}
	good = true
	return backupPackageReader{f}, *j.Package, nil
}
