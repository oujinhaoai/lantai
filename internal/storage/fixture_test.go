package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/authz/authztest"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/commit/committest"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/contract/rights/rightstest"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// staticReads 用授权桩实现最终读取检查：与 identity.BeginRead 相同，在
// security_guard 读锁内按当前状态授权后打开。
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
	svc     *Service
	clk     *clock.Fake
	az      *authztest.Static
	ledger  *committest.Memory
	rights  *rightstest.Static
	project ids.ID
	// who 是项目内有上传、读取与提交权限的 Agent 会话。
	who  authz.Context
	keyN int
	cfg  Config
	fs   fileop.FS
}

// testConfig 用较小的分片，便于在单元测试中覆盖多分片与续传。
func testConfig() Config {
	return Config{PartSize: 64 << 10, SinglePartMax: 64 << 10, MinFreeBytes: 1 << 20}
}

func newFixture(t *testing.T, cfg Config, faults *fileop.Faults) *fixture {
	t.Helper()
	ctx := t.Context()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC))
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
	f := &fixture{t: t, inst: inst, clk: clk, cfg: cfg, fs: fileop.FS{Faults: faults}}
	f.az = authztest.New(clk, inst.InstanceID())
	f.rights = rightstest.New()
	f.svc = f.newService()
	f.ledger = committest.New(clk, f.az, f.svc)
	f.svc.ledger = f.ledger
	f.project = ids.New()
	f.who = f.agent(f.project)
	return f
}

// newService 在同一实例上创建新的服务对象，模拟进程重启后的状态只来自持久记录。
func (f *fixture) newService() *Service {
	f.t.Helper()
	var ledger commit.Reader = placeholderReader{}
	if f.ledger != nil {
		ledger = f.ledger
	}
	svc, err := New(Deps{
		Runtime: f.inst.DB(ownership.Runtime), Home: f.inst.Layout().Home, Gate: f.inst.Gate(), Clock: f.clk,
		IDs: f.inst.IDs(), Authz: f.az, Reads: staticReads{az: f.az, coord: f.inst.Gate().Coordinator()},
		Ledger: ledger, Rights: f.rights, ReadGrantKey: bytes.Repeat([]byte{7}, 32), InstanceID: f.inst.InstanceID(), FS: f.fs,
	}, f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return svc
}

// restart 以新的服务对象替换当前服务（内存状态丢失，持久记录保留）。
func (f *fixture) restart() {
	f.svc = f.newService()
	f.svc.ledger = f.ledger
}

type placeholderReader struct{ commit.Reader }

// agent 登记一个在项目内可上传、读取与提交版本的 Agent，并开启会话。
func (f *fixture) agent(project ids.ID) authz.Context {
	p := ids.New()
	f.az.AddPrincipal(p, authz.Agent)
	f.az.Grant(p, project, ActionUpload, ActionReadContent, commit.ActionCommitVersion)
	return f.az.OpenSession(p, 12*time.Hour)
}

func (f *fixture) key() string {
	f.keyN++
	return fmt.Sprintf("k-%d", f.keyN)
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func specOf(b []byte) FileSpec { return FileSpec{SHA256: shaOf(b), Size: int64(len(b))} }

// synthetic 生成确定性的合成内容。
func synthetic(seed string, n int) []byte {
	out := make([]byte, 0, n)
	block := sha256.Sum256([]byte(seed))
	for len(out) < n {
		out = append(out, block[:]...)
		block = sha256.Sum256(block[:])
	}
	return out[:n]
}

func (f *fixture) createUpload(who authz.Context, project ids.ID, contents ...[]byte) Upload {
	f.t.Helper()
	var specs []FileSpec
	for _, c := range contents {
		specs = append(specs, specOf(c))
	}
	u, err := f.svc.CreateUpload(f.t.Context(), CreateUploadRequest{Who: who, IdempotencyKey: f.key(), ProjectID: project, Files: specs})
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// putAll 按布局写入内容的全部分片。
func (f *fixture) putAll(who authz.Context, u Upload, content []byte) {
	f.t.Helper()
	sha := shaOf(content)
	for _, uf := range u.Files {
		if uf.SHA256 != sha {
			continue
		}
		for n := 1; n <= uf.PartCount; n++ {
			f.putPart(who, u.UploadID, content, uf, n)
		}
		return
	}
	f.t.Fatalf("content %s is not part of upload %s", sha, u.UploadID)
}

func partBytes(content []byte, uf UploadFile, n int) []byte {
	start := int64(n-1) * uf.PartSize
	end := min(start+uf.PartSize, int64(len(content)))
	return content[start:end]
}

func (f *fixture) putPart(who authz.Context, upload ids.ID, content []byte, uf UploadFile, n int) PartResult {
	f.t.Helper()
	b := partBytes(content, uf, n)
	res, err := f.svc.PutPart(f.t.Context(), PartRequest{Who: who, UploadID: upload, SHA256: uf.SHA256, PartNumber: n,
		PartSHA256: shaOf(b), Size: int64(len(b)), Body: bytes.NewReader(b)})
	if err != nil {
		f.t.Fatalf("part %d: %v", n, err)
	}
	return res
}

// uploadAll 创建会话并上传、核验全部内容。
func (f *fixture) uploadAll(who authz.Context, project ids.ID, contents ...[]byte) Upload {
	f.t.Helper()
	u := f.createUpload(who, project, contents...)
	for _, c := range contents {
		f.putAll(who, u, c)
		if _, err := f.svc.CompleteFile(f.t.Context(), who, u.UploadID, shaOf(c)); err != nil {
			f.t.Fatal(err)
		}
	}
	return u
}

// files 把路径到内容的映射转为按路径排序的清单文件条目。
func files(m map[string][]byte) []install.File {
	var out []install.File
	for p, c := range m {
		out = append(out, install.File{Path: p, SHA256: shaOf(c), Size: int64(len(c))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func testManifest(fs []install.File) []byte {
	b, _ := json.Marshal(map[string]any{"contract": "synthetic-test-manifest", "files": fs})
	return b
}

// commitVersion 走完整的提交路径：上传 → Prepare（使用上传会话的操作）→
// Install → Commit。asset 为空时新建资产。
func (f *fixture) commitVersion(who authz.Context, project, asset ids.ID, slug string, base ids.ID, content map[string][]byte) commit.Committed {
	f.t.Helper()
	fs := files(content)
	var blobs [][]byte
	for _, c := range content {
		blobs = append(blobs, c)
	}
	u := f.uploadAll(who, project, blobs...)
	p, proof := f.prepareInstall(who, u, project, asset, slug, base, fs)
	c, err := f.ledger.Commit(f.t.Context(), p.OperationID, who, proof)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) prepareInstall(who authz.Context, u Upload, project, asset ids.ID, slug string, base ids.ID, fs []install.File) (commit.Prepared, install.Proof) {
	f.t.Helper()
	md := committest.ManifestDigest(fs)
	req := commit.PrepareRequest{Who: who, ProjectID: project, AssetID: asset, Slug: slug, BaseVersionID: base, ManifestDigest: md, Files: fs}
	body, _ := json.Marshal(map[string]any{"slug": slug, "asset": asset, "base": base, "files": fs})
	hash, err := commands.RequestHash(commands.HashInput{CommandType: commit.CommandType, ProjectID: project, Body: body})
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := commands.Context{OperationID: u.OperationID, IdempotencyKey: f.key(), RequestHash: hash, CommandType: commit.CommandType,
		ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: project, RecoveryEpoch: who.RecoveryEpoch}
	p, err := f.ledger.Prepare(f.t.Context(), cmd, req)
	if err != nil {
		f.t.Fatal(err)
	}
	ireq := p.InstallRequest()
	ireq.Manifest = testManifest(fs)
	proof, err := f.svc.Install(f.t.Context(), ireq)
	if err != nil {
		f.t.Fatal(err)
	}
	return p, proof
}

// grantForOperation 是安装契约套件的测试构造器：经正常上传把内容放入内容库，
// 再为套件给出的操作登记一条 uploaded 授权（真实流程中操作由上传会话分配）。
func (f *fixture) grantForOperation(op, project ids.ID, content []byte) {
	f.t.Helper()
	f.uploadAll(f.who, f.project, content)
	grantID := ids.New()
	now := clock.Millis(f.clk.Now())
	if _, err := f.inst.DB(ownership.Runtime).Exec(`INSERT INTO storage_blob_grants (grant_id, principal_id, session_id, project_id,
		operation_id, sha256, size, basis, purpose, issued_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'uploaded', 'ingest', ?, ?)`,
		grantID, f.who.PrincipalID, f.who.SessionID, project, op, shaOf(content), len(content), now, now+int64(time.Hour/time.Millisecond)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) manifestDigest(fs []install.File) digest.Digest {
	return committest.ManifestDigest(fs)
}
