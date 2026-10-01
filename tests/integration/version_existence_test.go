package integration

import (
	"bytes"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// 真实身份下，可见资产配无权项目中的版本与配不存在的版本，在版本详情、
// 读取授权、文件清单、执行读取、永久引用、URI 解析与 uses 解析中得到完全
// 相同的错误，不泄露版本是否存在（BUG-20260930-04）。
func TestForeignVersionUnderVisibleAssetIsIndistinguishableFromAbsent(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	open := e.project
	_, session := e.agent("probe@node-a", identity.RoleContributor)
	who := session.Context
	visible := e.ingest(who, "probe/visible", []byte("visible synthetic content"), *rightsOwned())

	hidden, err := e.catalog.CreateProject(ctx, catalog.ProjectRequest{Who: e.admin.Context, IdempotencyKey: e.key(), Key: "hidden", Name: "Hidden"})
	if err != nil {
		t.Fatal(err)
	}
	e.project = hidden
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	secret := e.ingest(e.login().Context, "secret/doc", []byte("restricted synthetic content"), *rightsOwned())
	e.project = open

	ref := func(version ids.ID) ids.PermanentRef {
		return ids.PermanentRef{InstanceID: e.inst.InstanceID(), AssetID: visible.AssetID, VersionID: version}
	}
	commitWithUse := func(version ids.ID) error {
		data := []byte("derived synthetic content " + string(version))
		u, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: who, IdempotencyKey: e.key(), ProjectID: open.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = e.storage.PutPart(ctx, storage.PartRequest{Who: who, UploadID: u.UploadID, SHA256: shaOf(data), PartNumber: 1, PartSHA256: shaOf(data), Size: int64(len(data)), Body: bytes.NewReader(data)}); err != nil {
			t.Fatal(err)
		}
		if _, err = e.storage.CompleteFile(ctx, who, u.UploadID, shaOf(data)); err != nil {
			t.Fatal(err)
		}
		_, err = e.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: u.UploadID, Slug: "probe/derived-" + string(version)[20:],
			Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Uses: []catalog.DeclaredUse{{AssetID: visible.AssetID, VersionID: version, Relation: "derived_from"}},
				Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}})
		return err
	}
	probes := []struct {
		name string
		call func(version ids.ID) error
	}{
		{"version detail", func(v ids.ID) error { _, err := e.catalog.GetVersion(ctx, who, visible.AssetID, v); return err }},
		{"execution version", func(v ids.ID) error { _, err := e.catalog.ExecutionVersion(ctx, who, visible.AssetID, v); return err }},
		{"read grant", func(v ids.ID) error {
			_, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: who, AssetID: visible.AssetID, VersionID: v, Path: "content.txt", Purpose: authz.Purpose("production")})
			return err
		}},
		{"version files", func(v ids.ID) error { _, err := e.storage.VersionFiles(ctx, who, visible.AssetID, v); return err }},
		{"permanent ref", func(v ids.ID) error { _, err := e.catalog.ResolvePermanent(ctx, who, ref(v)); return err }},
		{"uri", func(v ids.ID) error {
			uri, err := ref(v).URI()
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.catalog.Resolve(ctx, who, uri, 0)
			return err
		}},
		{"uses", commitWithUse},
	}
	absent := ids.New()
	for _, p := range probes {
		foreign, missing := p.call(secret.VersionID), p.call(absent)
		if errcode.CodeOf(missing) != errcode.NotFound || foreign == nil || foreign.Error() != missing.Error() {
			t.Errorf("%s: foreign version %v, absent version %v", p.name, foreign, missing)
		}
	}
	// 对照：同一资产下自己的版本照常可读。
	if _, err := e.catalog.GetVersion(ctx, who, visible.AssetID, visible.VersionID); err != nil {
		t.Fatal(err)
	}
}

// 流程输入引用与任务一致：先按版本实际所属项目授权，再比较归属；无权读取
// 与不存在不可区分，能读取的版本挂错资产才报告 REF_MISMATCH。
func TestFlowInputRefToUnreadableVersionIsIndistinguishableFromAbsent(t *testing.T) {
	f := newFlowEnv(t)
	ctx := t.Context()
	open := f.project
	visible := f.config(f.owner, "probe/visible", map[string]any{"probe": "visible"})
	hidden, err := f.catalog.CreateProject(ctx, catalog.ProjectRequest{Who: f.admin.Context, IdempotencyKey: f.key(), Key: "hidden", Name: "Hidden"})
	if err != nil {
		t.Fatal(err)
	}
	f.project = hidden
	f.setRole(f.admin.Context.PrincipalID, identity.RoleOwner, true)
	secret := f.ingest(f.login().Context, "secret/doc", []byte("restricted synthetic content"), *rightsOwned())
	f.project = open
	start := func(version ids.ID) error {
		ref := visible.Ref
		ref.VersionID = version
		_, err := f.flows.Start(ctx, f.maker, f.key(), workflow.StartRequest{ProjectID: open.ProjectID, DefinitionRef: f.definition.Ref, ProfileRef: f.profile.Ref, Title: "probe",
			AcceptanceCriteria: []string{"probe"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "probe", AssetType: "doc", CandidateCount: 1}}, InputRefs: []ids.PermanentRef{ref}})
		return err
	}
	foreign, missing := start(secret.VersionID), start(ids.New())
	if errcode.CodeOf(missing) != errcode.NotFound || foreign == nil || foreign.Error() != missing.Error() {
		t.Fatalf("foreign version %v, absent version %v", foreign, missing)
	}
	if err := start(f.profile.VersionID); errcode.CodeOf(err) != errcode.RefMismatch {
		t.Fatalf("readable version under the wrong asset: %v", err)
	}
}
