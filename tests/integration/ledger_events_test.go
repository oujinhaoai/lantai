package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func (e *env) ingest(who authz.Context, slug string, data []byte, r manifest.Rights, uses ...catalog.DeclaredUse) catalog.VersionResult {
	e.t.Helper()
	ctx := e.t.Context()
	u, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: who, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}}})
	if err != nil {
		e.t.Fatal(err)
	}
	_, err = e.storage.PutPart(ctx, storage.PartRequest{Who: who, UploadID: u.UploadID, SHA256: shaOf(data), PartNumber: 1, PartSHA256: shaOf(data), Size: int64(len(data)), Body: bytes.NewReader(data)})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err = e.storage.CompleteFile(ctx, who, u.UploadID, shaOf(data)); err != nil {
		e.t.Fatal(err)
	}
	res, err := e.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: u.UploadID, Slug: slug, Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: &r, Uses: uses, Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}
func (e *env) eventQuery() (*events.Store, *query.Service) {
	e.t.Helper()
	es := events.New(e.inst.DB(ownership.Events), e.clk, e.inst.Gate())
	q, err := query.New(query.Deps{DB: e.inst.DB(ownership.Index), Gate: e.inst.Gate(), Reader: e.ledger, Catalog: e.catalog, Events: es, Authz: e.id, Rights: e.rights, InstanceID: e.inst.InstanceID()})
	if err != nil {
		e.t.Fatal(err)
	}
	return es, q
}
func (e *env) relay(es *events.Store) {
	e.t.Helper()
	for _, db := range []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime} {
		source := commands.OutboxSource{Label: string(db), DB: e.inst.DB(db), Gate: e.inst.Gate()}
		for {
			n, err := es.Relay(e.t.Context(), source, 1000)
			if err != nil {
				e.t.Fatal(err)
			}
			if n == 0 {
				break
			}
		}
	}
}
func checkDecision(t *testing.T, e *env, who authz.Context, ref ids.PermanentRef, p authz.Purpose, want errcode.Code) {
	t.Helper()
	d, err := e.rights.EvaluateUse(t.Context(), who, ref, p)
	if err != nil {
		t.Fatal(err)
	}
	if (want == "" && !d.Allowed) || (want != "" && (d.Allowed || errcode.CodeOf(d.Err()) != want)) {
		t.Fatalf("purpose %s: %+v want %s", p, d, want)
	}
}

func TestLedgerEventsQueryWithAuthoritativeRights(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	_, ms := e.agent("maker@node-a", identity.RoleContributor)
	_, curator := e.agent("curator@node-b", identity.RoleCurator)
	_, viewer := e.agent("viewer@node-c", identity.RoleViewer)
	r := *rightsOwned()
	r.NoAI = true
	original := e.ingest(ms.Context, "资料/设计规则", []byte("same synthetic source"), r)
	ref := ids.PermanentRef{InstanceID: e.inst.InstanceID(), AssetID: original.AssetID, VersionID: original.VersionID}
	derived := e.ingest(ms.Context, "资料/派生成果", []byte("derived"), *rightsOwned(), catalog.DeclaredUse{AssetID: ref.AssetID, VersionID: ref.VersionID, Relation: "derived_from"})
	dref := ids.PermanentRef{InstanceID: e.inst.InstanceID(), AssetID: derived.AssetID, VersionID: derived.VersionID}
	checkDecision(t, e, viewer.Context, dref, authz.PurposeGenerativeInput, errcode.UseRestricted)
	checkDecision(t, e, viewer.Context, dref, authz.PurposeProduction, "")
	es, q := e.eventQuery()
	e.relay(es)
	if _, err := q.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := q.Search(ctx, query.SearchRequest{Who: viewer.Context, Filter: query.Filter{ProjectID: e.project.ProjectID, Text: "设计"}, Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("Chinese text projection: %+v %v", page, err)
	}
	relations, err := q.Relations(ctx, viewer.Context, dref, false)
	if err != nil || len(relations) != 1 {
		t.Fatalf("relations: %+v %v", relations, err)
	}
	// 同哈希的两个合法来源仍是两个候选，不能挑较宽许可。
	r.NoAI = false
	e.ingest(ms.Context, "资料/同字节来源", []byte("same synthetic source"), r)
	matches, err := e.rights.MatchHash(ctx, viewer.Context, shaOf([]byte("same synthetic source")))
	if err != nil || !matches.Ambiguous || len(matches.Candidates) != 2 {
		t.Fatalf("hash candidates: %+v %v", matches, err)
	}
	// 证据在文件成功写入与台账接受之后生效；索引不追赶也即时返回 pending。
	req := provenance.AppendRequest{Who: curator.Context, IdempotencyKey: e.key(), Ref: ref, ManifestDigest: original.ManifestDigest, Evidence: provenance.Evidence{Note: "unknown external input", ExternalInputs: []provenance.ExternalInput{{RightsStatus: "unknown", SourceURL: "https://example.test/source"}}}}
	accepted, err := e.rights.AppendEvidence(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.rights.AppendEvidence(ctx, req)
	if err != nil || again.RecordID != accepted.RecordID {
		t.Fatalf("evidence replay: %+v %v", again, err)
	}
	checkDecision(t, e, viewer.Context, dref, authz.PurposeProduction, errcode.RightsPending)
	// 更正只是追加，无法让旧未知来源消失。
	req.IdempotencyKey = e.key()
	req.ExpectedRevision = 1
	req.Supersedes = accepted.RecordID
	req.Evidence = provenance.Evidence{Note: "more context", ExternalInputs: []provenance.ExternalInput{}}
	if _, err = e.rights.AppendEvidence(ctx, req); err != nil {
		t.Fatal(err)
	}
	checkDecision(t, e, viewer.Context, dref, authz.PurposeProduction, errcode.RightsPending)
	// 普通著录也不能擦除 noai 或原 uses。
	if _, err = e.catalog.PatchAsset(ctx, curator.Context, e.key(), original.AssetID, 1, catalog.AssetPatch{Title: ptr("更新标题")}); err != nil {
		t.Fatal(err)
	}
	checkDecision(t, e, viewer.Context, dref, authz.PurposeGenerativeInput, errcode.UseRestricted)
	req.IdempotencyKey = e.key()
	req.ExpectedRevision = 0
	if _, err = e.rights.AppendEvidence(ctx, req); errcode.CodeOf(err) != errcode.PreconditionFailed {
		t.Fatalf("old evidence baseline: %v", err)
	}
	e.relay(es)
	if _, err = q.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	audit, err := es.ExportAudit(ctx, filepath.Join(e.inst.Layout().Home, "audit"), 1000)
	if err != nil || audit.Through == 0 {
		t.Fatalf("audit: %+v %v", audit, err)
	}
	// 维护屏障拦截新增业务、投影和审计；权威只读继续可用。
	_, hold, err := e.inst.Gate().Maintain(ctx, commands.ReasonMaintenance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.CatchUp(ctx); errcode.CodeOf(err) != errcode.MaintenanceMode {
		t.Fatalf("query maintenance: %v", err)
	}
	if _, err = es.ExportAudit(ctx, filepath.Join(e.inst.Layout().Home, "audit"), 1000); errcode.CodeOf(err) != errcode.MaintenanceMode {
		t.Fatalf("audit maintenance: %v", err)
	}
	if _, err = e.catalog.GetVersion(ctx, viewer.Context, original.AssetID, original.VersionID); err != nil {
		t.Fatal(err)
	}
	hold.Release()
	e.inst.Gate().Open()
	// 损坏已接受的证据绝不能被忽略。
	path := filepath.Join(e.inst.Layout().Home, filepath.FromSlash(accepted.Ref))
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	checkDecision(t, e, viewer.Context, ref, authz.PurposeProduction, errcode.RightsPending)
}

func TestPersonalReadRequiresExplicitGrantAndRechecksStaleProjection(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	maker, ms := e.agent("maker@node-a", identity.RoleContributor)
	viewer, vs := e.agent("viewer@node-c", identity.RoleViewer)
	r := *rightsOwned()
	r.Sensitivity = "personal"
	v := e.ingest(ms.Context, "资料/私人", []byte("private synthetic"), r)
	ref := ids.PermanentRef{InstanceID: e.inst.InstanceID(), AssetID: v.AssetID, VersionID: v.VersionID}
	checkDecision(t, e, vs.Context, ref, authz.PurposeArchiveReview, errcode.UseRestricted)
	es, q := e.eventQuery()
	e.relay(es)
	if _, err := q.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := q.Search(ctx, query.SearchRequest{Who: vs.Context, Limit: 10})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("personal visible without grant: %+v %v", p, err)
	}
	value, _ := json.Marshal([]ids.ID{viewer.ID, maker.ID})
	e.sudo(&identity.SetPolicy{ProjectID: e.project.ProjectID, Key: "personal.readers", ExpectedRevision: 0, Value: value})
	checkDecision(t, e, vs.Context, ref, authz.PurposeArchiveReview, "")
	g, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: vs.Context, AssetID: v.AssetID, VersionID: v.VersionID, Path: "content.txt", Purpose: authz.PurposeArchiveReview})
	if err != nil {
		t.Fatal(err)
	}
	e.sudo(&identity.SetPolicy{ProjectID: e.project.ProjectID, Key: "personal.readers", ExpectedRevision: 1, Value: json.RawMessage(`[]`)})
	p, err = q.Search(ctx, query.SearchRequest{Who: vs.Context, Limit: 10})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("stale projection after explicit revoke: %+v %v", p, err)
	}
	if r := e.http("GET", g.URL, vs.Token, map[string]string{"Range": "bytes=0-3"}, nil, 0); r.status < 400 {
		t.Fatalf("old grant remained usable: %d", r.status)
	}
	e.relay(es)
	change, err := q.Changes(ctx, vs.Context, 0, 1000)
	if err != nil || change.HighWater == 0 || !change.ResyncRequired || len(change.Items) != 0 {
		t.Fatalf("filtered changes: %+v %v", change, err)
	}
}

func TestIndexDeletionRebuildsFromRealLedgerAndFiles(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	_, maker := e.agent("maker@node-a", identity.RoleContributor)
	_, viewer := e.agent("viewer@node-b", identity.RoleViewer)
	source := e.ingest(maker.Context, "资料/来源", []byte("synthetic source"), *rightsOwned())
	derived := e.ingest(maker.Context, "资料/成果", []byte("synthetic derived"), *rightsOwned(), catalog.DeclaredUse{AssetID: source.AssetID, VersionID: source.VersionID, Relation: "derived_from"})
	ref := ids.PermanentRef{InstanceID: e.inst.InstanceID(), AssetID: derived.AssetID, VersionID: derived.VersionID}
	es, _ := e.eventQuery()
	e.relay(es)
	path := filepath.Join(t.TempDir(), "index.db")
	var originalItems []query.Item
	var originalRelations []query.Related
	for attempt := 0; attempt < 2; attempt++ {
		db, err := sqlite.Open(ctx, path, sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err = query.EnsureSchema(ctx, db); err != nil {
			db.Close()
			t.Fatal(err)
		}
		q, err := query.New(query.Deps{DB: db, Gate: e.inst.Gate(), Reader: e.ledger, Catalog: e.catalog, Events: es, Authz: e.id, Rights: e.rights, InstanceID: e.inst.InstanceID()})
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		if _, err = q.Rebuild(ctx); err != nil {
			db.Close()
			t.Fatal(err)
		}
		page, err := q.Search(ctx, query.SearchRequest{Who: viewer.Context, Filter: query.Filter{Text: "资料"}, Limit: 10})
		if err != nil || len(page.Items) != 2 {
			db.Close()
			t.Fatalf("rebuilt directory: %+v %v", page, err)
		}
		relations, err := q.Relations(ctx, viewer.Context, ref, false)
		if err != nil || len(relations) != 1 {
			db.Close()
			t.Fatalf("rebuilt relations: %+v %v", relations, err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			originalItems, originalRelations = page.Items, relations
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else if !reflect.DeepEqual(page.Items, originalItems) || !reflect.DeepEqual(relations, originalRelations) {
			t.Fatal("deleting index changed authoritative directory or relations")
		}
	}
}
