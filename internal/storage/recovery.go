package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
)

type maintenanceReader struct {
	ctx context.Context
	r   io.Reader
}

func (r maintenanceReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func hashMaintenanceFile(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := digest.NewHasher()
	_, err = io.Copy(h, maintenanceReader{ctx: ctx, r: f})
	return h.Hex(), h.Size(), err
}

// Finding contains only instance-relative locations, never host paths or payloads.
type Finding struct {
	Code        errcode.Code `json:"code"`
	Reason      string       `json:"reason"`
	Ref         string       `json:"ref,omitempty"`
	OperationID ids.ID       `json:"operation_id,omitempty"`
}
type Blob struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Ref    string `json:"ref"`
}
type StagingEntry struct {
	Ref     string `json:"ref"`
	Kind    string `json:"kind"`
	OwnerID ids.ID `json:"owner_id,omitempty"`
}
type RecordEntry struct {
	RecordID    ids.ID        `json:"record_id"`
	ProjectID   ids.ID        `json:"project_id"`
	AssetID     ids.ID        `json:"asset_id"`
	VersionID   ids.ID        `json:"version_id"`
	OperationID ids.ID        `json:"operation_id,omitempty"`
	Digest      digest.Digest `json:"digest"`
	Ref         string        `json:"ref"`
}
type InstallEntry struct {
	InstallState
	Proof *install.Proof `json:"proof,omitempty"`
}
type UploadEntry struct {
	UploadID    ids.ID      `json:"upload_id"`
	OperationID ids.ID      `json:"operation_id"`
	State       UploadState `json:"state"`
}
type Inventory struct {
	Blobs    []Blob         `json:"blobs"`
	Installs []InstallEntry `json:"installs"`
	Uploads  []UploadEntry  `json:"uploads"`
	Staging  []StagingEntry `json:"staging"`
	Records  []RecordEntry  `json:"records"`
	Orphans  []Orphan       `json:"orphans"`
	Findings []Finding      `json:"findings"`
}

// Inventory inspects all CAS objects, including unreferenced objects, and owned
// durable install/upload facts. It never adopts files, commits a version or
// removes staging. A consistent backup inventory requires the maintenance
// barrier. Findings must be resolved before declaring a backup verified.
func (s *Service) Inventory(ctx context.Context, deep bool) (Inventory, error) {
	var out Inventory
	add := func(reason, path string, op ids.ID, code errcode.Code) {
		out.Findings = append(out.Findings, Finding{Code: code, Reason: reason, Ref: s.layout.ref(path), OperationID: op})
	}
	walk := func(root string, fn func(string, fs.DirEntry) error) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			if err != nil {
				if path == root && errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				add("unreadable", path, "", errcode.StorageUnavailable)
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
				add("non_regular", path, "", errcode.OperationNeedsReconciliation)
				return nil
			}
			return fn(path, d)
		})
	}
	if err := walk(filepath.Join(s.layout.Home, "blobs"), func(path string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		sha := d.Name()
		if !digest.ValidHex(sha) || path != s.layout.BlobPath(sha) {
			add("unknown_blob", path, "", errcode.OperationNeedsReconciliation)
			return nil
		}
		st, err := d.Info()
		if err != nil {
			add("unreadable", path, "", errcode.StorageUnavailable)
			return nil
		}
		out.Blobs = append(out.Blobs, Blob{SHA256: sha, Size: st.Size(), Ref: s.layout.ref(path)})
		if deep {
			if err := s.VerifyBlob(ctx, sha); err != nil {
				code := errcode.CodeOf(err)
				if code == "" {
					code = errcode.StorageUnavailable
				}
				add("blob_verification", path, "", code)
			}
		}
		return nil
	}); err != nil {
		return out, err
	}
	blobs := map[string]int64{}
	for _, b := range out.Blobs {
		blobs[b.SHA256] = b.Size
	}
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id FROM storage_installs ORDER BY operation_id`)
	if err != nil {
		return out, err
	}
	var ops []ids.ID
	for rows.Next() {
		var id ids.ID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ops = append(ops, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, id := range ops {
		r, err := s.installRow(ctx, s.db, id)
		if err != nil {
			return out, err
		}
		e := InstallEntry{InstallState: InstallState{OperationID: id, State: r.State, VersionID: r.VersionID, InstallRef: r.InstallRef, QuarantineReason: r.QuarantineReason}}
		if r.State == "installed" {
			proof, err := s.proofOf(ctx, r)
			if err != nil {
				return out, err
			}
			e.Proof = &proof
			if _, err = proof.Canonical(); err != nil {
				add("install_record_invalid", s.layout.Home, id, errcode.OperationNeedsReconciliation)
				out.Installs = append(out.Installs, e)
				continue
			}
			for _, f := range proof.Files {
				if n, ok := blobs[f.SHA256]; !ok || n != f.Size {
					add("referenced_blob_missing_or_size", s.layout.BlobPath(f.SHA256), id, errcode.OperationNeedsReconciliation)
				}
			}
			if deep {
				if err = s.VerifyDeep(ctx, id); err != nil {
					code := errcode.CodeOf(err)
					if code == "" {
						code = errcode.StorageUnavailable
					}
					add("install_verification", filepath.Join(s.layout.Home, filepath.FromSlash(r.InstallRef)), id, code)
				}
			}
		}
		out.Installs = append(out.Installs, e)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT upload_id,operation_id,state FROM storage_uploads ORDER BY upload_id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var u UploadEntry
		if err = rows.Scan(&u.UploadID, &u.OperationID, &u.State); err != nil {
			rows.Close()
			return out, err
		}
		out.Uploads = append(out.Uploads, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	parts, err := s.stagingFindings(ctx, deep, blobs)
	if err != nil {
		return out, err
	}
	out.Findings = append(out.Findings, parts...)
	for _, kind := range []string{"uploads", "install"} {
		root := filepath.Join(s.layout.Home, "staging", kind)
		if err = walk(root, func(path string, d fs.DirEntry) error {
			if filepath.Dir(path) == root {
				id := ids.ID(d.Name())
				if !id.Valid() {
					id = ""
				}
				out.Staging = append(out.Staging, StagingEntry{Ref: s.layout.ref(path), Kind: kind, OwnerID: id})
			}
			return nil
		}); err != nil {
			return out, err
		}
	}
	// Record enumeration has its own identity and schema checks; file existence
	// alone never makes the record accepted provenance.
	if err = walk(filepath.Join(s.layout.Home, "projects"), func(path string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(s.layout.Home, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 7 || parts[2] != "assets" || parts[4] != "records" {
			return nil
		}
		b, err := readBoundedRegular(s.layout.Home, path, MaxRecordBytes)
		if err != nil {
			add("record_unreadable", path, "", errcode.OperationNeedsReconciliation)
			return nil
		}
		var r Record
		if err = json.Unmarshal(b, &r); err != nil {
			add("record_invalid", path, "", errcode.SchemaInvalid)
			return nil
		}
		canonical, err := r.canonical()
		if err != nil || !bytes.Equal(b, canonical) || string(r.ProjectID) != parts[1] || string(r.AssetID) != parts[3] || string(r.VersionID) != parts[5] || string(r.RecordID)+".json" != parts[6] {
			add("record_invalid", path, r.OperationID, errcode.SchemaInvalid)
			return nil
		}
		out.Records = append(out.Records, RecordEntry{RecordID: r.RecordID, ProjectID: r.ProjectID, AssetID: r.AssetID, VersionID: r.VersionID, OperationID: r.OperationID, Digest: digest.Of(b), Ref: s.layout.ref(path)})
		return nil
	}); err != nil {
		return out, err
	}
	for _, f := range out.Findings {
		if f.Reason == "non_regular" || f.Reason == "unreadable" {
			return out, nil
		}
	}
	out.Orphans, err = s.ScanOrphans(ctx)
	return out, err
}

// stagingFindings compares only durable received/verified facts. Private bytes
// not yet acknowledged remain residual files; they are never adopted as parts.
func (s *Service) stagingFindings(ctx context.Context, deep bool, blobs map[string]int64) ([]Finding, error) {
	var out []Finding
	rows, err := s.db.QueryContext(ctx, `SELECT u.operation_id,f.sha256,f.size FROM storage_upload_files f JOIN storage_uploads u ON u.upload_id=f.upload_id WHERE f.state='verified' AND u.state='open' ORDER BY u.upload_id,f.sha256`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var op ids.ID
		var sha string
		var size int64
		if err = rows.Scan(&op, &sha, &size); err != nil {
			rows.Close()
			return nil, err
		}
		if n, ok := blobs[sha]; !ok || n != size {
			out = append(out, Finding{Code: errcode.OperationNeedsReconciliation, Reason: "upload_blob_missing_or_size", OperationID: op})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	type part struct {
		upload, op          ids.ID
		sha, partSHA        string
		offset, size, total int64
	}
	rows, err = s.db.QueryContext(ctx, `SELECT p.upload_id,u.operation_id,p.sha256,p.part_sha256,p.start_offset,p.size,f.size FROM storage_upload_parts p JOIN storage_upload_files f ON f.upload_id=p.upload_id AND f.sha256=p.sha256 JOIN storage_uploads u ON u.upload_id=p.upload_id WHERE f.state='pending' AND u.state='open' ORDER BY p.upload_id,p.sha256,p.part_number`)
	if err != nil {
		return nil, err
	}
	var all []part
	for rows.Next() {
		var p part
		if err = rows.Scan(&p.upload, &p.op, &p.sha, &p.partSHA, &p.offset, &p.size, &p.total); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		finding := Finding{Code: errcode.OperationNeedsReconciliation, Reason: "upload_part_record_invalid", OperationID: p.op}
		if !p.upload.Valid() || !digest.ValidHex(p.sha) || !digest.ValidHex(p.partSHA) || p.offset < 0 || p.size < 0 || p.total < 0 || p.offset > p.total || p.size > p.total-p.offset {
			out = append(out, finding)
			continue
		}
		path := s.layout.uploadData(p.upload, p.sha)
		finding.Ref = s.layout.ref(path)
		if err = regularPath(s.layout.Home, path); err != nil {
			finding.Reason = "upload_part_missing_or_unsafe"
			out = append(out, finding)
			continue
		}
		f, e := os.Open(path)
		if e != nil {
			finding.Reason = "upload_part_unreadable"
			out = append(out, finding)
			continue
		}
		st, e := f.Stat()
		if e != nil || st.Size() < p.offset+p.size {
			f.Close()
			finding.Reason = "upload_part_truncated"
			out = append(out, finding)
			continue
		}
		if deep {
			h := digest.NewHasher()
			_, e = io.Copy(h, maintenanceReader{ctx: ctx, r: io.NewSectionReader(f, p.offset, p.size)})
			if e != nil || h.Hex() != p.partSHA || h.Size() != p.size {
				finding.Reason = "upload_part_digest"
				out = append(out, finding)
			}
		}
		f.Close()
	}
	return out, nil
}

func readBoundedRegular(root, path string, limit int64) ([]byte, error) {
	if err := regularPath(root, path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errcode.New(errcode.QuotaExceeded, "maintenance file exceeds size limit")
	}
	return b, err
}

// regularPath rejects symlinks in every component, including an ancestor. The
// data root is service-owned; maintenance excludes concurrent service mutation.
func regularPath(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errcode.New(errcode.OperationNeedsReconciliation, "maintenance path escapes data root")
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || (p == path && !st.Mode().IsRegular()) {
			return errcode.New(errcode.OperationNeedsReconciliation, "maintenance path is not a regular file")
		}
		if p == root {
			break
		}
	}
	return nil
}
