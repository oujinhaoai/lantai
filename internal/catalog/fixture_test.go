package catalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/authz/authztest"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/commit/committest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/contract/rights/rightstest"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

type staticReads struct {
	az    authz.Authorizer
	coord *commands.Coordinator
}

func (r staticReads) BeginRead(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource,
	open func(ctx context.Context, d authz.Decision) error) error {
	lctx, h, err := r.coord.Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return err
	}
	defer h.Release()
	d, err := r.az.Authorize(lctx, who, action, res)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return d.Err()
	}
	return open(lctx, d)
}

type fixture struct {
	t       *testing.T
	inst    *operations.Instance
	clk     *clock.Fake
	az      *authztest.Static
	rights  *rightstest.Static
	ledger  *committest.Memory
	storage *storage.Service
	svc     *Service
	faults  *fileop.Faults
	admin   authz.Context
	project commit.Project
	// agent 在项目内是制作者（上传、建资产、提交、改自己的著录、读取）。
	agent authz.Context
	keyN  int
}

var agentActions = []authz.Action{storage.ActionUpload, storage.ActionReadContent, ActionRead, ActionCreateAsset,
	commit.ActionCommitVersion, ActionPatchOwnMetadata}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC))
	gen := &ids.Generator{Clock: clk, Rand: rand.Reader}
	inst, err := operations.Create(ctx, operations.CreateOptions{Options: operations.Options{Home: home, Clock: clk, IDs: gen}, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.Close(context.Background()) })
	if err := inst.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, inst: inst, clk: clk, faults: &fileop.Faults{}}
	f.az = authztest.New(clk, inst.InstanceID())
	f.rights = rightstest.New()
	f.ledger = committest.New(clk, f.az, nil)
	f.storage, err = storage.New(storage.Deps{
		Runtime: inst.DB(ownership.Runtime), Home: inst.Layout().Home, Gate: inst.Gate(), Clock: clk, IDs: inst.IDs(),
		Authz: f.az, Reads: staticReads{az: f.az, coord: inst.Gate().Coordinator()}, Ledger: f.ledger, Rights: f.rights,
		ReadGrantKey: bytes.Repeat([]byte{9}, 32), InstanceID: inst.InstanceID(), FS: fileop.FS{Faults: f.faults},
	}, storage.Config{PartSize: 1 << 20, SinglePartMax: 1 << 20, MinFreeBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	f.ledger.SetInstaller(f.storage)
	f.svc, err = New(Deps{Home: inst.Layout().Home, Gate: inst.Gate(), Storage: f.storage, Ledger: f.ledger, Authz: f.az,
		Rights: f.rights, Clock: clk, IDs: inst.IDs(), InstanceID: inst.InstanceID(), FS: fileop.FS{Faults: f.faults}})
	if err != nil {
		t.Fatal(err)
	}
	f.ledger.SetRevisionVerifier(f.svc)
	f.ledger.SetGate(inst.Gate())
	f.ledger.SetAcceptanceVerifier(f.svc)
	admin := ids.New()
	f.az.AddPrincipal(admin, authz.Human)
	f.az.Grant(admin, "", ActionCreateProject)
	f.admin = f.az.OpenSession(admin, 24*time.Hour)
	preq := ProjectRequest{Who: f.admin, IdempotencyKey: "create-pansi", Key: "pansi", Name: "盘丝洞", ProjectType: "production"}
	p, err := f.svc.CreateProject(ctx, preq)
	if err != nil {
		t.Fatal(err)
	}
	// 授权桩没有“系统管理员在任何项目都可修改说明”的规则：先登记项目、授予
	// 项目内权限后，以同一幂等键重试补完第一个说明修订。
	if !p.DescriptionPending {
		t.Fatalf("the stub admin had no project permission yet, description should be pending: %+v", p)
	}
	f.az.Grant(admin, p.ProjectID, ActionRead, ActionPatchProject, ActionPatchMetadata, storage.ActionReadContent)
	if p, err = f.svc.CreateProject(ctx, preq); err != nil || p.DescriptionPending || p.Description.Revision != 1 {
		t.Fatalf("retry to complete the description: %+v %v", p, err)
	}
	f.project = p.Project
	f.agent = f.newAgent(p.ProjectID)
	return f
}

func (f *fixture) newAgent(project ids.ID) authz.Context {
	p := ids.New()
	f.az.AddPrincipal(p, authz.Agent)
	f.az.Grant(p, project, agentActions...)
	return f.az.OpenSession(p, 24*time.Hour)
}

func (f *fixture) key() string {
	f.keyN++
	return fmt.Sprintf("k-%d", f.keyN)
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// upload 创建上传会话并上传、核验全部内容。
func (f *fixture) upload(who authz.Context, project ids.ID, contents ...[]byte) storage.Upload {
	f.t.Helper()
	var specs []storage.FileSpec
	for _, c := range contents {
		specs = append(specs, storage.FileSpec{SHA256: shaOf(c), Size: int64(len(c))})
	}
	u, err := f.storage.CreateUpload(f.t.Context(), storage.CreateUploadRequest{Who: who, IdempotencyKey: f.key(), ProjectID: project, Files: specs})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, c := range contents {
		f.putContent(who, u, c)
	}
	return u
}

func (f *fixture) putContent(who authz.Context, u storage.Upload, c []byte) {
	f.t.Helper()
	if _, err := f.storage.PutPart(f.t.Context(), storage.PartRequest{Who: who, UploadID: u.UploadID, SHA256: shaOf(c), PartNumber: 1,
		PartSHA256: shaOf(c), Size: int64(len(c)), Body: bytes.NewReader(c)}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.storage.CompleteFile(f.t.Context(), who, u.UploadID, shaOf(c)); err != nil {
		f.t.Fatal(err)
	}
}

// content 描述一个版本：路径 → 内容；第一个文件（按路径排序后）为 primary。
func contentOf(t manifest.AssetType, files map[string][]byte) ContentInput {
	c := ContentInput{AssetType: t, Rights: &manifest.Rights{Usage: "production", License: "LicenseRef-Owned", Sensitivity: "normal"}}
	for p, b := range files {
		role := "source"
		if filepath.Ext(p) == ".glb" || filepath.Ext(p) == ".mp4" {
			role = "primary"
		}
		c.Files = append(c.Files, manifest.InputFile{Path: p, Role: role, SHA256: shaOf(b), Size: int64(len(b))})
	}
	return c
}

func blobs(files map[string][]byte) [][]byte {
	var out [][]byte
	for _, b := range files {
		out = append(out, b)
	}
	return out
}

// create 上传并新建资产的首版。
func (f *fixture) create(who authz.Context, slug string, t manifest.AssetType, files map[string][]byte) VersionResult {
	f.t.Helper()
	u := f.upload(who, f.project.ProjectID, blobs(files)...)
	res, err := f.svc.CommitVersion(f.t.Context(), VersionRequest{Who: who, IdempotencyKey: f.key(), UploadID: u.UploadID,
		Slug: slug, Content: contentOf(t, files)})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

// appendVersion 上传并追加一个版本。
func (f *fixture) appendVersion(who authz.Context, asset, base ids.ID, t manifest.AssetType, files map[string][]byte) VersionResult {
	f.t.Helper()
	u := f.upload(who, f.project.ProjectID, blobs(files)...)
	res, err := f.svc.CommitVersion(f.t.Context(), VersionRequest{Who: who, IdempotencyKey: f.key(), UploadID: u.UploadID,
		AssetID: asset, BaseVersionID: base, Content: contentOf(t, files)})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func wantCode(t *testing.T, err error, code errcode.Code) *errcode.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got success", code)
	}
	e, ok := errcode.As(err)
	if !ok || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return e
}

func wantReason(t *testing.T, err error, code errcode.Code, reason string) {
	t.Helper()
	e := wantCode(t, err, code)
	for _, d := range e.Details {
		if d.Reason == reason {
			return
		}
	}
	t.Fatalf("want reason %s in %+v", reason, e.Details)
}
