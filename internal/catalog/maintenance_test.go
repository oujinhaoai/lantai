package catalog

import (
	"context"
	"os"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/storage"
)

func TestCheckFilesIsReadOnlyAndRepairsOnlyCommittedSnapshots(t *testing.T) {
	f := newFixture(t)
	v := f.create(f.agent, "fsck/asset", manifest.TypeDoc, map[string][]byte{"a.md": []byte("a")})
	projectTarget := commit.MetadataTarget{Kind: commit.TargetProject, ProjectID: f.project.ProjectID, ID: f.project.ProjectID}
	assetTarget := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: v.ProjectID, ID: v.AssetID}
	project, _ := f.ledger.CurrentMetadata(t.Context(), projectTarget)
	asset, _ := f.ledger.CurrentMetadata(t.Context(), assetTarget)
	metadata := []commit.CommittedMetadata{project, asset}
	before, err := f.svc.CheckFiles(t.Context(), metadata)
	if err != nil || len(before.Findings) != 0 || len(before.Files) != 2 {
		t.Fatalf("initial fsck: %+v %v", before, err)
	}
	if err = os.Remove(f.svc.snapshotPath(projectTarget)); err != nil {
		t.Fatal(err)
	}
	bad := []byte("stale snapshot")
	if err = os.WriteFile(f.svc.snapshotPath(assetTarget), bad, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := f.svc.CheckFiles(t.Context(), metadata)
	if err != nil || len(report.Findings) != 2 {
		t.Fatalf("snapshot findings: %+v %v", report, err)
	}
	if _, err = os.Stat(f.svc.snapshotPath(projectTarget)); !os.IsNotExist(err) {
		t.Fatal("read-only check repaired missing snapshot")
	}
	if _, err = f.svc.RepairSnapshots(t.Context(), metadata); err == nil {
		t.Fatal("repaired without maintenance")
	}
	lctx, h, err := f.inst.Gate().Maintain(t.Context(), commands.ReasonRecovering)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { h.Release(); f.inst.Gate().Open() }()
	repaired, err := f.svc.RepairSnapshots(lctx, metadata)
	if err != nil || repaired.Written != 2 {
		t.Fatalf("repair: %+v %v", repaired, err)
	}
	path := f.svc.revisionPath(asset.Target, asset.Revision, asset.OperationID)
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("corrupt immutable history"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.RepairSnapshots(lctx, metadata); errcode.CodeOf(err) != errcode.HashMismatch {
		t.Fatalf("repaired history from snapshot: %v", err)
	}
}

type producerCheck func(context.Context, storage.Producer) error

func (f producerCheck) VerifyProducer(ctx context.Context, p storage.Producer) error {
	return f(ctx, p)
}

func TestFrozenProducerDigestAndFinalRegistrationCheck(t *testing.T) {
	f := newFixture(t)
	files := map[string][]byte{"a.md": []byte("generated synthetic content")}
	u := f.upload(f.agent, f.project.ProjectID, blobs(files)...)
	c := contentOf(manifest.TypeDoc, files)
	producer := storage.Producer{ExtensionID: "org.example.synthetic", ExtensionVersion: "1.0.0", PackageDigest: digest.Of([]byte("package")), Source: "builtin_release", ContributionID: "org.example.synthetic.validate", CoreReleaseDigest: digest.Of([]byte("release"))}
	c.Producer = &producer
	seen := false
	f.svc.SetProducers(producerCheck(func(_ context.Context, p storage.Producer) error {
		seen = true
		if p != producer {
			t.Fatal("producer changed before final acceptance")
		}
		return errcode.New(errcode.Forbidden, "registration disabled")
	}))
	req := VersionRequest{Who: f.agent, IdempotencyKey: "producer-check", UploadID: u.UploadID, Slug: "generated/example", Content: c}
	_, err := f.svc.CommitVersion(t.Context(), req)
	if errcode.CodeOf(err) != errcode.Forbidden || !seen {
		t.Fatalf("unverified producer committed: %v seen=%v", err, seen)
	}
	f.svc.SetProducers(producerCheck(func(context.Context, storage.Producer) error { return nil }))
	result, err := f.svc.CommitVersion(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.svc.GetVersion(t.Context(), f.agent, result.AssetID, result.VersionID)
	if err != nil || info.Manifest.Content.Producer == nil || *info.Manifest.Content.Producer != producer {
		t.Fatalf("producer not frozen: %+v %v", info, err)
	}
	changed := *info.Manifest.Content.Producer
	changed.CoreReleaseDigest = digest.Of([]byte("new release"))
	content := info.Manifest.Content
	content.Producer = &changed
	md, err := content.Digest()
	if err != nil || md == result.ManifestDigest {
		t.Fatalf("producer not covered by digest: %s %v", md, err)
	}
}
