package provenance

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/authz/authztest"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type authority struct {
	commit.Reader
	versions map[ids.ID]commit.Committed
	assets   map[ids.ID]catalog.AssetInfo
}

func (a *authority) ReadProjection(_ context.Context, id ids.ID) (catalog.AssetInfo, error) {
	v, ok := a.assets[id]
	if !ok {
		return v, errcode.New(errcode.NotFound, "")
	}
	return v, nil
}
func (a *authority) Version(_ context.Context, asset, version ids.ID) (commit.Committed, error) {
	v, ok := a.versions[version]
	if !ok {
		return v, errcode.New(errcode.NotFound, "")
	}
	if v.AssetID != asset {
		return v, errcode.New(errcode.RefMismatch, "")
	}
	return v, nil
}
func (a *authority) Versions(_ context.Context, after ids.ID, limit int) ([]commit.Committed, error) {
	var out []commit.Committed
	for id, v := range a.versions {
		if id > after {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b commit.Committed) int { return strings.Compare(string(a.VersionID), string(b.VersionID)) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type files struct {
	manifests   map[ids.ID][]byte
	records     map[ids.ID][]byte
	appendCount int
	afterAppend func()
	failAppend  error
}

func (f *files) ReadManifest(_ context.Context, v commit.Committed) ([]byte, error) {
	b, ok := f.manifests[v.VersionID]
	if !ok {
		return nil, errcode.New(errcode.NotFound, "")
	}
	return slices.Clone(b), nil
}
func (f *files) AppendRecord(_ context.Context, r storage.Record) (storage.RecordRef, error) {
	if f.failAppend != nil {
		return storage.RecordRef{}, f.failAppend
	}
	raw, err := canonjson.CanonicalizeValue(struct {
		Contract string `json:"contract"`
		storage.Record
	}{storage.RecordContract, r})
	if err != nil {
		return storage.RecordRef{}, err
	}
	old, ok := f.records[r.RecordID]
	if ok && string(old) != string(raw) {
		return storage.RecordRef{}, errcode.New(errcode.IdempotencyConflict, "")
	}
	f.records[r.RecordID] = raw
	f.appendCount++
	if f.afterAppend != nil {
		fn := f.afterAppend
		f.afterAppend = nil
		fn()
	}
	return storage.RecordRef{RecordID: r.RecordID, Digest: digest.Of(raw), Ref: "records/" + string(r.RecordID) + ".json", Created: !ok}, nil
}
func (f *files) ReadRecord(_ context.Context, _, _, _, record ids.ID) ([]byte, error) {
	raw, ok := f.records[record]
	if !ok {
		return nil, errcode.New(errcode.NotFound, "")
	}
	return slices.Clone(raw), nil
}

type fixture struct {
	t                 *testing.T
	s                 *Service
	db                *sql.DB
	az                *authztest.Static
	reader            *authority
	files             *files
	who               authz.Context
	instance, project ids.ID
	clk               *clock.Fake
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, m := range migrations.For(ownership.Ledger) {
		if _, err = db.ExecContext(ctx, m.SQL); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{t: t, db: db, instance: ids.New(), project: ids.New(), clk: clock.NewFake(time.Now()), reader: &authority{versions: map[ids.ID]commit.Committed{}, assets: map[ids.ID]catalog.AssetInfo{}}, files: &files{manifests: map[ids.ID][]byte{}, records: map[ids.ID][]byte{}}}
	f.az = authztest.New(f.clk, f.instance)
	principal := ids.New()
	f.az.AddPrincipal(principal, authz.Human)
	f.az.Grant(principal, f.project, "catalog.read", ActionAppendEvidence)
	f.who = f.az.OpenSession(principal, time.Hour)
	gate := commands.NewGate(commands.NewCoordinator())
	gate.Open()
	f.s, err = New(Deps{DB: db, Gate: gate, Reader: f.reader, Catalog: f.reader, Files: f.files, Authz: f.az, InstanceID: f.instance, Clock: f.clk, IDs: &ids.Generator{Clock: f.clk, Rand: rand.Reader}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *fixture) add(r manifest.Rights, uses ...manifest.Use) commit.Committed {
	f.t.Helper()
	if r.Usage == "" {
		r = manifest.Rights{Usage: "production", License: "LicenseRef-Owned", RedistributeRaw: true, Sensitivity: "normal"}
	}
	content, err := manifest.Normalize(manifest.Input{AssetType: manifest.TypeDoc, Files: []manifest.InputFile{{Path: "file.txt", Role: "primary", SHA256: strings.Repeat("1", 64), Size: 1}}, Rights: r, Uses: uses})
	if err != nil {
		f.t.Fatal(err)
	}
	md, err := content.Digest()
	if err != nil {
		f.t.Fatal(err)
	}
	v := commit.Committed{ProjectID: f.project, AssetID: ids.New(), VersionID: ids.New(), VersionNumber: 1, ManifestDigest: md, OperationID: ids.New(), CommittedBy: f.who.PrincipalID}
	doc := manifest.Document{InstanceID: f.instance, ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, VersionNumber: 1, OperationID: v.OperationID, CreatedBy: v.CommittedBy, ManifestDigest: md, Content: content}
	raw, err := doc.Render()
	if err != nil {
		f.t.Fatal(err)
	}
	f.reader.versions[v.VersionID] = v
	f.reader.assets[v.AssetID] = catalog.AssetInfo{Asset: commit.Asset{AssetID: v.AssetID, ProjectID: v.ProjectID}, Description: catalog.AssetDescription{Sensitivity: r.Sensitivity}, Latest: v}
	f.files.manifests[v.VersionID] = raw
	return v
}
func (f *fixture) req(v commit.Committed) AppendRequest {
	return AppendRequest{Who: f.who, IdempotencyKey: "evidence-" + string(ids.New()), Ref: v.Ref(f.instance), ManifestDigest: v.ManifestDigest, Evidence: Evidence{Note: "synthetic evidence", ExternalInputs: []ExternalInput{}}}
}
func (f *fixture) evaluate(v commit.Committed, p authz.Purpose) bool {
	f.t.Helper()
	d, err := f.s.EvaluateUse(f.t.Context(), f.who, v.Ref(f.instance), p)
	if err != nil {
		f.t.Fatal(err)
	}
	return d.Allowed
}
func code(t *testing.T, err error, c errcode.Code) {
	t.Helper()
	if err == nil || errcode.CodeOf(err) != c {
		t.Fatalf("got %v; want %s", err, c)
	}
}

func TestUnknownAndTransitiveRestrictionsNeverLoosen(t *testing.T) {
	f := setup(t)
	r := manifest.Rights{Usage: "production", License: "LicenseRef-Unknown", RedistributeRaw: true, Sensitivity: "normal"}
	src := f.add(r)
	child := f.add(manifest.Rights{}, manifest.Use{InstanceID: f.instance, AssetID: src.AssetID, VersionID: src.VersionID, Relation: "reference"})
	if f.evaluate(child, authz.PurposeProduction) {
		t.Fatal("unknown source allowed production")
	}
	if !f.evaluate(child, authz.PurposeArchiveReview) {
		t.Fatal("unknown source blocked permitted archive review")
	}
	normal := f.add(manifest.Rights{})
	request := f.req(normal)
	request.Evidence.ExternalInputs = []ExternalInput{{RightsStatus: "unknown", SourceURL: "https://example.invalid/source"}}
	accepted, err := f.s.AppendEvidence(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if f.evaluate(normal, authz.PurposeProduction) {
		t.Fatal("accepted unknown external input allowed production")
	}
	correction := f.req(normal)
	correction.ExpectedRevision = 1
	correction.Supersedes = accepted.RecordID
	correction.Evidence.Note = "ordinary correction does not erase old source"
	if _, err = f.s.AppendEvidence(t.Context(), correction); err != nil {
		t.Fatal(err)
	}
	if f.evaluate(normal, authz.PurposeProduction) {
		t.Fatal("correction erased immutable unknown evidence")
	}
}

func TestEvidenceCrashRetryConflictAndOrphanIsolation(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	r := f.req(v)
	r.Evidence.ExternalInputs = []ExternalInput{{RightsStatus: "unknown"}}
	f.files.failAppend = errors.New("injected file failure")
	if _, err := f.s.AppendEvidence(t.Context(), r); err == nil {
		t.Fatal("expected file failure")
	}
	if !f.evaluate(v, authz.PurposeProduction) {
		t.Fatal("prepared evidence became visible without file/acceptance")
	}
	f.files.failAppend = nil
	got, err := f.s.AppendEvidence(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.s.AppendEvidence(t.Context(), r)
	if err != nil || got.RecordID != again.RecordID || got.OperationID != again.OperationID || f.files.appendCount != 1 {
		t.Fatalf("retry changed identity: %+v %+v calls=%d err=%v", got, again, f.files.appendCount, err)
	}
	r.Evidence.Note = "different bytes"
	_, err = f.s.AppendEvidence(t.Context(), r)
	code(t, err, errcode.IdempotencyConflict)
	var n int
	if err = f.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("outbox=%d err=%v", n, err)
	}
}

func TestEvidenceRechecksRevocationAndProducerAtAcceptance(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	r := f.req(v)
	f.files.afterAppend = func() { f.az.Revoke(f.who.PrincipalID, f.project) }
	_, err := f.s.AppendEvidence(t.Context(), r)
	code(t, err, errcode.TokenRevoked)
	var n int
	if err = f.db.QueryRow(`SELECT count(*) FROM provenance_records`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("revoked evidence accepted: n=%d err=%v", n, err)
	}
	if len(f.files.records) != 1 {
		t.Fatal("installed orphan was not retained")
	}
	f.az.Grant(f.who.PrincipalID, f.project, "catalog.read", ActionAppendEvidence)
	f.who = f.az.OpenSession(f.who.PrincipalID, time.Hour)
	r.Who = f.who
	if _, err = f.s.AppendEvidence(t.Context(), r); err != nil {
		t.Fatal("same principal could not recover retained evidence", err)
	}
	producer := f.req(v)
	producer.ExpectedRevision = 1
	producer.Producer = &storage.Producer{ExtensionID: "test", ExtensionVersion: "1.0.0", PackageDigest: digest.Of([]byte("package")), Source: "builtin"}
	_, err = f.s.AppendEvidence(t.Context(), producer)
	code(t, err, errcode.SchemaInvalid)
}

func TestEvidenceCorruptionAndManifestMismatchFailClosed(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	r := f.req(v)
	r.ManifestDigest = digest.Of([]byte("wrong"))
	_, err := f.s.AppendEvidence(t.Context(), r)
	code(t, err, errcode.PreconditionFailed)
	r = f.req(v)
	got, err := f.s.AppendEvidence(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	f.files.records[got.RecordID] = []byte(`{"tampered":true}`)
	d, err := f.s.EvaluateUse(t.Context(), f.who, v.Ref(f.instance), authz.PurposeProduction)
	if err != nil || d.Allowed || d.Code != errcode.RightsPending {
		t.Fatalf("tampered evidence not pending: %+v %v", d, err)
	}
	v2 := f.add(manifest.Rights{})
	delete(f.files.manifests, v2.VersionID)
	d, err = f.s.EvaluateUse(t.Context(), f.who, v2.Ref(f.instance), authz.PurposeArchiveReview)
	if err != nil || d.Allowed || d.Code != errcode.RightsPending {
		t.Fatalf("missing manifest not pending: %+v %v", d, err)
	}
}

func TestPersonalManifestRequiresExplicitReadAndHashKeepsAmbiguity(t *testing.T) {
	f := setup(t)
	personal := f.add(manifest.Rights{Usage: "production", License: "LicenseRef-Owned", RedistributeRaw: true, Sensitivity: "personal"})
	if f.evaluate(personal, authz.PurposeArchiveReview) {
		t.Fatal("project membership bypassed personal permission")
	}
	f.az.Grant(f.who.PrincipalID, f.project, ActionPersonalRead)
	if !f.evaluate(personal, authz.PurposeArchiveReview) {
		t.Fatal("explicit personal permission rejected")
	}
	f.add(manifest.Rights{})
	matches, err := f.s.MatchHash(t.Context(), f.who, strings.Repeat("1", 64))
	if err != nil || !matches.Ambiguous || len(matches.Candidates) != 2 {
		t.Fatalf("hash ambiguity erased: %+v %v", matches, err)
	}
}

func TestDependencyBudgetsFailClosed(t *testing.T) {
	f := setup(t)
	src := f.add(manifest.Rights{})
	derived := f.add(manifest.Rights{}, manifest.Use{InstanceID: f.instance, AssetID: src.AssetID, VersionID: src.VersionID, Relation: "uses"})
	f.s.MaxNodes = 1
	d, err := f.s.EvaluateUse(t.Context(), f.who, derived.Ref(f.instance), authz.PurposeProduction)
	if err != nil || d.Allowed || d.Code != errcode.RightsPending {
		t.Fatalf("budget truncation allowed use: %+v %v", d, err)
	}
}

func TestRecordJSONIsStableAcrossRetry(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	r := f.req(v)
	f.files.afterAppend = func() { f.s.Gate.Close(commands.ReasonMaintenance) }
	_, err := f.s.AppendEvidence(t.Context(), r)
	code(t, err, errcode.MaintenanceMode)
	var initial []byte
	for _, b := range f.files.records {
		initial = slices.Clone(b)
	}
	f.clk.Advance(time.Minute)
	f.s.Gate.Open()
	got, err := f.s.AppendEvidence(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if string(initial) != string(f.files.records[got.RecordID]) {
		t.Fatal("retry rewrote frozen evidence")
	}
	var record storage.Record
	if err = json.Unmarshal(initial, &record); err != nil || record.SessionID != r.Who.SessionID {
		t.Fatalf("record identity lost: %+v %v", record, err)
	}
}

type staticProducer struct {
	want   storage.Producer
	active bool
	calls  int
}

func (p *staticProducer) VerifyProducer(_ context.Context, got storage.Producer) error {
	p.calls++
	if !p.active || got != p.want {
		return errcode.New(errcode.Forbidden, "producer is not statically registered")
	}
	return nil
}
func TestStaticProducerIdentityAndFinalRecheck(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	producer := storage.Producer{ExtensionID: "org.example.checker", ExtensionVersion: "1.0.0", PackageDigest: digest.Of([]byte("registered package")), Source: "builtin", ContributionID: "validator.check"}
	verifier := &staticProducer{want: producer, active: true}
	f.s.Producers = verifier
	r := f.req(v)
	r.Producer = &producer
	got, err := f.s.AppendEvidence(t.Context(), r)
	if err != nil || verifier.calls != 2 {
		t.Fatalf("statically registered producer rejected or not rechecked: %+v %v calls=%d", got, err, verifier.calls)
	}
	bad := producer
	bad.PackageDigest = digest.Of([]byte("other package"))
	r = f.req(v)
	r.ExpectedRevision = 1
	r.Producer = &bad
	_, err = f.s.AppendEvidence(t.Context(), r)
	code(t, err, errcode.Forbidden)
	r = f.req(v)
	r.ExpectedRevision = 1
	r.Producer = &producer
	f.files.afterAppend = func() { verifier.active = false }
	_, err = f.s.AppendEvidence(t.Context(), r)
	code(t, err, errcode.Forbidden)
	rev, err := f.s.CurrentRevision(t.Context(), v.VersionID)
	if err != nil || rev != 1 {
		t.Fatalf("deactivated producer accepted: rev=%d err=%v", rev, err)
	}
}

func TestCurrentPersonalDescriptionAndMissingCatalogFailClosed(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	if !f.evaluate(v, authz.PurposeRawExport) {
		t.Fatal("normal owned source rejected")
	}
	a := f.reader.assets[v.AssetID]
	a.Description.Sensitivity = "personal"
	f.reader.assets[v.AssetID] = a
	if f.evaluate(v, authz.PurposeRawExport) {
		t.Fatal("current personal description bypassed by normal frozen manifest")
	}
	f.az.Grant(f.who.PrincipalID, f.project, ActionPersonalRead)
	if !f.evaluate(v, authz.PurposeRawExport) {
		t.Fatal("explicit personal reader rejected")
	}
	f.s.SetCatalog(nil)
	d, err := f.s.EvaluateUse(t.Context(), f.who, v.Ref(f.instance), authz.PurposeRawExport)
	if err != nil || d.Allowed || d.Code != errcode.RightsPending {
		t.Fatalf("missing current description adapter did not fail closed: %+v %v", d, err)
	}
}

func TestGlobalRightsEpochChangesForEverySource(t *testing.T) {
	f := setup(t)
	a, b := f.add(manifest.Rights{}), f.add(manifest.Rights{})
	derived := f.add(manifest.Rights{}, manifest.Use{InstanceID: f.instance, AssetID: a.AssetID, VersionID: a.VersionID, Relation: "uses"}, manifest.Use{InstanceID: f.instance, AssetID: b.AssetID, VersionID: b.VersionID, Relation: "uses"})
	for revision := int64(0); revision < 3; revision++ {
		r := f.req(a)
		r.ExpectedRevision = revision
		if _, err := f.s.AppendEvidence(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.s.EvaluateUse(t.Context(), f.who, derived.Ref(f.instance), authz.PurposeProduction)
	if err != nil || !before.Allowed {
		t.Fatalf("unexpected baseline: %+v %v", before, err)
	}
	r := f.req(b)
	r.Evidence.ExternalInputs = []ExternalInput{{RightsStatus: "unknown"}}
	accepted, err := f.s.AppendEvidence(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.s.EvaluateUse(t.Context(), f.who, derived.Ref(f.instance), authz.PurposeProduction)
	if err != nil || after.Allowed || after.RightsEpoch <= before.RightsEpoch || after.RightsEpoch != accepted.RightsEpoch {
		t.Fatalf("source change left epoch unchanged: before=%+v after=%+v record=%+v err=%v", before, after, accepted, err)
	}
}

func TestHashScanBudgetPreservesAlreadyKnownAmbiguity(t *testing.T) {
	f := setup(t)
	for range 4 {
		f.add(manifest.Rights{})
	}
	f.s.MaxNodes = 2
	got, err := f.s.MatchHash(t.Context(), f.who, strings.Repeat("1", 64))
	if err != nil || !got.Pending || !got.Ambiguous || len(got.Candidates) != 2 {
		t.Fatalf("partial scan erased established ambiguity: %+v %v", got, err)
	}
}

func TestHashScanMarksUnverifiableVisibleCandidatesPending(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	delete(f.files.manifests, v.VersionID)
	got, err := f.s.MatchHash(t.Context(), f.who, strings.Repeat("1", 64))
	if err != nil || !got.Pending || len(got.Candidates) != 0 {
		t.Fatalf("incomplete verification looked exhaustive: %+v %v", got, err)
	}
}

func TestEvidenceAppendRequiresCurrentPersonalReadBeforeWriting(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	a := f.reader.assets[v.AssetID]
	a.Description.Sensitivity = "personal"
	f.reader.assets[v.AssetID] = a
	_, err := f.s.AppendEvidence(t.Context(), f.req(v))
	code(t, err, errcode.NotFound)
	if f.files.appendCount != 0 || len(f.files.records) != 0 {
		t.Fatal("unreadable personal target produced file side effects")
	}
	var n int
	if err = f.db.QueryRow(`SELECT count(*) FROM provenance_prepared`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unreadable target accepted prepare n=%d err=%v", n, err)
	}
	f.az.Grant(f.who.PrincipalID, f.project, ActionPersonalRead)
	if _, err = f.s.AppendEvidence(t.Context(), f.req(v)); err != nil {
		t.Fatal("explicitly authorized evidence rejected", err)
	}
}

func TestMissingAcceptedRecordCannotEraseUnknownSource(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	r := f.req(v)
	r.Evidence.ExternalInputs = []ExternalInput{{RightsStatus: "unknown"}}
	accepted, err := f.s.AppendEvidence(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`DELETE FROM provenance_records WHERE record_id=?`, accepted.RecordID); err != nil {
		t.Fatal(err)
	}
	d, err := f.s.EvaluateUse(t.Context(), f.who, v.Ref(f.instance), authz.PurposeProduction)
	if err != nil || d.Allowed || d.Code != errcode.RightsPending {
		t.Fatalf("missing accepted record silently loosened rights: %+v %v", d, err)
	}
}
