package catalog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/storage"
)

func TestRetryKeepsFrozenUseAfterNameRelease(t *testing.T) {
	f := newFixture(t)
	source := f.create(f.agent, "inputs/board", manifest.TypeImage, map[string][]byte{"board.png": []byte("source")})
	files := map[string][]byte{"shot.mp4": []byte("shot")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(files)...)
	c := contentOf(manifest.TypeProduction, files)
	c.Uses = []DeclaredUse{{Ref: "pansi/inputs/board@latest", Relation: "uses"}}
	req := VersionRequest{Who: f.agent, IdempotencyKey: "retry-released-source", UploadID: u.UploadID, Slug: "shots/one", Content: c}
	f.faults.Rename = func(string, string) error { return syscall.EBUSY }
	_, err := f.svc.CommitVersion(t.Context(), req)
	wantCode(t, err, errcode.StorageUnavailable)
	f.faults.Rename = nil
	if err := f.ledger.ReleaseName(source.ProjectID, source.Slug); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Resolve(t.Context(), f.agent, c.Uses[0].Ref, 0)
	wantCode(t, err, errcode.NotFound)
	res, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.svc.GetVersion(t.Context(), f.agent, res.AssetID, res.VersionID)
	if err != nil || len(info.Manifest.Content.Uses) != 1 || info.Manifest.Content.Uses[0].VersionID != source.VersionID {
		t.Fatalf("frozen input: %+v %v", info, err)
	}
	// 提交后 staging 已清理，响应重放仍不重新解析已释放的浮动引用。
	replay, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil || replay.VersionID != res.VersionID {
		t.Fatalf("committed replay: %+v %v", replay, err)
	}
}

func TestRetryRevalidatesFrozenUseRatherThanCurrentAlias(t *testing.T) {
	f := newFixture(t)
	source := f.create(f.agent, "inputs/restricted", manifest.TypeImage, map[string][]byte{"board.png": []byte("first")})
	files := map[string][]byte{"shot.mp4": []byte("shot")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(files)...)
	c := contentOf(manifest.TypeProduction, files)
	c.Uses = []DeclaredUse{{Ref: "pansi/inputs/restricted@latest", Relation: "uses"}}
	req := VersionRequest{Who: f.agent, IdempotencyKey: "retry-restricted-source", UploadID: u.UploadID, Slug: "shots/restricted", Content: c}
	f.faults.Rename = func(string, string) error { return syscall.EBUSY }
	_, err := f.svc.CommitVersion(t.Context(), req)
	wantCode(t, err, errcode.StorageUnavailable)
	f.faults.Rename = nil
	f.appendVersion(f.agent, source.AssetID, source.VersionID, manifest.TypeImage, map[string][]byte{"board.png": []byte("latest")})
	f.rights.Restrict(source.VersionID, authz.PurposeProduction)
	_, err = f.svc.CommitVersion(t.Context(), req)
	wantCode(t, err, errcode.UseRestricted)
	view, err := f.ledger.Operation(t.Context(), u.OperationID)
	if err != nil || view.Stage != commands.StageBlocked {
		t.Fatalf("restricted frozen input must block the operation: %+v %v", view, err)
	}
}

func TestSourceGrantPurposeMustMatchFrozenContent(t *testing.T) {
	f := newFixture(t)
	files := map[string][]byte{"reference.png": []byte("reference-only-source")}
	source := f.create(f.agent, "inputs/reference-only", manifest.TypeImage, files)
	f.rights.Restrict(source.VersionID, authz.PurposeProduction)
	for _, usage := range []string{"production", "reference"} {
		t.Run(usage, func(t *testing.T) {
			// 仅通过来源授权复用；不重新上传，也故意不声明 uses。
			u, err := f.storage.CreateUpload(t.Context(), storage.CreateUploadRequest{
				Who: f.agent, IdempotencyKey: "source-purpose-upload-" + usage, ProjectID: f.project.ProjectID,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.storage.GrantFromSource(t.Context(), storage.SourceGrantRequest{
				Who: f.agent, UploadID: u.UploadID, Source: source.Ref, Path: "reference.png", Purpose: authz.PurposeReference,
			})
			if err != nil {
				t.Fatal(err)
			}
			content := contentOf(manifest.TypeImage, files)
			content.Rights.Usage = usage
			res, err := f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "source-purpose-commit-" + usage,
				UploadID: u.UploadID, Slug: "copies/" + usage, Content: content})
			if usage == "production" {
				wantCode(t, err, errcode.BlobGrantRequired)
				view, readErr := f.ledger.Operation(t.Context(), u.OperationID)
				if readErr != nil || view.Stage != commands.StageBlocked {
					t.Fatalf("reference grant must not authorize production: %+v %v", view, readErr)
				}
				return
			}
			if err != nil || res.VersionNumber != 1 {
				t.Fatalf("reference grant should authorize a reference-only version: %+v %v", res, err)
			}
		})
	}
}

func TestCommittedReplayDoesNotRecomputeInheritedRights(t *testing.T) {
	f := newFixture(t)
	first := f.create(f.agent, "docs/inherited", manifest.TypeDoc, map[string][]byte{"a.md": []byte("first")})
	files := map[string][]byte{"a.md": []byte("second")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(files)...)
	c := contentOf(manifest.TypeDoc, files)
	c.Rights = nil
	req := VersionRequest{Who: f.agent, IdempotencyKey: "inherited-replay", UploadID: u.UploadID, AssetID: first.AssetID, BaseVersionID: first.VersionID, Content: c}
	second, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	newFiles := map[string][]byte{"a.md": []byte("third")}
	newUpload := f.upload(f.agent, f.project.ProjectID, blobs(newFiles)...)
	newContent := contentOf(manifest.TypeDoc, newFiles)
	newContent.Rights.NoAI = true
	_, err = f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "new-rights", UploadID: newUpload.UploadID, AssetID: first.AssetID, BaseVersionID: second.VersionID, Content: newContent})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil || replay.VersionID != second.VersionID {
		t.Fatalf("inherited rights changed the request digest on replay: %+v %v", replay, err)
	}
	// 同键换了上传操作是另一份请求，不能把旧成功当成新操作的成功。
	req.UploadID = newUpload.UploadID
	_, err = f.svc.CommitVersion(t.Context(), req)
	wantCode(t, err, errcode.IdempotencyConflict)
}

// 模拟安装成功后、台账接受前的暂时失败。
type interruptedCommit struct {
	Ledger
	interrupt bool
}

func (l *interruptedCommit) Commit(ctx context.Context, op ids.ID, who authz.Context, proof install.Proof) (commit.Committed, error) {
	if l.interrupt {
		l.interrupt = false
		return commit.Committed{}, errcode.New(errcode.StorageUnavailable, "injected interruption before acceptance")
	}
	return l.Ledger.Commit(ctx, op, who, proof)
}

func TestInstalledRetryFromNewSessionPreservesManifest(t *testing.T) {
	f := newFixture(t)
	files := map[string][]byte{"a.md": []byte("content")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(files)...)
	req := VersionRequest{Who: f.agent, IdempotencyKey: "new-session-retry", UploadID: u.UploadID, Slug: "docs/retry", Content: contentOf(manifest.TypeDoc, files)}
	f.svc.ledger = &interruptedCommit{Ledger: f.ledger, interrupt: true}
	_, err := f.svc.CommitVersion(t.Context(), req)
	wantCode(t, err, errcode.StorageUnavailable)
	req.Who = f.az.OpenSession(f.agent.PrincipalID, 24*time.Hour)
	res, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.svc.GetVersion(t.Context(), req.Who, res.AssetID, res.VersionID)
	if err != nil || info.Manifest.SessionID != f.agent.SessionID {
		t.Fatalf("installed manifest must retain its original accepting session: %+v %v", info, err)
	}
}

func TestInvalidDescriptionReservesNothing(t *testing.T) {
	f := newFixture(t)
	files := map[string][]byte{"a.md": []byte("content")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(files)...)
	for i, patch := range []AssetPatch{
		{Sensitivity: ptr("personal")},
		{Title: ptr(" ")},
		{Extra: ptr(map[string]any{"unsupported": make(chan int)})},
	} {
		req := VersionRequest{Who: f.agent, IdempotencyKey: "invalid-description", UploadID: u.UploadID, Slug: "docs/invalid", Content: contentOf(manifest.TypeDoc, files), Describe: &patch}
		if _, err := f.svc.CommitVersion(t.Context(), req); err == nil {
			t.Fatalf("invalid description %d was accepted", i)
		}
		if _, err := f.ledger.Operation(t.Context(), u.OperationID); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("invalid description %d reserved a version: %v", i, err)
		}
	}
}

func TestAliasHistoryCannotRedirectToAnotherAsset(t *testing.T) {
	f := newFixture(t)
	old := f.create(f.agent, "docs/reused", manifest.TypeDoc, map[string][]byte{"old.md": []byte("old")})
	other := f.create(f.agent, "docs/other", manifest.TypeDoc, map[string][]byte{"other.md": []byte("other")})
	if err := f.ledger.ReleaseName(old.ProjectID, old.Slug); err != nil {
		t.Fatal(err)
	}
	f.create(f.agent, old.Slug, manifest.TypeDoc, map[string][]byte{"new.md": []byte("new")})
	oldPath := f.svc.aliasPath(old.ProjectID, old.Slug, 1)
	otherRaw, err := os.ReadFile(f.svc.aliasPath(other.ProjectID, other.Slug, 1))
	if err != nil {
		t.Fatal(err)
	}
	var rec AliasRecord
	if err := json.Unmarshal(otherRaw, &rec); err != nil {
		t.Fatal(err)
	}
	rec.NormalizedSlug = old.Slug
	raw, _ := json.Marshal(rec)
	if err := os.Chmod(oldPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Resolve(t.Context(), f.agent, "pansi/docs/reused@v001", 1)
	wantCode(t, err, errcode.OperationNeedsReconciliation)
	_, err = f.svc.RepairAliasHistory(t.Context())
	wantCode(t, err, errcode.OperationNeedsReconciliation)
}

func TestMetadataReplayRejectsTamperedHistoricalFile(t *testing.T) {
	f := newFixture(t)
	res := f.create(f.agent, "docs/history", manifest.TypeDoc, map[string][]byte{"a.md": []byte("a")})
	patch := AssetPatch{Title: ptr("first title")}
	if _, err := f.svc.PatchAsset(t.Context(), f.agent, "history-title", res.AssetID, 1, patch); err != nil {
		t.Fatal(err)
	}
	target := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: res.ProjectID, ID: res.AssetID}
	original, err := f.ledger.CurrentMetadata(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PatchAsset(t.Context(), f.agent, "history-summary", res.AssetID, 2, AssetPatch{Summary: ptr("later summary")}); err != nil {
		t.Fatal(err)
	}
	path := f.svc.revisionPath(target, original.Revision, original.OperationID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "first title", "forged title", 1))
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.PatchAsset(t.Context(), f.agent, "history-title", res.AssetID, 1, patch)
	wantCode(t, err, errcode.HashMismatch)
}

type changingCurrent struct {
	Ledger
	calls        atomic.Int32
	old, current commit.CommittedMetadata
	newChecked   chan struct{}
}

func (l *changingCurrent) CurrentMetadata(context.Context, commit.MetadataTarget) (commit.CommittedMetadata, error) {
	if l.calls.Add(1) == 1 {
		return l.old, nil
	}
	close(l.newChecked)
	return l.current, nil
}

func TestSnapshotPublicationCannotOverwriteNewerSnapshot(t *testing.T) {
	f := newFixture(t)
	res := f.create(f.agent, "docs/snapshot", manifest.TypeDoc, map[string][]byte{"a.md": []byte("a")})
	target := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: res.ProjectID, ID: res.AssetID}
	old, _ := f.ledger.CurrentMetadata(t.Context(), target)
	if _, err := f.svc.PatchAsset(t.Context(), f.agent, "snapshot-new", res.AssetID, 1, AssetPatch{Title: ptr("new title")}); err != nil {
		t.Fatal(err)
	}
	current, _ := f.ledger.CurrentMetadata(t.Context(), target)
	oldRaw, _ := os.ReadFile(f.svc.revisionPath(target, old.Revision, old.OperationID))
	currentRaw, _ := os.ReadFile(f.svc.revisionPath(target, current.Revision, current.OperationID))
	reader := &changingCurrent{Ledger: f.ledger, old: old, current: current, newChecked: make(chan struct{})}
	f.svc.ledger = reader
	oldPaused, resumeOld := make(chan struct{}), make(chan struct{})
	var renames atomic.Int32
	f.faults.Rename = func(_, next string) error {
		if next == f.svc.snapshotPath(target) && renames.Add(1) == 1 {
			close(oldPaused)
			<-resumeOld
		}
		return nil
	}
	oldDone, newDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(oldDone); f.svc.publishSnapshot(t.Context(), target, old, oldRaw) }()
	<-oldPaused
	go func() { defer close(newDone); f.svc.publishSnapshot(t.Context(), target, current, currentRaw) }()
	premature := false
	select {
	case <-reader.newChecked:
		premature = true
	case <-time.After(100 * time.Millisecond):
	}
	close(resumeOld)
	<-oldDone
	<-newDone
	if premature {
		t.Error("the second publication checked its revision before the first finished")
	}
	raw, err := os.ReadFile(filepath.Join(f.storage.Layout().AssetDir(res.ProjectID, res.AssetID), "asset.yaml"))
	if err != nil || string(raw) != string(currentRaw) {
		t.Fatalf("snapshot reverted to an earlier revision: %v\n%s", err, raw)
	}
}

func TestPermanentURIResolutionAndUses(t *testing.T) {
	f := newFixture(t)
	source := f.create(f.agent, "docs/source", manifest.TypeDoc, map[string][]byte{"a.md": []byte("a")})
	got, err := f.svc.Resolve(t.Context(), f.agent, source.URI, 0)
	if err != nil || got.VersionID != source.VersionID {
		t.Fatalf("permanent URI: %+v %v", got, err)
	}
	_, err = f.svc.Resolve(t.Context(), f.agent, source.URI, 1)
	wantCode(t, err, errcode.SchemaInvalid)
	files := map[string][]byte{"b.md": []byte("b")}
	u := f.upload(f.agent, source.ProjectID, blobs(files)...)
	c := contentOf(manifest.TypeDoc, files)
	c.Uses = []DeclaredUse{{Ref: source.URI, Relation: "reference"}}
	if _, err := f.svc.CommitVersion(t.Context(), VersionRequest{Who: f.agent, IdempotencyKey: "uri-use", UploadID: u.UploadID, Slug: "docs/derived", Content: c}); err != nil {
		t.Fatal(err)
	}
}
