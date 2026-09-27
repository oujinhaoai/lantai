package catalog

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
)

func ptr[T any](v T) *T { return &v }

func TestProjectsAreRegisteredOnceAndDescribed(t *testing.T) {
	f := newFixture(t)
	got, err := f.svc.GetProject(t.Context(), f.admin, "pansi")
	if err != nil || got.ProjectID != f.project.ProjectID || got.Description.Name != "盘丝洞" || got.Description.Revision != 1 {
		t.Fatalf("project = %+v %v", got, err)
	}
	snapshot, err := os.ReadFile(filepath.Join(f.storage.Layout().ProjectDir(f.project.ProjectID), "project.yaml"))
	if err != nil || !strings.Contains(string(snapshot), "contract: lantai.project/v1") {
		t.Fatalf("project.yaml snapshot: %v\n%s", err, snapshot)
	}
	// 同一幂等键重放返回原项目；key 已被占用 PATH_CONFLICT；非管理员不能登记。
	req := ProjectRequest{Who: f.admin, IdempotencyKey: "reg-lib", Key: "library", Name: "素材库", ProjectType: "library"}
	p1, err := f.svc.CreateProject(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !p1.DescriptionPending || p1.DescriptionError != errcode.Forbidden || p1.Description.Revision != 0 {
		t.Fatalf("description without project permission must stay pending: %+v", p1)
	}
	f.az.Grant(f.admin.PrincipalID, p1.ProjectID, ActionPatchProject)
	p2, err := f.svc.CreateProject(t.Context(), req)
	if err != nil || p2.ProjectID != p1.ProjectID || p2.Description.Revision != 1 || p2.Description.Name != "素材库" {
		t.Fatalf("replay = %+v %v", p2, err)
	}
	_, err = f.svc.CreateProject(t.Context(), ProjectRequest{Who: f.admin, IdempotencyKey: "reg-dup", Key: "library"})
	wantCode(t, err, errcode.PathConflict)
	_, err = f.svc.CreateProject(t.Context(), ProjectRequest{Who: f.agent, IdempotencyKey: "reg-agent", Key: "agents"})
	wantCode(t, err, errcode.Forbidden)
	_, err = f.svc.CreateProject(t.Context(), ProjectRequest{Who: f.admin, IdempotencyKey: "reg-bad", Key: "Bad Key"})
	wantCode(t, err, errcode.SchemaInvalid)
	// 条件修改项目说明。
	d, err := f.svc.PatchProject(t.Context(), f.admin, "pp-1", f.project.ProjectID, 1, ProjectPatch{Summary: ptr("第 3 章白模")})
	if err != nil || d.Revision != 2 || d.Summary != "第 3 章白模" || d.Name != "盘丝洞" {
		t.Fatalf("patch project = %+v %v", d, err)
	}
	_, err = f.svc.PatchProject(t.Context(), f.admin, "pp-2", f.project.ProjectID, 1, ProjectPatch{Summary: ptr("stale")})
	wantCode(t, err, errcode.PreconditionFailed)
	_, err = f.svc.PatchProject(t.Context(), f.agent, "pp-3", f.project.ProjectID, 2, ProjectPatch{Summary: ptr("agent")})
	wantCode(t, err, errcode.Forbidden)
}

func TestCreateAssetCommitsOnceAndResolves(t *testing.T) {
	f := newFixture(t)
	files := map[string][]byte{"whitebox.glb": []byte("glTF-bytes"), "scene.blend": []byte("blend-bytes")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(files)...)
	req := VersionRequest{Who: f.agent, IdempotencyKey: "create-1", UploadID: u.UploadID, Slug: "ch03/whitebox",
		Content: contentOf(manifest.TypeModel, files), Describe: &AssetPatch{Title: ptr("第 3 章出口白模"), Tags: ptr([]string{"白模", "ch03", "白模"})}}
	res, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.VersionNumber != 1 || res.AliasGeneration != 1 || res.Slug != "ch03/whitebox" || res.OperationID != u.OperationID ||
		!strings.HasPrefix(res.URI, "lantai://") || res.Description == nil || res.Description.Revision != 1 {
		t.Fatalf("result = %+v", res)
	}
	if res.Description.Title != "第 3 章出口白模" || strings.Join(res.Description.Tags, ",") != "ch03,白模" || res.Description.AssetType != manifest.TypeModel {
		t.Fatalf("description = %+v", res.Description)
	}
	// 响应丢失后重试：同一版本，不产生第二个版本。
	again, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil || again.VersionID != res.VersionID || again.AssetID != res.AssetID {
		t.Fatalf("replay = %+v %v", again, err)
	}
	all, _ := f.ledger.Versions(t.Context(), "", 10)
	if len(all) != 1 {
		t.Fatalf("versions after replay = %d", len(all))
	}
	// 同键异请求冲突。
	changed := req
	changed.Content = contentOf(manifest.TypeModel, map[string][]byte{"whitebox.glb": []byte("glTF-bytes")})
	_, err = f.svc.CommitVersion(t.Context(), changed)
	wantCode(t, err, errcode.IdempotencyConflict)

	// 解析：固定版本、最新版本与永久引用都指向同一版本。
	for _, ref := range []string{"pansi/ch03/whitebox@v001", "pansi/ch03/whitebox@latest", "pansi/CH03/WhiteBox@v001"} {
		r, err := f.svc.Resolve(t.Context(), f.agent, ref, 0)
		if err != nil || r.VersionID != res.VersionID || r.Generation != 1 {
			t.Fatalf("Resolve(%s) = %+v %v", ref, r, err)
		}
	}
	r, err := f.svc.ResolvePermanent(t.Context(), f.agent, res.Ref)
	if err != nil || r.VersionID != res.VersionID {
		t.Fatalf("permanent = %+v %v", r, err)
	}
	_, err = f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@published", 0)
	wantCode(t, err, errcode.NotPublished)
	_, err = f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@v002", 0)
	wantCode(t, err, errcode.NotFound)
	_, err = f.svc.Resolve(t.Context(), f.agent, "other::pansi/ch03/whitebox@v001", 0)
	wantReason(t, err, errcode.SchemaInvalid, "federation_unsupported")
	_, err = f.svc.ResolvePermanent(t.Context(), f.agent, ids.PermanentRef{AssetID: ids.New(), VersionID: res.VersionID})
	wantCode(t, err, errcode.NotFound)

	// 精确读取：清单文件的内容与台账一致。
	v, err := f.svc.GetVersion(t.Context(), f.agent, res.AssetID, res.VersionID)
	if err != nil || v.Manifest.ManifestDigest != res.ManifestDigest || len(v.Manifest.Content.Files) != 2 || v.Manifest.Content.Files[0].Path != "scene.blend" {
		t.Fatalf("version = %+v %v", v, err)
	}
	info, err := f.svc.GetAsset(t.Context(), f.agent, res.AssetID)
	if err != nil || info.Latest.VersionID != res.VersionID || info.Claim.State != commit.ClaimActive || info.Description.Title != "第 3 章出口白模" {
		t.Fatalf("asset = %+v %v", info, err)
	}
	// 上传会话随提交关闭，别名历史写出。
	up, _ := f.storage.GetUpload(t.Context(), f.agent, u.UploadID)
	if up.State != storage.UploadCompleted {
		t.Fatalf("upload state after commit = %s", up.State)
	}
	if _, err := os.Stat(f.svc.aliasPath(f.project.ProjectID, "ch03/whitebox", 1)); err != nil {
		t.Fatal("alias history record missing")
	}
}

// 无权主体无论对象是否存在都得到 NOT_FOUND；也不能在别人的项目入藏。
func TestStrangersLearnNothing(t *testing.T) {
	f := newFixture(t)
	res := f.create(f.agent, "secret/asset", manifest.TypeDoc, map[string][]byte{"a.md": []byte("a")})
	stranger := f.newAgent(ids.New())
	for _, ref := range []string{"pansi/secret/asset@v001", "pansi/nothing/here@v001", "nokey/secret/asset@v001"} {
		_, err := f.svc.Resolve(t.Context(), stranger, ref, 0)
		wantCode(t, err, errcode.NotFound)
	}
	_, err := f.svc.ResolvePermanent(t.Context(), stranger, res.Ref)
	wantCode(t, err, errcode.NotFound)
	_, err = f.svc.GetAsset(t.Context(), stranger, res.AssetID)
	wantCode(t, err, errcode.NotFound)
	_, err = f.svc.GetVersion(t.Context(), stranger, res.AssetID, res.VersionID)
	wantCode(t, err, errcode.NotFound)
	_, err = f.svc.PatchAsset(t.Context(), stranger, "p", res.AssetID, 1, AssetPatch{Title: ptr("x")})
	wantCode(t, err, errcode.NotFound)
}

func TestAppendChecksBaseTypeAndInheritsRights(t *testing.T) {
	f := newFixture(t)
	v1 := f.create(f.agent, "motions/vault", manifest.TypeMotion, map[string][]byte{"vault.fbx": []byte("v1")})
	v2 := f.appendVersion(f.agent, v1.AssetID, v1.VersionID, manifest.TypeMotion, map[string][]byte{"vault.fbx": []byte("v2")})
	if v2.VersionNumber != 2 || v2.AliasGeneration != 0 || v2.Description != nil {
		t.Fatalf("append = %+v", v2)
	}
	u := f.upload(f.agent, f.project.ProjectID, []byte("v3"))
	stale := VersionRequest{Who: f.agent, IdempotencyKey: "stale", UploadID: u.UploadID, AssetID: v1.AssetID, BaseVersionID: v1.VersionID,
		Content: contentOf(manifest.TypeMotion, map[string][]byte{"vault.fbx": []byte("v3")})}
	_, err := f.svc.CommitVersion(t.Context(), stale)
	wantCode(t, err, errcode.BaseVersionConflict)
	retyped := stale
	retyped.IdempotencyKey, retyped.BaseVersionID = "retyped", v2.VersionID
	retyped.Content.AssetType = manifest.TypeImage
	_, err = f.svc.CommitVersion(t.Context(), retyped)
	wantReason(t, err, errcode.SchemaInvalid, "asset_type_immutable")
	// 不给 rights 时沿用资产的默认许可。
	inherit := stale
	inherit.IdempotencyKey, inherit.BaseVersionID = "inherit", v2.VersionID
	inherit.Content.Rights = nil
	v3, err := f.svc.CommitVersion(t.Context(), inherit)
	if err != nil || v3.VersionNumber != 3 {
		t.Fatalf("inherit rights: %+v %v", v3, err)
	}
	doc, _ := f.svc.GetVersion(t.Context(), f.agent, v3.AssetID, v3.VersionID)
	if doc.Manifest.Content.Rights.License != "LicenseRef-Owned" || doc.Manifest.Content.BaseVersionID != v2.VersionID {
		t.Fatalf("v3 manifest = %+v", doc.Manifest.Content)
	}
	// 新建资产必须声明 rights。
	u2 := f.upload(f.agent, f.project.ProjectID, []byte("x"))
	c := contentOf(manifest.TypeDoc, map[string][]byte{"x.md": []byte("x")})
	c.Rights = nil
	_, err = f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "norights", UploadID: u2.UploadID, Slug: "docs/x", Content: c})
	wantReason(t, err, errcode.SchemaInvalid, "rights_required")
}

// 名称释放后同名新资产取得新代次：旧永久引用不被接管，缺代次的固定版本引用
// 明确报 REF_AMBIGUOUS，浮动引用指向当前代次。
func TestNameReuseKeepsPermanentReferences(t *testing.T) {
	f := newFixture(t)
	old := f.create(f.agent, "ch03/whitebox", manifest.TypeModel, map[string][]byte{"a.glb": []byte("old")})
	if err := f.ledger.ReleaseName(f.project.ProjectID, "ch03/whitebox"); err != nil { // M2 的释放命令用测试构造器模拟
		t.Fatal(err)
	}
	// 释放后、复用前：从未复用过的路径仍可精确解析到旧资产，但没有当前资产。
	if r, err := f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@v001", 0); err != nil || r.AssetID != old.AssetID {
		t.Fatalf("released path, fixed version: %+v %v", r, err)
	}
	_, err := f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@latest", 0)
	wantCode(t, err, errcode.NotFound)

	fresh := f.create(f.agent, "ch03/whitebox", manifest.TypeModel, map[string][]byte{"a.glb": []byte("new")})
	if fresh.AliasGeneration != 2 || fresh.AssetID == old.AssetID {
		t.Fatalf("reuse = %+v", fresh)
	}
	_, err = f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@v001", 0)
	wantReason(t, err, errcode.RefAmbiguous, "path_reused")
	cases := map[int64]ids.ID{1: old.VersionID, 2: fresh.VersionID}
	for gen, want := range cases {
		r, err := f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@v001", gen)
		if err != nil || r.VersionID != want || r.Generation != gen {
			t.Fatalf("generation %d: %+v %v", gen, r, err)
		}
	}
	_, err = f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@v001", 3)
	wantCode(t, err, errcode.NotFound)
	if r, err := f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@latest", 0); err != nil || r.AssetID != fresh.AssetID {
		t.Fatalf("floating after reuse: %+v %v", r, err)
	}
	_, err = f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@latest", 1)
	wantReason(t, err, errcode.SchemaInvalid, "generation_with_floating")
	if r, err := f.svc.ResolvePermanent(t.Context(), f.agent, old.Ref); err != nil || r.AssetID != old.AssetID || r.Generation != 1 {
		t.Fatalf("old permanent ref: %+v %v", r, err)
	}
	// 旧代次的历史文件缺失时明确要求对账，不猜测；按台账补齐后恢复。
	if err := os.Remove(f.svc.aliasPath(f.project.ProjectID, "ch03/whitebox", 1)); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@v001", 1)
	wantReason(t, err, errcode.OperationNeedsReconciliation, "alias_history_missing")
	rep, err := f.svc.RepairAliasHistory(t.Context())
	if err != nil || rep.Written != 1 || rep.Checked != 2 {
		t.Fatalf("repair = %+v %v", rep, err)
	}
	if r, err := f.svc.Resolve(t.Context(), f.agent, "pansi/ch03/whitebox@v001", 1); err != nil || r.AssetID != old.AssetID {
		t.Fatalf("after repair: %+v %v", r, err)
	}
}

func TestAssetDescriptionRevisions(t *testing.T) {
	f := newFixture(t)
	res := f.create(f.agent, "chars/fourth-sister", manifest.TypeModel, map[string][]byte{"body.glb": []byte("body")})
	if res.Description.Revision != 1 || res.Description.Title != "fourth-sister" {
		t.Fatalf("default description = %+v", res.Description)
	}
	d2, err := f.svc.PatchAsset(t.Context(), f.agent, "t-1", res.AssetID, 1, AssetPatch{Title: ptr("四妹"), Subjects: ptr([]string{"fourth-sister"})})
	if err != nil || d2.Revision != 2 || d2.Title != "四妹" || d2.Sensitivity != "normal" || d2.Defaults.License != "LicenseRef-Owned" {
		t.Fatalf("patch = %+v %v", d2, err)
	}
	// 过期的预期修订被拒绝；安全字段只能经专门命令修改。
	_, err = f.svc.PatchAsset(t.Context(), f.agent, "t-2", res.AssetID, 1, AssetPatch{Title: ptr("stale")})
	wantCode(t, err, errcode.PreconditionFailed)
	_, err = f.svc.PatchAsset(t.Context(), f.agent, "t-3", res.AssetID, 2, AssetPatch{Sensitivity: ptr("personal")})
	wantCode(t, err, errcode.FieldRequiresSpecialCommand)
	_, err = f.svc.PatchAsset(t.Context(), f.agent, "t-3b", res.AssetID, 2, AssetPatch{Defaults: &Defaults{License: "CC0-1.0"}})
	wantCode(t, err, errcode.FieldRequiresSpecialCommand)
	// 另一个制作者不能改别人的资产著录；整理者可以。
	other := f.newAgent(f.project.ProjectID)
	_, err = f.svc.PatchAsset(t.Context(), other, "t-4", res.AssetID, 2, AssetPatch{Title: ptr("hijack")})
	wantCode(t, err, errcode.Forbidden)
	d3, err := f.svc.PatchAsset(t.Context(), f.admin, "t-5", res.AssetID, 2, AssetPatch{Summary: ptr("整理者补充说明")})
	if err != nil || d3.Revision != 3 || d3.Title != "四妹" || d3.UpdatedBy != f.admin.PrincipalID {
		t.Fatalf("curator patch = %+v %v", d3, err)
	}
	// 重放一个已生效、之后又有新修订的操作：返回原结果，不回退当前修订。
	replay, err := f.svc.PatchAsset(t.Context(), f.agent, "t-1", res.AssetID, 1, AssetPatch{Title: ptr("四妹"), Subjects: ptr([]string{"fourth-sister"})})
	if err != nil || replay.Revision != 2 {
		t.Fatalf("replay = %+v %v", replay, err)
	}
	info, _ := f.svc.GetAsset(t.Context(), f.agent, res.AssetID)
	if info.Description.Revision != 3 {
		t.Fatalf("current revision after replay = %d", info.Description.Revision)
	}
	// 旧修订全部保留在 .history 中，快照是当前修订。
	hist, _ := filepath.Glob(filepath.Join(f.storage.Layout().AssetDir(res.ProjectID, res.AssetID), ".history", "asset.r*.yaml"))
	if len(hist) != 3 {
		t.Fatalf("history files = %v", hist)
	}
	snap, _ := os.ReadFile(filepath.Join(f.storage.Layout().AssetDir(res.ProjectID, res.AssetID), "asset.yaml"))
	if !strings.Contains(string(snap), "revision: 3") {
		t.Fatalf("snapshot is not the current revision:\n%s", snap)
	}
	// 修订文件被绕过服务端改动：读取时发现摘要不符。
	cur, _ := f.ledger.CurrentMetadata(t.Context(), commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: res.ProjectID, ID: res.AssetID})
	p := f.svc.revisionPath(cur.Target, cur.Revision, cur.OperationID)
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.GetAsset(t.Context(), f.agent, res.AssetID)
	wantCode(t, err, errcode.HashMismatch)
}

// 清单与已上传内容不符时放弃操作、释放占名；缺内容时保留 prepared，补传后
// 以同一幂等键重试成功。
func TestInstallFailuresRelease(t *testing.T) {
	f := newFixture(t)
	data := []byte("real content")
	u := f.upload(f.agent, f.project.ProjectID, data)
	lying := contentOf(manifest.TypeDoc, map[string][]byte{"a.md": data})
	lying.Files[0].Size++ // 申报的大小与上传内容不符
	_, err := f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "lie", UploadID: u.UploadID, Slug: "docs/a", Content: lying})
	wantCode(t, err, errcode.HashMismatch)
	if _, err := f.ledger.Claim(t.Context(), f.project.ProjectID, "docs/a"); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("a failed create kept the name: %v", err)
	}

	missing := []byte("uploaded later")
	u2 := f.upload(f.agent, f.project.ProjectID, data)
	files := map[string][]byte{"a.md": data, "b.md": missing}
	req := VersionRequest{Who: f.agent, IdempotencyKey: "later", UploadID: u2.UploadID, Slug: "docs/b", Content: contentOf(manifest.TypeDoc, files)}
	_, err = f.svc.CommitVersion(t.Context(), req)
	wantCode(t, err, errcode.BlobGrantRequired)
	// 在另一个会话里上传缺的内容：授权属于另一个操作，不能用于本操作。
	f.upload(f.agent, f.project.ProjectID, missing)
	_, err = f.svc.CommitVersion(t.Context(), req)
	wantCode(t, err, errcode.BlobGrantRequired)
	// 本操作的会话没有申报缺的内容：放弃这次入藏（释放占名），用新会话重新开始。
	if err := f.svc.CancelVersion(t.Context(), f.agent, u2.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ledger.Claim(t.Context(), f.project.ProjectID, "docs/b"); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("cancelled create kept the name: %v", err)
	}
	u3 := f.upload(f.agent, f.project.ProjectID, data, missing)
	req.UploadID, req.IdempotencyKey = u3.UploadID, "later-2"
	if res, err := f.svc.CommitVersion(t.Context(), req); err != nil || res.AliasGeneration != 1 {
		t.Fatalf("after cancel: %+v %v", res, err)
	}
}

// 可补救的故障（文件被占用）保留 prepared；期间浮动引用解析到了新版本，
// 以同一幂等键重试时仍使用冻结时的内容。
func TestRetryUsesFrozenContent(t *testing.T) {
	f := newFixture(t)
	storyboard := f.create(f.agent, "storyboard/ch03", manifest.TypeImage, map[string][]byte{"board.png": []byte("board v1")})
	shot := map[string][]byte{"shot.mp4": []byte("render")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(shot)...)
	c := contentOf(manifest.TypeProduction, shot)
	c.Uses = []DeclaredUse{{Ref: "pansi/storyboard/ch03@latest", Relation: "uses"}}
	req := VersionRequest{Who: f.agent, IdempotencyKey: "shot-1", UploadID: u.UploadID, Slug: "ch03/shot-01", Content: c}
	f.faults.Rename = func(string, string) error { return syscall.EBUSY }
	_, err := f.svc.CommitVersion(t.Context(), req)
	wantCode(t, err, errcode.StorageUnavailable)
	f.faults.Rename = nil
	f.appendVersion(f.agent, storyboard.AssetID, storyboard.VersionID, manifest.TypeImage, map[string][]byte{"board.png": []byte("board v2")})
	res, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := f.svc.GetVersion(t.Context(), f.agent, res.AssetID, res.VersionID)
	if len(doc.Manifest.Content.Uses) != 1 || doc.Manifest.Content.Uses[0].VersionID != storyboard.VersionID ||
		doc.Manifest.Content.Uses[0].Declared != "pansi/storyboard/ch03@latest" {
		t.Fatalf("uses = %+v; the retry must keep the input frozen at prepare time", doc.Manifest.Content.Uses)
	}
	if _, err := os.Stat(f.svc.frozenDir(res.OperationID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("frozen content should be cleaned up after commit")
	}
}

func TestUsesAreCheckedAgainstRestrictions(t *testing.T) {
	f := newFixture(t)
	src := f.create(f.agent, "lib/mixamo-vault", manifest.TypeMotion, map[string][]byte{"vault.fbx": []byte("mocap")})
	f.rights.Restrict(src.VersionID, authz.PurposeProduction)
	shot := map[string][]byte{"shot.mp4": []byte("render")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(shot)...)
	c := contentOf(manifest.TypeProduction, shot)
	c.Uses = []DeclaredUse{{AssetID: src.AssetID, VersionID: src.VersionID, Relation: "uses"}}
	_, err := f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "r1", UploadID: u.UploadID, Slug: "ch03/shot-02", Content: c})
	wantCode(t, err, errcode.UseRestricted)
	// 只作参考的关系按参考用途判定。
	c.Uses[0].Relation = "reference"
	res, err := f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "r2", UploadID: u.UploadID, Slug: "ch03/shot-02", Content: c})
	if err != nil || res.VersionNumber != 1 {
		t.Fatalf("reference use: %+v %v", res, err)
	}
}

// 规范化失败发生在台账保留之前：不占名、不保留版本号。
func TestInvalidManifestReservesNothing(t *testing.T) {
	f := newFixture(t)
	u := f.upload(f.agent, f.project.ProjectID, []byte("a"), []byte("b"))
	c := ContentInput{AssetType: manifest.TypeDoc, Rights: &manifest.Rights{Usage: "production", License: "MIT", Sensitivity: "normal"},
		Files: []manifest.InputFile{{Path: "Read.md", Role: "doc", SHA256: shaOf([]byte("a")), Size: 1},
			{Path: "READ.md", Role: "doc", SHA256: shaOf([]byte("b")), Size: 1}}}
	_, err := f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "bad", UploadID: u.UploadID, Slug: "docs/read", Content: c})
	wantCode(t, err, errcode.PathConflict)
	_, err = f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "bad2", UploadID: u.UploadID, Slug: "docs/nul", Content: c})
	wantReason(t, err, errcode.SchemaInvalid, "reserved_name")
	_, err = f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "bad3", UploadID: u.UploadID, Slug: "../escape", Content: c})
	wantCode(t, err, errcode.SchemaInvalid)
	if _, err := f.ledger.Claim(t.Context(), f.project.ProjectID, "docs/read"); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal("an invalid manifest reserved the name")
	}
}
