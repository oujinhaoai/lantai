package extensions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing/fstest"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// Module is the T09 owner of extension registration, enablement and activation.
const Module = "extensions"

// TrustUnenforced is the explicit deployment decision that a package may run
// although the host cannot enforce some declared constraint. It never claims a
// sandbox; without it such packages are rejected.
const TrustUnenforced = "trusted_unenforced"

// Authority is the identity port used for administrator reads and commands.
type Authority interface {
	authz.Authorizer
	authz.EpochSource
}

// PackageVersion is a committed plugin asset version as seen by its owners.
type PackageVersion struct {
	Ref              ids.PermanentRef
	ProjectID        ids.ID
	ManifestDigest   digest.Digest
	AssetType        string
	ExtensionID      string
	ExtensionVersion string
}

// PackageReview is the current ledger review fact of a package version. T09
// never records its own approval; it only references this fact.
type PackageReview struct {
	Approved bool   `json:"approved"`
	ReviewID ids.ID `json:"review_id,omitempty"`
	State    string `json:"state"`
}

// PackageSource adapts catalog/storage/ledger. Version authorizes the caller;
// Files and Review are trusted server reads of an already registered version.
type PackageSource interface {
	PackageVersion(context.Context, authz.Context, ids.PermanentRef) (PackageVersion, error)
	PackageFiles(context.Context, ids.PermanentRef) (map[string][]byte, error)
	PackageReview(context.Context, ids.PermanentRef) (PackageReview, error)
}

// Policies resolves effective identity policy without caller authorization.
type Policies interface {
	ResolvePolicies(context.Context, ids.ID) (map[string]identity.PolicyValue, error)
}

// HumanCommands is identity's final HumanGrant acceptance.
type HumanCommands interface {
	AcceptHumanItem(context.Context, authz.Context, ids.ID, ids.ID, identity.HumanAction, commands.Request, identity.HumanDomain) (commands.Receipt, error)
}

type ManagerDeps struct {
	Registry   *Registry
	Main       *sql.DB
	Runtime    *sql.DB
	Gate       *commands.Gate
	Authority  Authority
	Packages   PackageSource
	Policies   Policies
	Clock      clock.Clock
	IDs        *ids.Generator
	InstanceID ids.ID
	// BaseDir holds private one-shot run directories; empty uses the OS temp dir.
	BaseDir string
	// Capabilities overrides measured host capabilities in tests only.
	Capabilities *IsolationCapabilities
}

// Manager governs independently imported packages: static import, review
// references, enablement, restricted probes, admission, breaker and drain. It
// also serves the compiled builtin checker through the same jobs host port.
type Manager struct {
	d       ManagerDeps
	store   *commands.Store
	caps    IsolationCapabilities
	mu      sync.Mutex
	bmu     sync.Mutex
	probing map[ids.ID]bool
	running map[ids.ID]map[ids.ID]context.CancelFunc
}

func NewManager(d ManagerDeps) (*Manager, error) {
	if d.Registry == nil || d.Main == nil || d.Runtime == nil || d.Gate == nil || d.Authority == nil || d.Packages == nil || d.Policies == nil || d.IDs == nil || !d.InstanceID.Valid() {
		return nil, errors.New("extensions: manager requires registry, main/runtime databases, gate, authority, package source, policies, IDs and instance")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	st, err := commands.NewStore(Module, d.Clock)
	if err != nil {
		return nil, err
	}
	caps := HostCapabilities()
	if d.Capabilities != nil {
		caps = *d.Capabilities
	}
	return &Manager{d: d, store: st, caps: caps, probing: map[ids.ID]bool{}, running: map[ids.ID]map[ids.ID]context.CancelFunc{}}, nil
}

// Capabilities reports what the one-shot host can enforce on this platform.
func (m *Manager) Capabilities() IsolationCapabilities { return m.caps }

// PackageRecord registers verified bytes of one committed plugin asset version.
type PackageRecord struct {
	ExtensionID           string           `json:"extension_id"`
	ExtensionVersion      string           `json:"extension_version"`
	PackageDigest         digest.Digest    `json:"package_digest"`
	ManifestDigest        digest.Digest    `json:"manifest_digest"`
	Source                string           `json:"source"`
	Ref                   ids.PermanentRef `json:"ref"`
	ProjectID             ids.ID           `json:"project_id"`
	VersionManifestDigest digest.Digest    `json:"version_manifest_digest"`
	ImportedBy            ids.ID           `json:"imported_by"`
	ImportedAt            string           `json:"imported_at"`
	OperationID           ids.ID           `json:"operation_id"`
	Manifest              Manifest         `json:"manifest"`
}

// PackageView adds the current ledger review fact; it is not stored here.
type PackageView struct {
	PackageRecord
	Review PackageReview `json:"review"`
}

type ImportRequest struct {
	AssetID   ids.ID `json:"asset_id"`
	VersionID ids.ID `json:"version_id"`
}

func (m *Manager) authorize(ctx context.Context, who authz.Context, act authz.Action) error {
	d, err := m.d.Authority.Authorize(ctx, who, act, authz.Resource{Kind: "extension"})
	if err != nil {
		return err
	}
	return d.Err()
}

func (m *Manager) command(ctx context.Context, who authz.Context, key, typ string, body any) (commands.Context, error) {
	raw, err := canonjson.CanonicalizeValue(body)
	if err != nil {
		return commands.Context{}, err
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: typ, Body: raw})
	if err != nil {
		return commands.Context{}, err
	}
	epoch, err := m.d.Authority.RecoveryEpoch(ctx)
	if err != nil {
		return commands.Context{}, err
	}
	op, err := m.d.IDs.New()
	if err != nil {
		return commands.Context{}, err
	}
	c := commands.Context{OperationID: op, IdempotencyKey: key, CommandType: typ, ActorID: who.PrincipalID, SessionID: who.SessionID, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: who.PolicyRevision}
	return c, c.Validate()
}

func (m *Manager) event(c commands.Context, typ string, id ids.ID, revision int64, payload any) (event.Envelope, error) {
	return event.New(m.d.IDs, m.d.Clock, event.Params{EventType: typ, SchemaVersion: 1, AggregateType: "extension", AggregateID: id, AggregateRevision: revision, ActorID: c.ActorID, SessionID: c.SessionID, OperationID: c.OperationID, CorrelationID: c.Correlation(), Payload: payload})
}

// readPackage fetches the registered version bytes and re-verifies the exact
// package digest. Every dispatch and probe starts from this check.
func (m *Manager) readPackage(ctx context.Context, rec PackageRecord) (Package, fstest.MapFS, error) {
	files, err := m.d.Packages.PackageFiles(ctx, rec.Ref)
	if err != nil {
		return Package{}, nil, err
	}
	fsys := fstest.MapFS{}
	for p, b := range files {
		fsys[p] = &fstest.MapFile{Data: b, Mode: 0o400}
	}
	p, err := ReadPackage(fsys)
	if err != nil {
		return Package{}, nil, err
	}
	if p.Digest != rec.PackageDigest || p.ManifestDigest != rec.ManifestDigest {
		return Package{}, nil, failure(errcode.HashMismatch, "package_bytes_changed")
	}
	return p, fsys, nil
}

// Import statically registers one committed plugin asset version. It verifies
// the manifest, file digests and M2 capability limits, and never executes an
// entry, installer or probe. Approval and enablement are separate facts.
func (m *Manager) Import(ctx context.Context, who authz.Context, key string, in ImportRequest) (PackageRecord, error) {
	var out PackageRecord
	if !in.AssetID.Valid() || !in.VersionID.Valid() {
		return out, failure(errcode.SchemaInvalid, "package_version_required")
	}
	if err := m.authorize(ctx, who, identity.ActImportExtension); err != nil {
		return out, err
	}
	ref := ids.PermanentRef{InstanceID: m.d.InstanceID, AssetID: in.AssetID, VersionID: in.VersionID}
	c, err := m.command(ctx, who, key, "extensions.import", in)
	if err != nil {
		return out, err
	}
	if prior, err := m.store.LookupReceipt(ctx, m.d.Main, c.Key()); err != nil {
		return out, err
	} else if prior != nil {
		if o := commands.Decide(prior, c.RequestHash, m.d.Clock.Now()); o != commands.OutcomeReplay {
			return out, o.Err(prior)
		}
		err = json.Unmarshal(prior.ResponseSummary, &out)
		return out, err
	}
	v, err := m.d.Packages.PackageVersion(ctx, who, ref)
	if err != nil {
		return out, err
	}
	if v.AssetType != "plugin" || v.Ref != ref {
		return out, failure(errcode.SchemaInvalid, "plugin_asset_version_required")
	}
	rec := PackageRecord{Ref: ref, ProjectID: v.ProjectID, VersionManifestDigest: v.ManifestDigest, Source: "package"}
	files, err := m.d.Packages.PackageFiles(ctx, ref)
	if err != nil {
		return out, err
	}
	fsys := fstest.MapFS{}
	for p, b := range files {
		fsys[p] = &fstest.MapFile{Data: b, Mode: 0o400}
	}
	p, err := ReadPackage(fsys)
	if err != nil {
		return out, err
	}
	if err = CheckExternal(p.Manifest); err != nil {
		return out, err
	}
	if p.Manifest.ID != v.ExtensionID || p.Manifest.Version != v.ExtensionVersion {
		return out, failure(errcode.RefMismatch, "plugin_metadata_mismatch")
	}
	builtins, err := builtinPackages()
	if err != nil {
		return out, err
	}
	for _, b := range builtins {
		if b.Manifest.ID == p.Manifest.ID {
			return out, failure(errcode.ExtensionPointUnsupported, "builtin_id_reserved")
		}
	}
	rec.ExtensionID, rec.ExtensionVersion, rec.PackageDigest, rec.ManifestDigest, rec.Manifest = p.Manifest.ID, p.Manifest.Version, p.Digest, p.ManifestDigest, p.Manifest
	rec.ImportedBy, rec.ImportedAt, rec.OperationID = who.PrincipalID, clock.Format(m.d.Clock.Now()), c.OperationID
	// Owners were read without T09 holding locks (catalog takes its own); the
	// final authorization and the registration commit share the security guard.
	ctx, h, err := m.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return out, err
	}
	defer h.Release()
	if err = m.authorize(ctx, who, identity.ActImportExtension); err != nil {
		return out, err
	}
	res, err := m.store.Execute(ctx, m.d.Main, c, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT record FROM extensions_packages WHERE extension_id=? AND extension_version=?`, rec.ExtensionID, rec.ExtensionVersion).Scan(&raw)
		if err == nil {
			var old PackageRecord
			if err = json.Unmarshal([]byte(raw), &old); err != nil {
				return commands.Result{}, err
			}
			if old.PackageDigest != rec.PackageDigest || old.ManifestDigest != rec.ManifestDigest {
				return commands.Result{}, failure(errcode.IdempotencyConflict, "package_digest_replacement")
			}
			// The same immutable bytes keep their first review reference.
			return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: old}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return commands.Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO extensions_packages(extension_id,extension_version,package_digest,manifest_digest,record) VALUES(?,?,?,?,?)`, rec.ExtensionID, rec.ExtensionVersion, rec.PackageDigest, rec.ManifestDigest, encode(rec)); err != nil {
			return commands.Result{}, err
		}
		ev, err := m.event(c, "extension.package_imported", c.OperationID, 1, map[string]any{"extension_id": rec.ExtensionID, "extension_version": rec.ExtensionVersion, "package_digest": rec.PackageDigest})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: rec, Events: []event.Envelope{ev}}, nil
	})
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(res.Receipt.ResponseSummary, &out)
	return out, err
}

func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (m *Manager) packageRecord(ctx context.Context, q commands.DBTX, id, version string) (PackageRecord, error) {
	var rec PackageRecord
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record FROM extensions_packages WHERE extension_id=? AND extension_version=?`, id, version).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return rec, err
	}
	if err = json.Unmarshal([]byte(raw), &rec); err != nil {
		return rec, err
	}
	// Registered manifests are re-checked, so a tampered row fails closed.
	canonical, err := canonjson.CanonicalizeValue(rec.Manifest)
	if err != nil {
		return rec, err
	}
	if digest.Of(canonical) != rec.ManifestDigest || rec.Source != "package" || rec.Manifest.ID != id || rec.Manifest.Version != version {
		return rec, failure(errcode.HashMismatch, "registered_package_corrupt")
	}
	return rec, nil
}

// Packages lists imported packages with their current review facts.
func (m *Manager) Packages(ctx context.Context, who authz.Context) ([]PackageView, error) {
	if err := m.authorize(ctx, who, identity.ActReadExtensions); err != nil {
		return nil, err
	}
	rows, err := m.d.Main.QueryContext(ctx, `SELECT extension_id,extension_version FROM extensions_packages ORDER BY extension_id,extension_version LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	type key struct{ id, version string }
	keys := []key{}
	for rows.Next() {
		var k key
		if err = rows.Scan(&k.id, &k.version); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []PackageView{}
	for _, k := range keys {
		rec, err := m.packageRecord(ctx, m.d.Main, k.id, k.version)
		if err != nil {
			return nil, err
		}
		review, err := m.d.Packages.PackageReview(ctx, rec.Ref)
		if err != nil {
			return nil, err
		}
		out = append(out, PackageView{PackageRecord: rec, Review: review})
	}
	return out, nil
}

// Enablement is the main-database desired state for one plugin/target/scope.
type Enablement struct {
	ID               ids.ID           `json:"enablement_id"`
	ExtensionID      string           `json:"extension_id"`
	ExtensionVersion string           `json:"extension_version"`
	PackageDigest    digest.Digest    `json:"package_digest"`
	Target           string           `json:"target"`
	ScopeKind        string           `json:"scope_kind"`
	ScopeID          ids.ID           `json:"scope_id,omitempty"`
	State            string           `json:"state"`
	Revision         int64            `json:"revision"`
	Generation       int64            `json:"generation"`
	Entry            string           `json:"entry"`
	EntryDigest      digest.Digest    `json:"entry_digest"`
	Config           json.RawMessage  `json:"config"`
	ConfigDigest     digest.Digest    `json:"config_digest"`
	ConfigRevision   int64            `json:"config_revision"`
	PolicyRevision   int64            `json:"policy_revision"`
	Trust            string           `json:"trust"`
	Unenforced       []string         `json:"unenforced"`
	ProbeAuthorized  bool             `json:"probe_authorized"`
	ReviewRef        ids.PermanentRef `json:"review_ref"`
	ReviewID         ids.ID           `json:"review_id"`
	ChangedBy        ids.ID           `json:"changed_by"`
	HumanGrantID     ids.ID           `json:"human_grant_id"`
	OperationID      ids.ID           `json:"operation_id"`
	ChangedAt        string           `json:"changed_at"`
	Reason           string           `json:"reason"`
	Retired          *Retirement      `json:"retired,omitempty"`
}

// Retirement records how the previous generation ended: "drain" lets already
// admitted calls finish within their frozen deadline; "revoke" rejects them.
type Retirement struct {
	Generation int64  `json:"generation"`
	Mode       string `json:"mode"`
	At         string `json:"at"`
}

// EnableRequest is the complete HumanGrant-bound enable/upgrade/config request.
type EnableRequest struct {
	ExtensionID      string          `json:"extension_id"`
	ExtensionVersion string          `json:"extension_version"`
	PackageDigest    digest.Digest   `json:"package_digest"`
	Target           string          `json:"target"`
	ScopeKind        string          `json:"scope_kind"`
	ScopeID          ids.ID          `json:"scope_id,omitempty"`
	Config           json.RawMessage `json:"config"`
	ConfigRevision   int64           `json:"config_revision"`
	Trust            string          `json:"trust"`
	Probe            bool            `json:"probe"`
	Reason           string          `json:"reason"`
}

// DisableRequest stops new dispatch. Mode drain lets admitted calls finish in
// their frozen deadline; revoke immediately rejects results and cancels hosts.
type DisableRequest struct {
	EnablementID ids.ID `json:"enablement_id"`
	Mode         string `json:"mode"`
	Reason       string `json:"reason"`
}

func (m *Manager) enablementID(extension, target, scopeKind string, scopeID ids.ID) (ids.ID, error) {
	sum := sha256.Sum256([]byte(extension + "\x00" + target + "\x00" + scopeKind + "\x00" + string(scopeID)))
	return ids.DeriveChild(m.d.InstanceID, "extension-enablement:"+hex.EncodeToString(sum[:20]))
}

func (m *Manager) enablement(ctx context.Context, q commands.DBTX, id ids.ID) (*Enablement, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record FROM extensions_enablements WHERE enablement_id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Enablement
	return &e, json.Unmarshal([]byte(raw), &e)
}

// generationRecord returns the enablement record that defined a generation and
// the retirement of that generation if a later revision replaced it.
func (m *Manager) generationRecord(ctx context.Context, id ids.ID, generation int64) (Enablement, *Retirement, error) {
	rows, err := m.d.Main.QueryContext(ctx, `SELECT record FROM extensions_enablement_history WHERE enablement_id=? ORDER BY revision`, id)
	if err != nil {
		return Enablement{}, nil, err
	}
	defer rows.Close()
	var found *Enablement
	var retired *Retirement
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return Enablement{}, nil, err
		}
		var e Enablement
		if err = json.Unmarshal([]byte(raw), &e); err != nil {
			return Enablement{}, nil, err
		}
		if e.Generation == generation && e.State == "enabled" && found == nil {
			copy := e
			found = &copy
		}
		if found != nil && e.Retired != nil && e.Retired.Generation == generation {
			r := *e.Retired
			retired = &r
		}
	}
	if err = rows.Err(); err != nil {
		return Enablement{}, nil, err
	}
	if found == nil {
		return Enablement{}, nil, failure(errcode.ExtensionActivationStale, "generation_unknown")
	}
	return *found, retired, nil
}

// PolicyRevision is the instance revision of plugin policies bound into grants.
func (m *Manager) policyRevision(ctx context.Context) (int64, error) {
	p, err := m.d.Policies.ResolvePolicies(ctx, "")
	if err != nil {
		return 0, err
	}
	rev := int64(1)
	for k, v := range p {
		if strings.HasPrefix(k, "plugins.") && v.Revision > rev {
			rev = v.Revision
		}
	}
	return rev, nil
}

type enableFacts struct {
	id       ids.ID
	current  *Enablement
	record   PackageRecord
	config   []byte
	cdigest  digest.Digest
	crev     int64
	policy   int64
	review   PackageReview
	entry    File
	unenforc []string
}

// checkEnable validates every static fact of an enable request against current
// state. It runs before a challenge and again at final acceptance.
func (m *Manager) checkEnable(ctx context.Context, in EnableRequest) (enableFacts, error) {
	var f enableFacts
	switch {
	case in.Target != "server" && in.Target != "cli":
		return f, failure(errcode.ExtensionPointUnsupported, "target_unsupported")
	case in.Target == "server" && in.ScopeKind != "instance" && in.ScopeKind != "project":
		return f, failure(errcode.SchemaInvalid, "server_scope_invalid")
	case in.Target == "cli" && in.ScopeKind != "instance" && in.ScopeKind != "user":
		return f, failure(errcode.SchemaInvalid, "cli_scope_invalid")
	case (in.ScopeKind == "instance") != (in.ScopeID == ""), in.ScopeID != "" && !in.ScopeID.Valid():
		return f, failure(errcode.SchemaInvalid, "scope_id_invalid")
	case len(in.Reason) > 1024 || in.Trust != "" && in.Trust != TrustUnenforced:
		return f, failure(errcode.SchemaInvalid, "enable_request_invalid")
	}
	rec, err := m.packageRecord(ctx, m.d.Main, in.ExtensionID, in.ExtensionVersion)
	if err != nil {
		return f, err
	}
	if rec.PackageDigest != in.PackageDigest {
		return f, failure(errcode.HashMismatch, "package_digest_mismatch")
	}
	if err = CheckExternal(rec.Manifest); err != nil {
		return f, err
	}
	if _, ok := rec.Manifest.Targets[in.Target]; !ok {
		return f, failure(errcode.ExtensionPointUnsupported, "target_not_in_package")
	}
	f.review, err = m.d.Packages.PackageReview(ctx, rec.Ref)
	if err != nil {
		return f, err
	}
	if !f.review.Approved || !f.review.ReviewID.Valid() {
		return f, failure(errcode.ChecksNotSatisfied, "package_review_not_approved")
	}
	pkg, fsys, err := m.readPackage(ctx, rec)
	if err != nil {
		return f, err
	}
	f.entry, err = EntryPath(pkg.Manifest, in.Target)
	if err != nil && in.Target == "server" {
		return f, err
	}
	if in.Target == "cli" {
		// A CLI package may target another client platform; the client host
		// verifies its own platform artifact before running anything.
		t := pkg.Manifest.Targets["cli"]
		for _, a := range pkg.Manifest.PlatformArtifacts {
			if a.Path == t.Entry {
				f.entry = a.File
			}
		}
	}
	f.config, err = canonjson.Canonicalize(in.Config)
	if err != nil || len(f.config) == 0 || f.config[0] != '{' || len(f.config) > 64<<10 {
		return f, failure(errcode.SchemaInvalid, "config_object_required")
	}
	if err = validateConfig(fsys, rec, f.config); err != nil {
		return f, err
	}
	f.cdigest = digest.Of(f.config)
	// Server hosts cannot enforce memory, CPU, network or filesystem isolation,
	// and client machines are outside the server's control altogether.
	f.unenforc = m.caps.Unenforced(pkg.Manifest)
	if in.Target == "cli" {
		f.unenforc = []string{"client_host"}
	}
	if len(f.unenforc) > 0 && in.Trust != TrustUnenforced {
		return f, failure(errcode.ExtensionPointUnsupported, "isolation_insufficient")
	}
	if in.Target == "server" && !in.Probe {
		return f, failure(errcode.PreconditionRequired, "probe_authorization_required")
	}
	if in.Target == "cli" && in.Probe {
		return f, failure(errcode.ExtensionPointUnsupported, "client_probe_unsupported")
	}
	f.id, err = m.enablementID(in.ExtensionID, in.Target, in.ScopeKind, in.ScopeID)
	if err != nil {
		return f, err
	}
	f.current, err = m.enablement(ctx, m.d.Main, f.id)
	if err != nil {
		return f, err
	}
	f.crev = 1
	if c := f.current; c != nil {
		f.crev = c.ConfigRevision
		if c.ConfigDigest != f.cdigest {
			f.crev++
		}
	}
	if in.ConfigRevision != f.crev {
		return f, failure(errcode.PreconditionFailed, "config_revision_mismatch")
	}
	f.policy, err = m.policyRevision(ctx)
	f.record = rec
	return f, err
}

func validateConfig(fsys fstest.MapFS, rec PackageRecord, config []byte) error {
	schemaDoc, err := canonjson.Decode(fsys[rec.Manifest.ConfigSchema].Data)
	if err != nil {
		return failure(errcode.SchemaInvalid, "config_schema_invalid")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(noSchemaLoader{})
	if err = compiler.AddResource("https://lantai.invalid/package/config", schemaDoc); err != nil {
		return failure(errcode.SchemaInvalid, "config_schema_invalid")
	}
	s, err := compiler.Compile("https://lantai.invalid/package/config")
	if err != nil {
		return failure(errcode.SchemaInvalid, "config_schema_invalid")
	}
	doc, err := canonjson.Decode(config)
	if err != nil {
		return failure(errcode.SchemaInvalid, "config_invalid")
	}
	if err = s.Validate(doc); err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "configuration does not satisfy the package schema", err)
	}
	return nil
}

// EnableHumanAction fixes the complete request for a HumanGrant: package and
// config digests, target, scope, config/policy revisions and the next revision.
func (m *Manager) EnableHumanAction(ctx context.Context, in EnableRequest) (identity.HumanAction, error) {
	f, err := m.checkEnable(ctx, in)
	if err != nil {
		return identity.HumanAction{}, err
	}
	return m.enableAction(f, in)
}

func (m *Manager) enableAction(f enableFacts, in EnableRequest) (identity.HumanAction, error) {
	in.Config = f.config
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return identity.HumanAction{}, err
	}
	next := int64(1)
	if f.current != nil {
		next = f.current.Revision + 1
	}
	return identity.HumanAction{Action: identity.ActEnableExtension, ResourceID: f.id, ResourceRevision: next, Extension: &identity.ExtensionAuthorization{PluginID: in.ExtensionID, PackageDigest: in.PackageDigest, Target: in.Target, ScopeKind: in.ScopeKind, ScopeID: in.ScopeID, ConfigRevision: f.crev, ConfigDigest: f.cdigest, PolicyRevision: f.policy}, Request: raw}, nil
}

// DisableHumanAction binds the current enablement identity and next revision.
func (m *Manager) DisableHumanAction(ctx context.Context, in DisableRequest) (identity.HumanAction, error) {
	if in.Mode != "drain" && in.Mode != "revoke" || len(in.Reason) > 1024 {
		return identity.HumanAction{}, failure(errcode.SchemaInvalid, "disable_request_invalid")
	}
	e, err := m.enablement(ctx, m.d.Main, in.EnablementID)
	if err != nil {
		return identity.HumanAction{}, err
	}
	if e == nil {
		return identity.HumanAction{}, errcode.New(errcode.NotFound, "")
	}
	if e.State != "enabled" {
		return identity.HumanAction{}, errcode.New(errcode.InvalidStateTransition, "")
	}
	policy, err := m.policyRevision(ctx)
	if err != nil {
		return identity.HumanAction{}, err
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return identity.HumanAction{}, err
	}
	return identity.HumanAction{Action: identity.ActDisableExtension, ResourceID: e.ID, ResourceRevision: e.Revision + 1, Extension: &identity.ExtensionAuthorization{PluginID: e.ExtensionID, PackageDigest: e.PackageDigest, Target: e.Target, ScopeKind: e.ScopeKind, ScopeID: e.ScopeID, ConfigRevision: e.ConfigRevision, ConfigDigest: e.ConfigDigest, PolicyRevision: policy}, Request: raw}, nil
}

// ValidateHumanTarget verifies the frozen target identity before a challenge.
// Historical revisions stay valid targets; current state is rechecked at commit.
func (m *Manager) ValidateHumanTarget(ctx context.Context, _ authz.Context, a identity.HumanAction) error {
	switch a.Action {
	case identity.ActEnableExtension:
		var in EnableRequest
		if err := decodeStrict(a.Request, &in); err != nil {
			return err
		}
		id, err := m.enablementID(in.ExtensionID, in.Target, in.ScopeKind, in.ScopeID)
		if err != nil {
			return err
		}
		if id != a.ResourceID || a.Extension == nil || a.Extension.PluginID != in.ExtensionID || a.Extension.PackageDigest != in.PackageDigest {
			return errcode.New(errcode.RefMismatch, "")
		}
		_, err = m.packageRecord(ctx, m.d.Main, in.ExtensionID, in.ExtensionVersion)
		return err
	case identity.ActDisableExtension:
		var in DisableRequest
		if err := decodeStrict(a.Request, &in); err != nil {
			return err
		}
		e, err := m.enablement(ctx, m.d.Main, in.EnablementID)
		if err != nil {
			return err
		}
		if e == nil || e.ID != a.ResourceID || a.ResourceRevision > e.Revision+1 {
			return errcode.New(errcode.RefMismatch, "")
		}
		return nil
	}
	return errcode.New(errcode.UnsupportedCapability, "")
}

func decodeStrict(raw []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return failure(errcode.SchemaInvalid, "request_invalid")
	}
	return nil
}

// ChangeResult reports the committed enablement and, for server targets, the
// restricted probe outcome that decides whether the generation is dispatchable.
type ChangeResult struct {
	Receipt    commands.Receipt `json:"receipt"`
	Enablement *Enablement      `json:"enablement,omitempty"`
	Activation *Activation      `json:"activation,omitempty"`
}

// Enable consumes one HumanGrant item. After the main-database commit and
// outside every lock, a server target runs the probe the same grant authorized.
func (m *Manager) Enable(ctx context.Context, who authz.Context, in EnableRequest, grant, child ids.ID, human HumanCommands) (ChangeResult, error) {
	if human == nil {
		return ChangeResult{}, errcode.New(errcode.HumanProofRequired, "")
	}
	a, err := m.EnableHumanAction(ctx, in)
	if err != nil {
		return ChangeResult{}, err
	}
	r, err := human.AcceptHumanItem(ctx, who, grant, child, a, commands.Request{Security: commands.ModeExclusive}, &changeCommand{m: m, enable: &in})
	if err != nil {
		return ChangeResult{}, err
	}
	out := ChangeResult{Receipt: r}
	e, err := m.enablement(ctx, m.d.Main, a.ResourceID)
	if err != nil || e == nil {
		return out, err
	}
	out.Enablement = e
	if e.State == "enabled" && e.Target == "server" && e.ProbeAuthorized {
		act, err := m.runProbe(context.WithoutCancel(ctx), *e)
		out.Activation = &act
		return out, err
	}
	return out, nil
}

// Disable consumes one HumanGrant item and, for revoke, cancels live hosts.
func (m *Manager) Disable(ctx context.Context, who authz.Context, in DisableRequest, grant, child ids.ID, human HumanCommands) (ChangeResult, error) {
	if human == nil {
		return ChangeResult{}, errcode.New(errcode.HumanProofRequired, "")
	}
	a, err := m.DisableHumanAction(ctx, in)
	if err != nil {
		// A replay after the state changed still returns the original receipt.
		if prior, perr := m.store.ReceiptByOperation(ctx, m.d.Main, child); perr == nil {
			return ChangeResult{Receipt: *prior}, nil
		}
		return ChangeResult{}, err
	}
	r, err := human.AcceptHumanItem(ctx, who, grant, child, a, commands.Request{Security: commands.ModeExclusive}, &changeCommand{m: m, disable: &in})
	if err != nil {
		return ChangeResult{}, err
	}
	if in.Mode == "revoke" {
		m.cancelRunning(in.EnablementID)
	}
	e, err := m.enablement(ctx, m.d.Main, in.EnablementID)
	return ChangeResult{Receipt: r, Enablement: e}, err
}

type changeCommand struct {
	m       *Manager
	enable  *EnableRequest
	disable *DisableRequest
}

func (c *changeCommand) Receipt(ctx context.Context, id ids.ID) (commands.Receipt, error) {
	r, err := c.m.store.ReceiptByOperation(ctx, c.m.d.Main, id)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *r, nil
}

// Commit rebuilds the action from current state and requires it to equal the
// approved one, so a changed package, config, policy or revision is rejected.
func (c *changeCommand) Commit(ctx context.Context, cmd commands.Context, approved identity.HumanAction) (commands.Receipt, error) {
	m := c.m
	now := clock.Format(m.d.Clock.Now())
	var next Enablement
	var current *Enablement
	var action identity.HumanAction
	if c.enable != nil {
		f, err := m.checkEnable(ctx, *c.enable)
		if err != nil {
			return commands.Receipt{}, err
		}
		if action, err = m.enableAction(f, *c.enable); err != nil {
			return commands.Receipt{}, err
		}
		current = f.current
		next = Enablement{ID: f.id, ExtensionID: f.record.ExtensionID, ExtensionVersion: f.record.ExtensionVersion, PackageDigest: f.record.PackageDigest, Target: c.enable.Target, ScopeKind: c.enable.ScopeKind, ScopeID: c.enable.ScopeID, State: "enabled", Revision: 1, Generation: 1, Entry: f.entry.Path, EntryDigest: digest.Digest("sha256:" + f.entry.SHA256), Config: f.config, ConfigDigest: f.cdigest, ConfigRevision: f.crev, PolicyRevision: f.policy, Trust: c.enable.Trust, Unenforced: f.unenforc, ProbeAuthorized: c.enable.Probe, ReviewRef: f.record.Ref, ReviewID: f.review.ReviewID, Reason: c.enable.Reason}
		if current != nil {
			next.Revision, next.Generation = current.Revision+1, current.Generation+1
			if current.State == "enabled" {
				// Upgrade/config change is a normal drain of the previous generation.
				next.Retired = &Retirement{Generation: current.Generation, Mode: "drain", At: now}
			}
		}
	} else {
		var err error
		if action, err = m.DisableHumanAction(ctx, *c.disable); err != nil {
			return commands.Receipt{}, err
		}
		current, err = m.enablement(ctx, m.d.Main, c.disable.EnablementID)
		if err != nil || current == nil {
			return commands.Receipt{}, errors.Join(err, errcode.New(errcode.NotFound, ""))
		}
		next = *current
		next.State, next.Revision, next.Generation, next.Reason = "disabled", current.Revision+1, current.Generation+1, c.disable.Reason
		next.Retired = &Retirement{Generation: current.Generation, Mode: c.disable.Mode, At: now}
	}
	a, _ := canonjson.CanonicalizeValue(action)
	b, _ := canonjson.CanonicalizeValue(approved)
	if string(a) != string(b) {
		return commands.Receipt{}, failure(errcode.PreconditionFailed, "enablement_changed_since_approval")
	}
	next.ChangedBy, next.HumanGrantID, next.OperationID, next.ChangedAt = cmd.ActorID, cmd.HumanGrantID, cmd.OperationID, now
	res, err := m.store.Execute(ctx, m.d.Main, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		prior, err := m.enablement(ctx, tx, next.ID)
		if err != nil {
			return commands.Result{}, err
		}
		if (prior == nil) != (current == nil) || prior != nil && prior.Revision != current.Revision {
			return commands.Result{}, errcode.New(errcode.PreconditionFailed, "")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO extensions_enablements(enablement_id,extension_id,target,scope_kind,scope_id,revision,generation,state,record) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(enablement_id) DO UPDATE SET revision=excluded.revision,generation=excluded.generation,state=excluded.state,record=excluded.record`, next.ID, next.ExtensionID, next.Target, next.ScopeKind, string(next.ScopeID), next.Revision, next.Generation, next.State, encode(next)); err != nil {
			return commands.Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO extensions_enablement_history(enablement_id,revision,record) VALUES(?,?,?)`, next.ID, next.Revision, encode(next)); err != nil {
			return commands.Result{}, err
		}
		ev, err := m.event(cmd, "extension.enablement_changed", next.ID, next.Revision, map[string]any{"extension_id": next.ExtensionID, "extension_version": next.ExtensionVersion, "target": next.Target, "scope_kind": next.ScopeKind, "state": next.State, "generation": next.Generation})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: next, Events: []event.Envelope{ev}}, nil
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *res.Receipt, nil
}

// Enablements lists desired state with host activation and breaker facts.
func (m *Manager) Enablements(ctx context.Context, who authz.Context) ([]EnablementView, error) {
	if err := m.authorize(ctx, who, identity.ActReadExtensions); err != nil {
		return nil, err
	}
	list, err := m.listEnablements(ctx, "", "")
	if err != nil {
		return nil, err
	}
	out := []EnablementView{}
	for _, e := range list {
		v, err := m.view(ctx, e)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (m *Manager) listEnablements(ctx context.Context, target, state string) ([]Enablement, error) {
	rows, err := m.d.Main.QueryContext(ctx, `SELECT record FROM extensions_enablements WHERE (?='' OR target=?) AND (?='' OR state=?) ORDER BY extension_id,target,scope_kind,scope_id LIMIT 1000`, target, target, state, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Enablement{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e Enablement
		if err = json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnablementView is a diagnostic projection; it never grants anything.
type EnablementView struct {
	Enablement
	Review     PackageReview          `json:"review"`
	Activation *Activation            `json:"activation,omitempty"`
	Breaker    *BreakerView           `json:"breaker,omitempty"`
	Reasons    []string               `json:"reasons"`
	Host       *IsolationCapabilities `json:"host,omitempty"`
}

func (m *Manager) view(ctx context.Context, e Enablement) (EnablementView, error) {
	v := EnablementView{Enablement: e, Reasons: []string{}}
	var err error
	if v.Review, err = m.d.Packages.PackageReview(ctx, e.ReviewRef); err != nil {
		return v, err
	}
	if e.State != "enabled" {
		v.Reasons = append(v.Reasons, "disabled")
	}
	if !v.Review.Approved || v.Review.ReviewID != e.ReviewID {
		v.Reasons = append(v.Reasons, "package_review_revoked")
	}
	if e.Target == "server" {
		caps := m.caps
		v.Host = &caps
		rec, err := m.packageRecord(ctx, m.d.Main, e.ExtensionID, e.ExtensionVersion)
		if err != nil {
			return v, err
		}
		if !slices.Equal(m.caps.Unenforced(rec.Manifest), e.Unenforced) {
			v.Reasons = append(v.Reasons, "isolation_changed")
		}
		act, err := m.activation(ctx, e.ID, e.Generation)
		if err != nil {
			return v, err
		}
		v.Activation = act
		switch {
		case act == nil:
			v.Reasons = append(v.Reasons, "probe_required")
		case act.State == "probe_failed":
			v.Reasons = append(v.Reasons, "probe_failed")
		case act.EnvironmentDigest != m.environmentDigest(e):
			v.Reasons = append(v.Reasons, "probe_stale")
		}
		b, err := m.breakerView(ctx, m.breakerKey(e))
		if err != nil {
			return v, err
		}
		v.Breaker = b
		if b != nil && b.State == "open" {
			v.Reasons = append(v.Reasons, "breaker_open")
		}
	}
	return v, nil
}
