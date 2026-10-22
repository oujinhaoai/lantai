package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

const RecoveryPackageContract = "lantai.recovery-package/v1"
const recoveryMagic = "LANTAI-RECOVERY-1\n"
const recoveryKeyPath = "recovery/master-key.json"
const recoveryHeaderLimit = 64 << 20
const recoverySizeLimit int64 = 1 << 40

// RecoveryPackagePath is inside the private backup job directory but is not
// part of the inner v1 snapshot manifest or any later package payload.
func RecoveryPackagePath(snapshot string) string { return filepath.Join(snapshot, "recovery.lantai") }

type RecoveryPackageFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// RecoveryPackageManifest describes a sensitive bearer artifact. Its hashes
// prove byte consistency, not trusted authorship by a separately known source.
type RecoveryPackageManifest struct {
	Contract       string                `json:"contract"`
	PackageID      ids.ID                `json:"package_id"`
	BackupID       ids.ID                `json:"backup_id"`
	InstanceID     ids.ID                `json:"instance_id"`
	RecoveryEpoch  int64                 `json:"recovery_epoch"`
	KeyID          ids.ID                `json:"key_id"`
	ManifestDigest digest.Digest         `json:"manifest_digest"`
	CapturedAt     string                `json:"captured_at"`
	Sensitive      bool                  `json:"sensitive"`
	Files          []RecoveryPackageFile `json:"files"`
}

type RecoveryPackageInfo struct {
	Manifest RecoveryPackageManifest `json:"manifest"`
	SHA256   string                  `json:"sha256"`
	Size     int64                   `json:"size"`
}

func recoveryDigest(raw []byte) string {
	x := sha256.Sum256(raw)
	return hex.EncodeToString(x[:])
}

// WriteRecoveryPackage publishes one complete private artifact. writeKey is an
// internal capability provided by the identity owner; no secret path is taken
// from HTTP input and the recovery material never enters the snapshot folder.
func WriteRecoveryPackage(ctx context.Context, snapshot, target string, keyID ids.ID, writeKey func(io.Writer) error, checks ...func(context.Context) error) (RecoveryPackageInfo, error) {
	var zero RecoveryPackageInfo
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, f := range checks {
			if f != nil {
				if err := f(ctx); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := check(); err != nil {
		return zero, err
	}
	m, err := VerifyBackup(ctx, snapshot)
	if err != nil {
		return zero, err
	}
	if !keyID.Valid() || m.KeyID != keyID || writeKey == nil {
		return zero, errors.New("recovery package: matching recovery material required")
	}
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return zero, errors.New("recovery package: canonical output required")
	}
	root, err := os.OpenRoot(snapshot)
	if err != nil {
		return zero, err
	}
	defer root.Close()
	var key bytes.Buffer
	defer func() { clear(key.Bytes()) }()
	if err = writeKey(&key); err != nil {
		return zero, err
	}
	if key.Len() == 0 || key.Len() > 4096 {
		return zero, errors.New("recovery package: invalid recovery material length")
	}
	rawManifest, err := root.ReadFile("manifest.json")
	if err != nil {
		return zero, err
	}
	p := RecoveryPackageManifest{Contract: RecoveryPackageContract, PackageID: ids.New(), BackupID: m.BackupID, InstanceID: m.InstanceID, RecoveryEpoch: m.RecoveryEpoch, KeyID: m.KeyID, ManifestDigest: digest.Of(rawManifest), CapturedAt: clock.Format(m.CreatedAt), Sensitive: true}
	names := map[string]bool{"manifest.json": true, "COMPLETE": true}
	for _, f := range append(append([]BackupFile{}, m.Files...), m.Blobs...) {
		names[f.Stored] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		f, e := hashBackupFile(ctx, root, name)
		if e != nil {
			return zero, e
		}
		p.Files = append(p.Files, RecoveryPackageFile{Path: "snapshot/" + name, Size: f.Size, SHA256: f.SHA256})
	}
	p.Files = append(p.Files, RecoveryPackageFile{Path: recoveryKeyPath, Size: int64(key.Len()), SHA256: recoveryDigest(key.Bytes())})
	header, err := json.Marshal(p)
	if err != nil {
		return zero, err
	}
	header, err = canonjson.Canonicalize(header)
	if err != nil || len(header) > recoveryHeaderLimit {
		return zero, errors.New("recovery package: manifest too large")
	}
	if _, err = os.Lstat(target); err == nil {
		old, e := VerifyRecoveryPackage(ctx, target)
		if e == nil && old.Manifest.BackupID == m.BackupID && old.Manifest.KeyID == m.KeyID && old.Manifest.ManifestDigest == p.ManifestDigest && sameRecoveryFiles(old.Manifest.Files, p.Files) {
			return old, nil
		}
		return zero, errors.New("recovery package: output already exists with a different or invalid package")
	} else if !errors.Is(err, os.ErrNotExist) {
		return zero, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil || parent != filepath.Dir(target) {
		return zero, errors.New("recovery package: output parent must not contain links")
	}
	if err = fsutil.CheckPrivate(parent); err != nil {
		return zero, err
	}
	f, err := fsutil.CreatePrivateTemp(filepath.Dir(target), ".recovery-package-")
	if err != nil {
		return zero, err
	}
	temp := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(temp) }()
	h := sha256.New()
	w := io.MultiWriter(f, h)
	if _, err = io.WriteString(w, recoveryMagic); err != nil {
		return zero, err
	}
	if err = binary.Write(w, binary.BigEndian, uint64(len(header))); err != nil {
		return zero, err
	}
	if _, err = w.Write(header); err != nil {
		return zero, err
	}
	for _, item := range p.Files {
		if err = check(); err != nil {
			return zero, err
		}
		if item.Path == recoveryKeyPath {
			_, err = w.Write(key.Bytes())
		} else {
			name := strings.TrimPrefix(item.Path, "snapshot/")
			if err = regularPath(root, name); err != nil {
				return zero, err
			}
			in, e := root.Open(name)
			if e != nil {
				return zero, e
			}
			fh := sha256.New()
			n, e := io.Copy(io.MultiWriter(w, fh), contextReader{ctx, in})
			closeErr := in.Close()
			if e != nil {
				return zero, e
			}
			if closeErr != nil {
				return zero, closeErr
			}
			if n != item.Size || hex.EncodeToString(fh.Sum(nil)) != item.SHA256 {
				return zero, errors.New("recovery package: snapshot changed during export")
			}
		}
		if err != nil {
			return zero, err
		}
	}
	if _, err = f.Write(h.Sum(nil)); err != nil {
		return zero, err
	}
	if err = f.Sync(); err != nil {
		return zero, err
	}
	if err = f.Close(); err != nil {
		return zero, err
	}
	info, err := VerifyRecoveryPackage(ctx, temp)
	if err != nil {
		return zero, err
	}
	if err = check(); err != nil {
		return zero, err
	}
	// Link is an atomic no-replace publication; an existing artifact is never
	// silently overwritten. Remove the private staging link before returning.
	if err = os.Link(temp, target); err != nil {
		return zero, err
	}
	if err = os.Remove(temp); err != nil {
		return zero, err
	}
	if err = fsutil.SyncDir(filepath.Dir(target)); err != nil {
		return zero, err
	}
	return info, nil
}

func sameRecoveryFiles(a, b []RecoveryPackageFile) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readRecoveryHeader(f *os.File) (RecoveryPackageInfo, int64, error) {
	var info RecoveryPackageInfo
	st, err := f.Stat()
	if err != nil {
		return info, 0, err
	}
	if !st.Mode().IsRegular() || st.Size() < int64(len(recoveryMagic)+8+32) || st.Size() > recoverySizeLimit {
		return info, 0, errors.New("recovery package: invalid size or file type")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return info, 0, err
	}
	magic := make([]byte, len(recoveryMagic))
	if _, err = io.ReadFull(f, magic); err != nil || string(magic) != recoveryMagic {
		return info, 0, errors.New("recovery package: unsupported format")
	}
	var size uint64
	if err = binary.Read(f, binary.BigEndian, &size); err != nil || size == 0 || size > recoveryHeaderLimit {
		return info, 0, errors.New("recovery package: invalid manifest size")
	}
	raw := make([]byte, int(size))
	if _, err = io.ReadFull(f, raw); err != nil {
		return info, 0, err
	}
	if _, err = canonjson.Canonicalize(raw); err != nil {
		return info, 0, errors.New("recovery package: malformed manifest")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&info.Manifest); err != nil || d.Decode(&struct{}{}) != io.EOF {
		return info, 0, errors.New("recovery package: malformed manifest")
	}
	m := info.Manifest
	if _, err := clock.Parse(m.CapturedAt); err != nil {
		return info, 0, errors.New("recovery package: invalid capture timestamp")
	}
	if m.Contract != RecoveryPackageContract || !m.PackageID.Valid() || !m.BackupID.Valid() || !m.InstanceID.Valid() || !m.KeyID.Valid() || !m.ManifestDigest.Valid() || !m.Sensitive || m.RecoveryEpoch < 1 || len(m.Files) < 3 || len(m.Files) > 100000 {
		return info, 0, errors.New("recovery package: invalid identity or entry count")
	}
	offset := int64(len(recoveryMagic)+8) + int64(size)
	total := offset + 32
	seen := map[string]bool{}
	keyCount := 0
	for _, item := range m.Files {
		fold := strings.ToLower(item.Path)
		if !safeRelative(item.Path) || len(item.Path) > 4096 || seen[fold] || item.Size < 0 || item.Size > recoverySizeLimit-total {
			return info, 0, errors.New("recovery package: unsafe or duplicate entry")
		}
		seen[fold] = true
		sha, e := hex.DecodeString(item.SHA256)
		if e != nil || len(sha) != sha256.Size || strings.ToLower(item.SHA256) != item.SHA256 {
			return info, 0, errors.New("recovery package: invalid entry digest")
		}
		if item.Path == recoveryKeyPath {
			keyCount++
			if item.Size == 0 || item.Size > 4096 {
				return info, 0, errors.New("recovery package: invalid recovery material size")
			}
		} else if !strings.HasPrefix(item.Path, "snapshot/") {
			return info, 0, errors.New("recovery package: unsupported entry")
		}
		total += item.Size
	}
	if keyCount != 1 || !seen["snapshot/manifest.json"] || !seen["snapshot/complete"] || total != st.Size() {
		return info, 0, errors.New("recovery package: incomplete or trailing data")
	}
	info.Size = st.Size()
	return info, offset, nil
}

func verifyRecoveryFile(ctx context.Context, f *os.File) (RecoveryPackageInfo, int64, error) {
	info, offset, err := readRecoveryHeader(f)
	if err != nil {
		return info, 0, err
	}
	before, _ := f.Stat()
	h := sha256.New()
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return info, 0, err
	}
	if _, err = io.CopyN(h, contextReader{ctx, f}, offset); err != nil {
		return info, 0, err
	}
	for _, item := range info.Manifest.Files {
		part := sha256.New()
		if _, err = io.CopyN(io.MultiWriter(h, part), contextReader{ctx, f}, item.Size); err != nil {
			return info, 0, errors.New("recovery package: truncated entry")
		}
		if hex.EncodeToString(part.Sum(nil)) != item.SHA256 {
			return info, 0, fmt.Errorf("recovery package: entry checksum mismatch: %s", item.Path)
		}
	}
	want := h.Sum(nil)
	footer := make([]byte, sha256.Size)
	if _, err = io.ReadFull(f, footer); err != nil || !bytes.Equal(want, footer) {
		return info, 0, errors.New("recovery package: checksum mismatch")
	}
	h.Write(footer)
	info.SHA256 = hex.EncodeToString(h.Sum(nil))
	if err = verifyRecoveryInventory(f, info, offset); err != nil {
		return info, 0, err
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return info, 0, errors.New("recovery package: changed during verification")
	}
	return info, offset, nil
}

// Verify both inventories without extracting or trusting the package's own
// hashes as an identity proof. SQLite and owner checks still run on restore.
func verifyRecoveryInventory(f *os.File, info RecoveryPackageInfo, offset int64) error {
	entries := map[string]RecoveryPackageFile{}
	var raw, complete []byte
	for _, item := range info.Manifest.Files {
		if item.Path == "snapshot/manifest.json" || item.Path == "snapshot/COMPLETE" {
			limit := int64(recoveryHeaderLimit)
			if item.Path == "snapshot/COMPLETE" {
				limit = 128
			}
			if item.Size > limit {
				return errors.New("recovery package: oversized snapshot metadata")
			}
			b, err := io.ReadAll(io.NewSectionReader(f, offset, item.Size))
			if err != nil {
				return err
			}
			if item.Path == "snapshot/manifest.json" {
				raw = b
			} else {
				complete = b
			}
		}
		entries[item.Path] = item
		offset += item.Size
	}
	var m BackupManifest
	if _, err := decodeBackupJSON(bytes.NewReader(raw), &m); err != nil {
		return err
	}
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return err
	}
	reg, err := schema.Default()
	if err != nil {
		return err
	}
	if err = reg.Validate(BackupContract, doc); err != nil {
		return err
	}
	p := info.Manifest
	if m.State != "complete" || m.CompletedAt.IsZero() || m.BackupID != p.BackupID || m.InstanceID != p.InstanceID || m.KeyID != p.KeyID || m.RecoveryEpoch != p.RecoveryEpoch || clock.Format(m.CreatedAt) != p.CapturedAt || digest.Of(raw) != p.ManifestDigest || strings.TrimSpace(string(complete)) != string(p.ManifestDigest) {
		return errors.New("recovery package: snapshot binding mismatch")
	}
	if m.Matrix.DataFormatVersion < 1 || m.Matrix.DataFormatVersion > DataFormatVersion || len(m.Matrix.Databases) != len(authoritative) {
		return errors.New("recovery package: unsupported snapshot format")
	}
	seen := map[string]bool{}
	for _, item := range append(append([]BackupFile{}, m.Files...), m.Blobs...) {
		name := "snapshot/" + item.Stored
		got, ok := entries[name]
		if !ok || seen[name] || item.Path != item.Stored || got.Size != item.Size || got.SHA256 != item.SHA256 {
			return errors.New("recovery package: snapshot inventory mismatch")
		}
		seen[name] = true
	}
	if len(seen)+3 != len(entries) || !seen["snapshot/instance.json"] {
		return errors.New("recovery package: incomplete snapshot inventory")
	}
	for _, name := range authoritative {
		if !seen["snapshot/db/"+string(name)+".db"] {
			return errors.New("recovery package: missing database")
		}
	}
	return nil
}

func VerifyRecoveryPackage(ctx context.Context, file string) (RecoveryPackageInfo, error) {
	f, err := os.Open(file)
	if err != nil {
		return RecoveryPackageInfo{}, err
	}
	defer f.Close()
	info, _, err := verifyRecoveryFile(ctx, f)
	return info, err
}

// VerifyOpenRecoveryPackage keeps verification and subsequent streaming on the
// same descriptor, avoiding a pathname replacement between those operations.
func VerifyOpenRecoveryPackage(ctx context.Context, f *os.File) (RecoveryPackageInfo, error) {
	info, _, err := verifyRecoveryFile(ctx, f)
	return info, err
}

// ExtractRecoveryPackage refuses existing destinations and never writes the
// old key to disk. It validates the complete container before extracting and
// the original v1 database/file contract afterwards. consumeKey is internal.
func ExtractRecoveryPackage(ctx context.Context, file, snapshot string, consumeKey func(io.Reader) error) (RecoveryPackageInfo, error) {
	var zero RecoveryPackageInfo
	f, err := os.Open(file)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	info, offset, err := verifyRecoveryFile(ctx, f)
	if err != nil {
		return zero, err
	}
	if consumeKey == nil {
		return zero, errors.New("recovery package: recovery material consumer required")
	}
	if _, err = independentTarget(file, snapshot); err != nil {
		return zero, err
	}
	if err = os.Mkdir(snapshot, 0700); err != nil {
		return zero, err
	}
	root, err := os.OpenRoot(snapshot)
	if err != nil {
		return zero, err
	}
	defer root.Close()
	for _, item := range info.Manifest.Files {
		r := io.NewSectionReader(f, offset, item.Size)
		offset += item.Size
		if item.Path == recoveryKeyPath {
			raw, e := io.ReadAll(r)
			if e != nil {
				return zero, e
			}
			if recoveryDigest(raw) != item.SHA256 {
				clear(raw)
				return zero, errors.New("recovery package: recovery material changed")
			}
			e = consumeKey(bytes.NewReader(raw))
			clear(raw)
			if e != nil {
				return zero, e
			}
			continue
		}
		name := strings.TrimPrefix(item.Path, "snapshot/")
		if err = makeBackupParents(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
			return zero, err
		}
		out, e := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return zero, e
		}
		h := sha256.New()
		_, e = io.Copy(io.MultiWriter(out, h), contextReader{ctx, r})
		if e == nil {
			e = out.Sync()
		}
		closeErr := out.Close()
		if e != nil {
			return zero, e
		}
		if closeErr != nil {
			return zero, closeErr
		}
		if hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
			return zero, errors.New("recovery package: entry changed during extraction")
		}
	}
	m, err := VerifyBackup(ctx, snapshot)
	if err != nil {
		return zero, err
	}
	raw, err := root.ReadFile("manifest.json")
	if err != nil {
		return zero, err
	}
	if m.BackupID != info.Manifest.BackupID || m.InstanceID != info.Manifest.InstanceID || m.KeyID != info.Manifest.KeyID || m.RecoveryEpoch != info.Manifest.RecoveryEpoch || clock.Format(m.CreatedAt) != info.Manifest.CapturedAt || digest.Of(raw) != info.Manifest.ManifestDigest {
		return zero, errors.New("recovery package: snapshot identity mismatch")
	}
	return info, fsutil.SyncDir(snapshot)
}
