package extensions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/plugins"
	"github.com/oujinhaoai/lantai/plugins/corecheck"
)

type Deps struct {
	DB            *sql.DB
	Gate          *commands.Gate
	ReleaseDigest digest.Digest
}
type Registry struct {
	db      *sql.DB
	gate    *commands.Gate
	release digest.Digest
	mu      sync.Mutex
}
type Registration struct {
	ID                string        `json:"id"`
	Version           string        `json:"version"`
	PackageDigest     digest.Digest `json:"package_digest"`
	ManifestDigest    digest.Digest `json:"manifest_digest"`
	Source            string        `json:"source"`
	Manifest          Manifest      `json:"manifest"`
	CoreReleaseDigest digest.Digest `json:"core_release_digest,omitempty"`
}

func New(d Deps) (*Registry, error) {
	if d.DB == nil || d.Gate == nil || !d.ReleaseDigest.Valid() {
		return nil, errors.New("extensions: main database, instance gate and current executable digest are required")
	}
	return &Registry{db: d.DB, gate: d.Gate, release: d.ReleaseDigest}, nil
}

// CurrentReleaseDigest hashes the current deployed executable, not a user value
// or a mutable git revision string. The deployment must keep this artifact immutable.
func CurrentReleaseDigest() (digest.Digest, error) {
	p, e := os.Executable()
	if e != nil {
		return "", e
	}
	f, e := os.Open(p)
	if e != nil {
		return "", e
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil || !before.Mode().IsRegular() {
		return "", errors.New("extensions: executable is not a regular release artifact")
	}
	h := digest.NewHasher()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	after, e := f.Stat()
	if e != nil {
		return "", e
	}
	if before.Size() != after.Size() || h.Size() != before.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("extensions: executable changed during release verification")
	}
	return h.Digest(), nil
}
func builtinPackages() ([]Package, error) {
	// This list is code-reviewed and compiled; no directory/PATH discovery and no
	// public API that accepts an external manifest as builtin_release.
	f, err := fs.Sub(plugins.Files, "corecheck")
	if err != nil {
		return nil, err
	}
	p, err := ReadPackage(f)
	if err != nil {
		return nil, err
	}
	return []Package{p}, nil
}
func (r *Registry) RegisterBuiltins(ctx context.Context) error {
	p, e := builtinPackages()
	if e != nil {
		return e
	}
	return r.register(ctx, p)
}
func (r *Registry) register(ctx context.Context, packages []Package) error {
	if err := r.gate.RequireMaintenance(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	contributions := map[string]bool{}
	for _, p := range packages {
		if err := CheckM1(p.Manifest, nil); err != nil {
			return err
		}
		if !p.Digest.Valid() || !p.ManifestDigest.Valid() {
			return failure(errcode.SchemaInvalid, "package_identity_invalid")
		}
		if seen[p.Manifest.ID] {
			return failure(errcode.IdempotencyConflict, "duplicate_package_id")
		}
		seen[p.Manifest.ID] = true
		for _, c := range p.Manifest.Contributes {
			if contributions[c.ID] {
				return failure(errcode.IdempotencyConflict, "duplicate_contribution_id")
			}
			contributions[c.ID] = true
		}
	}
	tx, e := r.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, p := range packages {
		m := p.Manifest
		var oldDigest, oldManifest string
		e = tx.QueryRowContext(ctx, `SELECT package_digest,manifest_digest FROM extensions_registrations WHERE extension_id=? AND extension_version=?`, m.ID, m.Version).Scan(&oldDigest, &oldManifest)
		if e == nil {
			if oldDigest != p.Digest.String() || oldManifest != p.ManifestDigest.String() {
				return failure(errcode.IdempotencyConflict, "package_digest_replacement")
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		} else {
			if _, e = tx.ExecContext(ctx, `INSERT INTO extensions_registrations(extension_id,extension_version,package_digest,manifest_digest,manifest_json,source) VALUES(?,?,?,?,?,'builtin_release')`, m.ID, m.Version, p.Digest, p.ManifestDigest, []byte(p.ManifestJSON)); e != nil {
				return e
			}
			for _, c := range m.Contributes {
				var owner string
				e = tx.QueryRowContext(ctx, `SELECT extension_id FROM extensions_contributions WHERE contribution_id=? LIMIT 1`, c.ID).Scan(&owner)
				if e == nil && owner != m.ID {
					return failure(errcode.IdempotencyConflict, "contribution_owner_conflict")
				}
				if e != nil && !errors.Is(e, sql.ErrNoRows) {
					return e
				}
				if _, e = tx.ExecContext(ctx, `INSERT INTO extensions_contributions(contribution_id,extension_id,extension_version,point_id,target) VALUES(?,?,?,?,?)`, c.ID, m.ID, m.Version, c.Point, c.Target); e != nil {
					return e
				}
			}
		}
		var binding string
		e = tx.QueryRowContext(ctx, `SELECT package_digest FROM extensions_release_bindings WHERE extension_id=? AND extension_version=? AND core_release_digest=?`, m.ID, m.Version, r.release).Scan(&binding)
		if e == nil {
			if binding != p.Digest.String() {
				return failure(errcode.IdempotencyConflict, "release_binding_conflict")
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return e
		} else {
			if _, e = tx.ExecContext(ctx, `INSERT INTO extensions_release_bindings(extension_id,extension_version,core_release_digest,package_digest) VALUES(?,?,?,?)`, m.ID, m.Version, r.release, p.Digest); e != nil {
				return e
			}
		}
	}
	if e = r.gate.RequireMaintenance(ctx); e != nil {
		return e
	}
	return tx.Commit()
}
func (r *Registry) Lookup(ctx context.Context, id, version string) (Registration, error) {
	var out Registration
	var raw []byte
	e := r.db.QueryRowContext(ctx, `SELECT extension_id,extension_version,package_digest,manifest_digest,manifest_json,source FROM extensions_registrations WHERE extension_id=? AND extension_version=?`, id, version).Scan(&out.ID, &out.Version, &out.PackageDigest, &out.ManifestDigest, &raw, &out.Source)
	if errors.Is(e, sql.ErrNoRows) {
		return out, errcode.New(errcode.NotFound, "")
	}
	if e != nil {
		return out, e
	}
	if digest.Of(raw) != out.ManifestDigest {
		return out, failure(errcode.HashMismatch, "registered_manifest_corrupt")
	}
	if e = json.Unmarshal(raw, &out.Manifest); e != nil {
		return out, e
	}
	if e = CheckM1(out.Manifest, nil); e != nil {
		return out, e
	}
	if out.Manifest.ID != out.ID || out.Manifest.Version != out.Version || !out.PackageDigest.Valid() || out.Source != "builtin_release" {
		return out, failure(errcode.HashMismatch, "registered_identity_corrupt")
	}
	return out, nil
}

// List returns only the compiled packages bound to this executable release.
func (r *Registry) List(ctx context.Context) ([]Registration, error) {
	builtins, e := builtinPackages()
	if e != nil {
		return nil, e
	}
	rows, e := r.db.QueryContext(ctx, `SELECT extension_id,extension_version,package_digest FROM extensions_release_bindings WHERE core_release_digest=? ORDER BY extension_id,extension_version`, r.release)
	if e != nil {
		return nil, e
	}
	type key struct{ id, version, digest string }
	keys := []key{}
	for rows.Next() {
		var k key
		if e = rows.Scan(&k.id, &k.version, &k.digest); e != nil {
			rows.Close()
			return nil, e
		}
		keys = append(keys, k)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []Registration{}
	for _, k := range keys {
		trusted := false
		for _, p := range builtins {
			trusted = trusted || (p.Manifest.ID == k.id && p.Manifest.Version == k.version && p.Digest.String() == k.digest)
		}
		if !trusted {
			return nil, failure(errcode.ExtensionActivationStale, "registered_release_not_compiled")
		}
		v, e := r.Lookup(ctx, k.id, k.version)
		if e != nil {
			return nil, e
		}
		if v.PackageDigest.String() != k.digest {
			return nil, failure(errcode.HashMismatch, "registered_binding_corrupt")
		}
		v.CoreReleaseDigest = r.release
		out = append(out, v)
	}
	return out, nil
}

// VerifyProducer accepts identity only, not runtime authorization or human review.
// Historical evidence retains its original bytes; new acceptance uses this release.
func (r *Registry) VerifyProducer(ctx context.Context, p storage.Producer) error {
	if err := validate("lantai.common-defs/v1#/$defs/producer_ref", p); err != nil {
		return err
	}
	if p.Source != "builtin_release" || p.CoreReleaseDigest != r.release {
		return failure(errcode.ExtensionActivationStale, "producer_release_untrusted")
	}
	// The compiled allowlist is checked as well as persisted rows. Database
	// contents or a caller's source string cannot turn an external package builtin.
	builtins, e := builtinPackages()
	if e != nil {
		return e
	}
	var trusted *Package
	for i := range builtins {
		v := &builtins[i]
		if v.Manifest.ID == p.ExtensionID && v.Manifest.Version == p.ExtensionVersion && v.Digest == p.PackageDigest {
			trusted = v
			break
		}
	}
	if trusted == nil {
		return failure(errcode.ExtensionActivationStale, "producer_not_compiled")
	}
	v, e := r.Lookup(ctx, p.ExtensionID, p.ExtensionVersion)
	if e != nil {
		return e
	}
	if v.PackageDigest != p.PackageDigest {
		return failure(errcode.ExtensionActivationStale, "producer_package_mismatch")
	}
	var exists int
	e = r.db.QueryRowContext(ctx, `SELECT 1 FROM extensions_release_bindings WHERE extension_id=? AND extension_version=? AND package_digest=? AND core_release_digest=?`, p.ExtensionID, p.ExtensionVersion, p.PackageDigest, r.release).Scan(&exists)
	if errors.Is(e, sql.ErrNoRows) {
		return failure(errcode.ExtensionActivationStale, "producer_release_unregistered")
	}
	if e != nil {
		return e
	}
	if p.ContributionID == "" {
		return failure(errcode.SchemaInvalid, "producer_contribution_required")
	}
	for _, c := range trusted.Manifest.Contributes {
		if c.ID == p.ContributionID {
			return nil
		}
	}
	return failure(errcode.ExtensionPointUnsupported, "producer_contribution_unknown")
}
func (r *Registry) Producer(ctx context.Context, contribution string) (storage.Producer, error) {
	packages, e := builtinPackages()
	if e != nil {
		return storage.Producer{}, e
	}
	for _, p := range packages {
		for _, c := range p.Manifest.Contributes {
			if c.ID == contribution {
				v := storage.Producer{ExtensionID: p.Manifest.ID, ExtensionVersion: p.Manifest.Version, PackageDigest: p.Digest, CoreReleaseDigest: r.release, Source: "builtin_release", ContributionID: c.ID}
				return v, r.VerifyProducer(ctx, v)
			}
		}
	}
	return storage.Producer{}, errcode.New(errcode.NotFound, "")
}

type CheckResult struct {
	Contract       string                 `json:"contract"`
	Ref            ids.PermanentRef       `json:"ref"`
	ManifestDigest digest.Digest          `json:"manifest_digest"`
	Verdict        execution.CheckVerdict `json:"verdict"`
	Findings       []string               `json:"findings"`
	Producer       storage.Producer       `json:"producer"`
}

// ValidateManifest is the only M1 compiled validator dispatch. Its caller must
// supply authorized fixed bytes; the result is evidence, never review approval.
func (r *Registry) ValidateManifest(ctx context.Context, raw []byte, ref ids.PermanentRef, expected digest.Digest) (CheckResult, error) {
	p, e := r.Producer(ctx, "org.lantai.corecheck.manifest")
	if e != nil {
		return CheckResult{}, e
	}
	if e = ctx.Err(); e != nil {
		return CheckResult{}, e
	}
	if len(raw) > 8<<20 {
		return CheckResult{}, failure(errcode.QuotaExceeded, "builtin_input_too_large")
	}
	result := CheckResult{Contract: "lantai.check-result/v1", Ref: ref, ManifestDigest: expected, Verdict: execution.VerdictFail, Findings: []string{"manifest_identity_or_content_invalid"}, Producer: p}
	if corecheck.Check(raw, ref, expected) {
		result.Verdict = execution.VerdictPass
		result.Findings = []string{}
	}
	return result, validate("lantai.check-result/v1", result)
}
