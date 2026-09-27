package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// T02 使用的授权动作都登记在身份模块的权限矩阵中（未登记的动作一律拒绝）。
func TestT02ActionsAreRegistered(t *testing.T) {
	for _, a := range []authz.Action{storage.ActionUpload, storage.ActionReadContent, catalog.ActionRead, catalog.ActionCreateAsset,
		catalog.ActionPatchMetadata, catalog.ActionPatchOwnMetadata, catalog.ActionCreateProject, catalog.ActionPatchProject} {
		if _, ok := identity.Spec(a); !ok {
			t.Errorf("action %s is not registered in identity", a)
		}
	}
}

// 真实身份模块下的纵向链路：Agent 用长期凭据换会话 → 经传输面分片上传 →
// catalog 入藏 → 签发读取授权 → 经传输面 Range 下载；转发地址、别人的会话、
// 撤销角色与结束会话之后的新请求都被拒绝。
func TestIngestAndDownloadWithRealIdentity(t *testing.T) {
	e := newEnv(t, storage.Config{PartSize: 64 << 10, SinglePartMax: 64 << 10, MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	maker, ms := e.agent("maker@node-a", identity.RoleContributor)
	viewerP, viewer := e.agent("viewer@node-b", identity.RoleViewer)
	_, outsider := e.agent("outsider@node-c", "")

	body := bytes.Repeat([]byte("synthetic glTF payload "), 10000) // 约 230 KiB，4 个分片
	readme := []byte("# 合成资产\n")
	u, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: ms.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID,
		Files: []storage.FileSpec{{SHA256: shaOf(body), Size: int64(len(body))}, {SHA256: shaOf(readme), Size: int64(len(readme))}}})
	if err != nil {
		t.Fatal(err)
	}
	e.putParts(ms.Token, u, body)
	e.putParts(ms.Token, u, readme)
	for _, c := range [][]byte{body, readme} {
		if _, err := e.storage.CompleteFile(ctx, ms.Context, u.UploadID, shaOf(c)); err != nil {
			t.Fatal(err)
		}
	}
	// 查看者不能入藏；没有角色的主体看不到项目。
	if _, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: viewer.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID}); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatalf("viewer upload: %v", err)
	}
	if _, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: outsider.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID}); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("outsider upload: %v", err)
	}

	content := catalog.ContentInput{AssetType: manifest.TypeModel, Rights: rightsOwned(), Files: []manifest.InputFile{
		{Path: "model/body.glb", Role: "primary", SHA256: shaOf(body), Size: int64(len(body))},
		{Path: "README.md", Role: "doc", SHA256: shaOf(readme), Size: int64(len(readme))}}}
	req := catalog.VersionRequest{Who: ms.Context, IdempotencyKey: "ingest-1", UploadID: u.UploadID, Slug: "ch03/whitebox", Content: content,
		Describe: &catalog.AssetPatch{Title: ptr("第 3 章白模")}}
	res, err := e.catalog.CommitVersion(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.VersionNumber != 1 || res.Description == nil || res.Description.UpdatedBy != maker.ID {
		t.Fatalf("ingest = %+v", res)
	}
	if again, err := e.catalog.CommitVersion(ctx, req); err != nil || again.VersionID != res.VersionID {
		t.Fatalf("replay = %+v %v", again, err)
	}
	r, err := e.catalog.Resolve(ctx, viewer.Context, "pansi/ch03/whitebox@latest", 0)
	if err != nil || r.VersionID != res.VersionID {
		t.Fatalf("viewer resolve: %+v %v", r, err)
	}
	if _, err := e.catalog.Resolve(ctx, outsider.Context, "pansi/ch03/whitebox@latest", 0); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("outsider resolve: %v", err)
	}

	g, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: viewer.Context, AssetID: res.AssetID, VersionID: res.VersionID,
		Path: "model/body.glb", Purpose: authz.PurposeReference})
	if err != nil {
		t.Fatal(err)
	}
	full := e.http("GET", g.URL, viewer.Token, nil, nil, 0)
	if full.status != 200 || !bytes.Equal(full.body, body) {
		t.Fatalf("download: %d, %d bytes", full.status, len(full.body))
	}
	rng := e.http("GET", g.URL, viewer.Token, map[string]string{"Range": "bytes=100-199"}, nil, 0)
	if rng.status != 206 || !bytes.Equal(rng.body, body[100:200]) {
		t.Fatalf("range: %d", rng.status)
	}
	if r := e.http("GET", g.URL, "", nil, nil, 0); r.status != 401 {
		t.Fatalf("URL without the holder's session: %d", r.status)
	}
	if r := e.http("GET", g.URL, ms.Token, nil, nil, 0); r.status != 404 {
		t.Fatalf("forwarded to another principal: %d", r.status)
	}
	if _, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: outsider.Context, AssetID: res.AssetID, VersionID: res.VersionID,
		Path: "model/body.glb", Purpose: authz.PurposeReference}); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("outsider read grant: %v", err)
	}

	// 撤销查看者的角色：已签发、签名仍有效的授权在下一个 Range 请求即失效。
	e.setRole(viewerP.ID, identity.RoleViewer, false)
	if r := e.http("GET", g.URL, viewer.Token, map[string]string{"Range": "bytes=200-299"}, nil, 0); r.status < 400 {
		t.Fatalf("download after role revocation: %d", r.status)
	}

	// 制作者结束会话：旧令牌的下载请求被拒绝，新会话需要新的读取授权。
	g2, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: ms.Context, AssetID: res.AssetID, VersionID: res.VersionID,
		Path: "README.md", Purpose: authz.PurposeProduction})
	if err != nil {
		t.Fatal(err)
	}
	if r := e.http("GET", g2.URL, ms.Token, nil, nil, 0); r.status != 200 || !bytes.Equal(r.body, readme) {
		t.Fatalf("maker download: %d", r.status)
	}
	if err := e.id.EndSession(ctx, ms.Context, ms.Context.SessionID); err != nil {
		t.Fatal(err)
	}
	if r := e.http("GET", g2.URL, ms.Token, nil, nil, 0); r.status != 401 {
		t.Fatalf("download after the session ended: %d", r.status)
	}
}

// 真实身份模块下的著录修订：制作者改自己建立的资产，其他制作者不能改；
// 负责人可以改；过期修订与安全字段被拒绝。
func TestDescriptionRevisionsWithRealIdentity(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	_, ms := e.agent("maker@node-a", identity.RoleContributor)
	_, other := e.agent("other@node-b", identity.RoleContributor)
	_, curator := e.agent("curator@node-c", identity.RoleCurator)
	data := []byte("doc")
	u, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: ms.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID,
		Files: []storage.FileSpec{{SHA256: shaOf(data), Size: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	e.putParts(ms.Token, u, data)
	if _, err := e.storage.CompleteFile(ctx, ms.Context, u.UploadID, shaOf(data)); err != nil {
		t.Fatal(err)
	}
	res, err := e.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: ms.Context, IdempotencyKey: e.key(), UploadID: u.UploadID,
		Slug: "docs/rules", Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(),
			Files: []manifest.InputFile{{Path: "rules.md", Role: "doc", SHA256: shaOf(data), Size: 3}}}})
	if err != nil || res.Description == nil {
		t.Fatalf("ingest: %+v %v", res, err)
	}
	if _, err := e.catalog.PatchAsset(ctx, ms.Context, e.key(), res.AssetID, 1, catalog.AssetPatch{Title: ptr("项目规矩")}); err != nil {
		t.Fatalf("own patch: %v", err)
	}
	if _, err := e.catalog.PatchAsset(ctx, other.Context, e.key(), res.AssetID, 2, catalog.AssetPatch{Title: ptr("x")}); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatalf("other contributor: %v", err)
	}
	d, err := e.catalog.PatchAsset(ctx, curator.Context, e.key(), res.AssetID, 2, catalog.AssetPatch{Tags: ptr([]string{"规矩"})})
	if err != nil || d.Revision != 3 || d.Title != "项目规矩" {
		t.Fatalf("curator patch: %+v %v", d, err)
	}
	if _, err := e.catalog.PatchAsset(ctx, curator.Context, e.key(), res.AssetID, 2, catalog.AssetPatch{Tags: ptr([]string{"x"})}); errcode.CodeOf(err) != errcode.PreconditionFailed {
		t.Fatalf("stale revision: %v", err)
	}
	if _, err := e.catalog.PatchAsset(ctx, curator.Context, e.key(), res.AssetID, 3, catalog.AssetPatch{Sensitivity: ptr("personal")}); errcode.CodeOf(err) != errcode.FieldRequiresSpecialCommand {
		t.Fatalf("security field: %v", err)
	}
	info, err := e.catalog.GetAsset(ctx, other.Context, res.AssetID)
	if err != nil || info.Description.Revision != 3 {
		t.Fatalf("read back: %+v %v", info, err)
	}
	raw, _ := json.Marshal(info.Description)
	if !strings.Contains(string(raw), "项目规矩") {
		t.Fatalf("description = %s", raw)
	}
}

// 三个批量会话同时占满批量档时，人本人的交互下载照样立即获准。
func TestInteractiveDownloadIsNotStarvedByBatchTransfers(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{InteractiveSlots: 1, BatchSlots: 3, BatchPerPrincipal: 1})
	ctx := t.Context()
	var holds []*transfer.Ticket
	for i := range 3 {
		_, s := e.agent(fmt.Sprintf("bulk%d@node-a", i), identity.RoleContributor)
		tk, err := e.sched.Admit(ctx, s.Context)
		if err != nil {
			t.Fatal(err)
		}
		holds = append(holds, tk)
	}
	defer func() {
		for _, tk := range holds {
			tk.Release()
		}
	}()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	human := e.login()
	if human.Context.TransferClass() != authz.Interactive {
		t.Fatal("a human's own session must be interactive")
	}
	data := []byte("board")
	_, ms := e.agent("maker@node-z", identity.RoleContributor)
	holds[0].Release() // 让出一个批量名额给制作者上传
	holds = holds[1:]
	u, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: ms.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID,
		Files: []storage.FileSpec{{SHA256: shaOf(data), Size: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	e.putParts(ms.Token, u, data)
	if _, err := e.storage.CompleteFile(ctx, ms.Context, u.UploadID, shaOf(data)); err != nil {
		t.Fatal(err)
	}
	tk, err := e.sched.Admit(ctx, ms.Context) // 重新占满
	if err != nil {
		t.Fatal(err)
	}
	holds = append(holds, tk)
	res, err := e.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: ms.Context, IdempotencyKey: e.key(), UploadID: u.UploadID,
		Slug: "boards/one", Content: catalog.ContentInput{AssetType: manifest.TypeImage, Rights: rightsOwned(),
			Files: []manifest.InputFile{{Path: "board.png", Role: "primary", SHA256: shaOf(data), Size: 5}}}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: human.Context, AssetID: res.AssetID, VersionID: res.VersionID,
		Path: "board.png", Purpose: authz.PurposeArchiveReview})
	if err != nil {
		t.Fatal(err)
	}
	if st := e.sched.Stats(); st.ActiveBatch != 3 {
		t.Fatalf("batch pool should be full: %+v", st)
	}
	if r := e.http("GET", g.URL, human.Token, nil, nil, 0); r.status != 200 || !bytes.Equal(r.body, data) {
		t.Fatalf("interactive download while batch is saturated: %d", r.status)
	}
}

func ptr[T any](v T) *T { return &v }
