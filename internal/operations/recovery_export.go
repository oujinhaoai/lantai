package operations

import (
	"context"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// RecoveryPackageAccessError carries a bounded, non-sensitive diagnostic. It
// never includes filesystem paths or recovery material.
type RecoveryPackageAccessError struct{ Reason string }

func (e *RecoveryPackageAccessError) Error() string { return e.Reason }

func packageAccessError(reason string) error {
	return &RecoveryPackageAccessError{Reason: "backup_package_" + reason}
}

// OpenExportRecoveryPackage applies the same file and byte checks at publication
// and download. Verification and streaming use the same descriptor. It does not
// authorize export; the caller must separately validate the current human grant
// and compare the returned identity with its source-owned completion record.
func OpenExportRecoveryPackage(ctx context.Context, name string) (*os.File, RecoveryPackageInfo, error) {
	var zero RecoveryPackageInfo
	parent := filepath.Dir(name)
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || !filepath.IsAbs(name) || filepath.Clean(name) != name || resolved != parent {
		return nil, zero, packageAccessError("path_unsafe")
	}
	if err = fsutil.CheckPrivate(parent); err != nil {
		return nil, zero, packageAccessError("permissions")
	}
	dir, err := os.Lstat(parent)
	if err != nil || !dir.IsDir() {
		return nil, zero, packageAccessError("path_unsafe")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, zero, packageAccessError("unavailable")
	}
	defer root.Close()
	openedDir, err := root.Stat(".")
	if err != nil || !os.SameFile(dir, openedDir) {
		return nil, zero, packageAccessError("changed")
	}
	base := filepath.Base(name)
	check := func(st os.FileInfo) error {
		if !st.Mode().IsRegular() {
			return packageAccessError("file_type")
		}
		if err := recoveryExportMetadata(st); err != nil {
			return err
		}
		return nil
	}
	before, err := root.Lstat(base)
	if err != nil {
		return nil, zero, packageAccessError("unavailable")
	}
	if err = check(before); err != nil {
		return nil, zero, err
	}
	if err = fsutil.CheckPrivateFile(name); err != nil {
		return nil, zero, packageAccessError("permissions")
	}
	f, err := root.OpenFile(base, os.O_RDONLY|recoveryExportOpenFlags, 0)
	if err != nil {
		return nil, zero, packageAccessError("unavailable")
	}
	good := false
	defer func() {
		if !good {
			_ = f.Close()
		}
	}()
	checkIdentity := func() error {
		currentDir, e := os.Lstat(parent)
		if e != nil || !os.SameFile(dir, currentDir) || !currentDir.IsDir() {
			return packageAccessError("changed")
		}
		if fsutil.CheckPrivate(parent) != nil || fsutil.CheckPrivateFile(name) != nil {
			return packageAccessError("permissions")
		}
		after, e := f.Stat()
		if e != nil || !os.SameFile(before, after) {
			return packageAccessError("changed")
		}
		if e = check(after); e != nil {
			return e
		}
		current, e := root.Lstat(base)
		if e != nil || !os.SameFile(after, current) {
			return packageAccessError("changed")
		}
		return check(current)
	}
	if err = checkIdentity(); err != nil {
		return nil, zero, err
	}
	info, err := VerifyOpenRecoveryPackage(ctx, f)
	if err != nil {
		if ctx.Err() != nil {
			return nil, zero, ctx.Err()
		}
		return nil, zero, packageAccessError("integrity")
	}
	if err = checkIdentity(); err != nil {
		return nil, zero, err
	}
	if _, err = f.Seek(0, 0); err != nil {
		return nil, zero, packageAccessError("unavailable")
	}
	good = true
	return f, info, nil
}
