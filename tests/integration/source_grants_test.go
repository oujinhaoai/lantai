package integration

import (
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 同哈希不是读取或复用权限；两个真实来源的用途分别判定，并在最终接受时复验。
func TestSameBytesSourceGrantsWithRealIdentity(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	principal, maker := e.agent("sources@node", identity.RoleContributor)
	_, curator := e.agent("sources-curator@node", identity.RoleCurator)
	data := []byte("identical synthetic source bytes")
	normal := e.ingest(maker.Context, "sources/normal", data, *rightsOwned())
	restrictedRights := *rightsOwned()
	restrictedRights.NoAI = true
	restricted := e.ingest(maker.Context, "sources/noai", data, restrictedRights)
	ref := func(asset, version ids.ID) ids.PermanentRef {
		return ids.PermanentRef{InstanceID: e.inst.InstanceID(), AssetID: asset, VersionID: version}
	}
	upload, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: maker.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}}})
	if err != nil {
		t.Fatal(err)
	}
	request := install.Request{OperationID: upload.OperationID, ProjectID: upload.ProjectID, Files: []install.File{{Path: "content.txt", SHA256: shaOf(data), Size: int64(len(data))}}}
	want := func(err error, code errcode.Code) {
		t.Helper()
		if errcode.CodeOf(err) != code {
			t.Fatalf("want %s, got %v", code, err)
		}
	}
	status, err := e.storage.CheckBlobs(ctx, maker.Context, upload.UploadID, []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}})
	if err != nil || len(status) != 1 || status[0].Status != "upload_required" {
		t.Fatalf("hash alone: %+v %v", status, err)
	}
	want(e.storage.VerifyGrantAcceptance(ctx, maker.Context, request, authz.PurposeGenerativeInput), errcode.BlobGrantRequired)
	sourceRequest := storage.SourceGrantRequest{Who: maker.Context, UploadID: upload.UploadID, Source: ref(restricted.AssetID, restricted.VersionID), Path: "content.txt", Purpose: authz.PurposeGenerativeInput}
	_, err = e.storage.GrantFromSource(ctx, sourceRequest)
	want(err, errcode.UseRestricted)
	sourceRequest.Purpose = authz.PurposeReference
	restrictedGrant, err := e.storage.GrantFromSource(ctx, sourceRequest)
	if err != nil {
		t.Fatal(err)
	}
	want(e.storage.VerifyGrantAcceptance(ctx, maker.Context, request, authz.PurposeGenerativeInput), errcode.BlobGrantRequired)
	sourceRequest.Source = ref(normal.AssetID, normal.VersionID)
	sourceRequest.Purpose = authz.PurposeGenerativeInput
	normalGrant, err := e.storage.GrantFromSource(ctx, sourceRequest)
	if err != nil {
		t.Fatal(err)
	}
	if normalGrant.GrantID == restrictedGrant.GrantID || normalGrant.Source == nil || normalGrant.Source.VersionID != normal.VersionID || restrictedGrant.Source == nil || restrictedGrant.Source.VersionID != restricted.VersionID {
		t.Fatal("source/purpose grants conflated")
	}
	if err := e.storage.VerifyGrantAcceptance(ctx, maker.Context, request, authz.PurposeGenerativeInput); err != nil {
		t.Fatal(err)
	}
	// 真实来源模块追加限制，已有授权不能沿用旧快照。
	_, err = e.rights.AppendEvidence(ctx, provenance.AppendRequest{Who: curator.Context, IdempotencyKey: e.key(), Ref: ref(normal.AssetID, normal.VersionID), ManifestDigest: normal.ManifestDigest, Evidence: provenance.Evidence{Note: "synthetic unknown source", ExternalInputs: []provenance.ExternalInput{{RightsStatus: "unknown", SourceURL: "https://example.test/source"}}}})
	if err != nil {
		t.Fatal(err)
	}
	want(e.storage.VerifyGrantAcceptance(ctx, maker.Context, request, authz.PurposeGenerativeInput), errcode.RightsPending)
	if err := e.storage.VerifyGrantAcceptance(ctx, maker.Context, request, authz.PurposeReference); err != nil {
		t.Fatal(err)
	}
	e.setRole(principal.ID, identity.RoleContributor, false)
	want(e.storage.VerifyGrantAcceptance(ctx, maker.Context, request, authz.PurposeReference), errcode.NotFound)
}
