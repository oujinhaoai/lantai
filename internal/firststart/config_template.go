package firststart

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

// This reserved directory is a bounded bootstrap transaction, not a glob of
// disposable files. Only a digest-bound config part and legacy.empty are recognized. Unknown
// entries, unsafe metadata and bytes not matching the trusted input fail closed.
const configBootstrapDir = ".lantai-config-bootstrap-v1"

var configPartName = regexp.MustCompile(`^config-[0-9a-f]{64}\.part$`)

func configPartPath(tx string, raw []byte) string {
	return filepath.Join(tx, fmt.Sprintf("config-%x.part", sha256.Sum256(raw)))
}

type seedStage string

const (
	seedAfterFileSync seedStage = "after_file_sync"
	seedAfterPublish  seedStage = "after_publish"
)

// SeedConfigTemplate preserves an existing valid config. Otherwise it seeds a
// new empty root, or migrates the old deployment's read-only config bind into an
// owned file after read-only instance/database/key checks. It never generates or
// copies keys, changes identity, resumes a restore or removes unknown files.
func SeedConfigTemplate(home, template string) error {
	return seedConfigTemplate(home, template, nil)
}

// observe is per invocation and package-private: no production flag or global
// crash hook. Tests kill a child at real post-fsync / post-publication boundaries.
func seedConfigTemplate(home, template string, observe func(seedStage) error) error {
	if template == "" {
		return nil
	}
	l, err := operations.NewLayout(home)
	if err != nil {
		return err
	}
	st, err := os.Lstat(l.Home)
	if err != nil || !st.IsDir() || !privateMetadata(st) || st.Mode().Perm()&0o022 != 0 {
		return errors.New("config bootstrap requires a service-owned data directory without shared write access")
	}
	lock, err := fsutil.TryLock(l.LockPath())
	if err != nil {
		return err
	}
	defer lock.Unlock()
	tx := filepath.Join(l.Home, configBootstrapDir)
	txExists, err := inspectBootstrap(tx)
	if err != nil {
		return err
	}
	existing, err := os.Lstat(l.ConfigPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && (!existing.Mode().IsRegular() || existing.Mode().Perm()&0o022 != 0) {
		return errors.New("existing configuration is linked, nonregular or shared-writable; refusing to replace it")
	}
	if existing != nil && existing.Size() > 0 {
		// Validate before ignoring the template: invalid data is retained with an
		// explicit error, never silently repaired from a possibly different input.
		raw, e := readConfigFile(l.ConfigPath(), txExists)
		if e != nil {
			return fmt.Errorf("existing configuration retained: %w", e)
		}
		if _, e = operations.ParseConfig(l, raw); e != nil {
			return fmt.Errorf("existing configuration retained: %w", e)
		}
		if !txExists {
			return nil
		}
		return finishPublishedBootstrap(l, tx, existing, raw)
	}
	if existing != nil && !legacyEmptyConfig(existing) {
		return errors.New("empty configuration is not a recognized legacy Docker placeholder; retained")
	}
	marker, markerErr := operations.ReadMarker(l)
	legacy := markerErr == nil
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return markerErr
	}
	if legacy {
		if marker.Restore != nil {
			if txExists {
				return errors.New("bootstrap transaction conflicts with restore ownership; retained")
			}
			// The recovery owner persists its own target configuration and resume latch.
			// Do not import a deployment template or block that owner's safe retry.
			return nil
		}
		if marker.State != operations.MarkerActive || marker.Migration != nil {
			return errors.New("configuration missing during initialization, restore or data migration; bootstrap refuses to replace that owner's state")
		}
	} else {
		if existing != nil {
			return errors.New("unmarked legacy configuration placeholder; refusing initialization")
		}
		if txExists {
			if _, e := os.Lstat(filepath.Join(tx, "legacy.empty")); e == nil {
				return errors.New("legacy bootstrap witness without an active instance; retained")
			} else if !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
		entries, e := os.ReadDir(l.Home)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			if entry.Name() != "lantai.lock" && entry.Name() != configBootstrapDir {
				return fmt.Errorf("unmarked nonempty data retained: unexpected %s; bootstrap will not remove it", entry.Name())
			}
		}
	}
	raw, err := readConfigFile(template, false)
	if err != nil {
		return fmt.Errorf("configuration input unavailable; no configuration installed: %w", err)
	}
	cfg, err := operations.ParseConfig(l, raw)
	if err != nil {
		return err
	}
	if err = validateOrigins(cfg.HTTP.AllowedOrigins); err != nil {
		return err
	}
	if legacy {
		if err = checkLegacyConfig(l, raw, cfg); err != nil {
			return fmt.Errorf("legacy configuration migration refused; existing files retained: %w", err)
		}
	}
	if !txExists {
		if err = os.Mkdir(tx, 0700); err != nil {
			return err
		}
		if err = fsutil.SyncDir(l.Home); err != nil {
			return err
		}
	}
	part := configPartPath(tx, raw)
	entries, err := os.ReadDir(tx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if configPartName.MatchString(entry.Name()) && filepath.Join(tx, entry.Name()) != part {
			return errors.New("bootstrap input digest changed; existing part retained")
		}
	}
	if err = prepareConfigPart(part, raw); err != nil {
		return err
	}
	if observe != nil {
		if err = observe(seedAfterFileSync); err != nil {
			return cleanupUnpublishedBootstrap(tx, part, err)
		}
	}
	// A bind-created empty file has no configuration bytes to preserve. Retain it
	// in this transaction until the complete replacement is durably published.
	if existing != nil {
		placeholder := filepath.Join(tx, "legacy.empty")
		if _, e := os.Lstat(placeholder); !errors.Is(e, os.ErrNotExist) {
			return errors.New("ambiguous legacy placeholder state; retained")
		}
		now, e := os.Lstat(l.ConfigPath())
		if e != nil || !os.SameFile(existing, now) || !legacyEmptyConfig(now) {
			return errors.New("legacy placeholder changed; retained")
		}
		if err = os.Rename(l.ConfigPath(), placeholder); err != nil {
			return err
		}
		if err = fsutil.SyncDir(tx); err != nil {
			return err
		}
		if err = fsutil.SyncDir(l.Home); err != nil {
			return err
		}
	}
	// Hard link is an atomic no-replace operation in the same data filesystem.
	// Unsupported filesystems fail; never fall back to a clobbering Rename.
	if err = os.Link(part, l.ConfigPath()); err != nil {
		return fmt.Errorf("publish configuration without replacement: %w", err)
	}
	if err = fsutil.SyncDir(l.Home); err != nil {
		return err
	}
	if observe != nil {
		if err = observe(seedAfterPublish); err != nil {
			return err
		}
	}
	published, err := os.Lstat(l.ConfigPath())
	if err != nil {
		return err
	}
	return finishPublishedBootstrap(l, tx, published, raw)
}

func readConfigFile(path string, allowLinked bool) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > 64<<10 {
		return nil, errors.New("configuration must be a nonempty regular file of at most 64 KiB")
	}
	if !allowLinked && !singleConfigLink(st) {
		return nil, errors.New("configuration must not have multiple hard links")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(st, after) {
		return nil, errors.New("configuration changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, errors.New("configuration has an invalid size")
	}
	return raw, nil
}

func inspectBootstrap(tx string) (bool, error) {
	st, err := os.Lstat(tx)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.IsDir() || !privateMetadata(st) || st.Mode().Perm() != 0700 {
		return false, errors.New("unsafe bootstrap transaction directory retained")
	}
	entries, err := os.ReadDir(tx)
	if err != nil {
		return false, err
	}
	parts := 0
	for _, entry := range entries {
		info, e := entry.Info()
		if e != nil {
			return false, e
		}
		switch {
		case configPartName.MatchString(entry.Name()):
			parts++
			if parts > 1 {
				return false, errors.New("multiple bootstrap parts retained")
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !bootstrapPartOwner(info) || info.Size() > 64<<10 {
				return false, errors.New("unsafe bootstrap part retained")
			}
		case entry.Name() == "legacy.empty":
			if !legacyEmptyConfig(info) {
				return false, errors.New("unrecognized legacy placeholder retained")
			}
		default:
			return false, fmt.Errorf("unknown bootstrap entry %s retained", entry.Name())
		}
	}
	return true, nil
}

func prepareConfigPart(part string, raw []byte) error {
	st, err := os.Lstat(part)
	if err == nil {
		if !privateMetadata(st) || st.Mode().Perm() != 0600 || !st.Mode().IsRegular() {
			return errors.New("unsafe unpublished bootstrap part retained")
		}
		old, e := os.ReadFile(part)
		if e != nil {
			return e
		}
		if !bytes.HasPrefix(raw, old) {
			return errors.New("bootstrap part does not match configuration input; retained")
		}
		// The exact protocol file contains only a prefix of this reviewed input.
		// Restart from zero rather than append to an interrupted write.
		if err = os.Remove(part); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(part)
		}
	}()
	n, err := f.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = fsutil.SyncDir(filepath.Dir(part)); err != nil {
		return err
	}
	ok = true
	return nil
}

func cleanupUnpublishedBootstrap(tx, part string, cause error) error {
	// Only called after successfully preparing our exact part and before any
	// placeholder move. Normal errors clean the temporary file; kill recovery is
	// handled separately using the reserved protocol and content checks.
	if err := os.Remove(part); err != nil {
		return errors.Join(cause, err)
	}
	entries, err := os.ReadDir(tx)
	if err != nil {
		return errors.Join(cause, err)
	}
	if len(entries) == 0 {
		err = os.Remove(tx)
	}
	return errors.Join(cause, err, fsutil.SyncDir(filepath.Dir(tx)))
}

func finishPublishedBootstrap(l operations.Layout, tx string, config os.FileInfo, raw []byte) error {
	if _, err := os.Lstat(filepath.Join(tx, "legacy.empty")); err == nil {
		marker, e := operations.ReadMarker(l)
		if e != nil || marker.State != operations.MarkerActive || marker.Restore != nil || marker.Migration != nil {
			return errors.New("legacy bootstrap witness does not belong to an active upgrade; retained")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	part := configPartPath(tx, raw)
	st, err := os.Lstat(part)
	if err == nil {
		if !os.SameFile(st, config) || !bootstrapPartOwner(st) || config.Mode().Perm() != 0600 {
			return errors.New("published configuration does not match bootstrap inode; all files retained")
		}
		got, e := os.ReadFile(part)
		if e != nil || !bytes.Equal(got, raw) {
			return errors.New("published bootstrap bytes changed; retained")
		}
		// Publish must be durable before unlinking the last recovery witness.
		if err = fsutil.SyncDir(l.Home); err != nil {
			return err
		}
		if err = os.Remove(part); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	placeholder := filepath.Join(tx, "legacy.empty")
	if st, e := os.Lstat(placeholder); e == nil {
		if !legacyEmptyConfig(st) {
			return errors.New("legacy placeholder changed; retained")
		}
		if err = os.Remove(placeholder); err != nil {
			return err
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if err = fsutil.SyncDir(tx); err != nil {
		return err
	}
	if err = os.Remove(tx); err != nil {
		return err
	} // nonempty/unknown state fails
	return fsutil.SyncDir(l.Home)
}

func checkLegacyConfig(l operations.Layout, raw []byte, cfg operations.Config) error {
	for _, path := range []string{l.MarkerPath(), l.DBDir(), l.DBPath(ownership.Main), l.DBPath(ownership.Ledger), l.DBPath(ownership.Runtime), l.DBPath(ownership.Events)} {
		st, err := os.Lstat(path)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !privateMetadata(st) || st.Mode().Perm()&0o022 != 0 {
			return errors.New("existing instance/database metadata is unsafe; retained")
		}
		if path == l.DBDir() {
			if !st.IsDir() {
				return errors.New("database directory is not a directory")
			}
		} else if !st.Mode().IsRegular() {
			return errors.New("instance/database path is not a regular file")
		}
	}
	rep, err := operations.InspectConfig(context.Background(), l.Home, raw)
	if err != nil {
		return err
	}
	if !rep.Compatible() {
		return fmt.Errorf("instance/database preflight: %v", rep.Problems)
	}
	// Never create, copy, rotate, chmod or select a new key during an upgrade.
	keyDir, err := os.Lstat(cfg.SecretsDir)
	if err != nil || !keyDir.IsDir() || keyDir.Mode().Perm() != 0700 || !privateMetadata(keyDir) {
		return errors.New("existing configured key directory is missing, linked or unsafe")
	}
	keyInfo, err := os.Lstat(masterkey.Path(cfg.SecretsDir))
	if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm() != 0600 || !privateMetadata(keyInfo) {
		return errors.New("existing configured master key is missing, linked or unsafe")
	}
	key, err := masterkey.Load(cfg.SecretsDir)
	if err != nil {
		return err
	}
	db, err := sqlite.Open(context.Background(), l.DBPath(ownership.Main), sqlite.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer db.Close()
	open, err := identity.BootstrapIsOpen(context.Background(), db)
	if err != nil {
		return err
	}
	if open {
		return errors.New("existing administrator bootstrap is incomplete")
	}
	return identity.CheckStoredFactors(context.Background(), db, key)
}
