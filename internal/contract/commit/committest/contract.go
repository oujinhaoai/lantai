package committest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/authz/authztest"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/install/installtest"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

// Harness 让契约套件驱动任意台账实现；安装端与修订文件端使用内存桩。
type Harness struct {
	Ledger    commit.Ledger
	Reader    commit.Reader
	Namespace commit.Namespace
	Metadata  commit.Metadata
	Projects  commit.Projects
	Authz     *authztest.Static
	Installer *installtest.Memory
	Revisions *Revisions
	Clock     *clock.Fake
	// NewProject lets a persistent implementation register an actual project
	// before the suite creates its project-scoped actor. Memory fixtures may omit it.
	NewProject func() ids.ID
	// Cancel 可选：放弃尚未提交的操作。
	Cancel func(op ids.ID) error
	// CancelMetadata 可选：放弃尚未生效的说明修订。
	CancelMetadata func(op ids.ID) error
	// Release 可选：释放当前占名（M2 的宽限删除或管理员释放；M1 用测试构造器）。
	Release func(project ids.ID, slug string) error
}

// NewHarness 用内存台账组装一个可直接使用的环境。
func NewHarness(t *testing.T) Harness {
	clk := clock.NewFake(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	az := authztest.New(clk, ids.New())
	in := installtest.New(clk)
	rev := NewRevisions(clk)
	m := New(clk, az, in)
	m.SetRevisionVerifier(rev)
	return Harness{Ledger: m, Reader: m, Namespace: m, Metadata: m, Projects: m, Authz: az, Installer: in,
		Revisions: rev, Clock: clk, Cancel: m.CancelOperation, CancelMetadata: m.CancelMetadata, Release: m.ReleaseName}
}

type actor struct {
	who     authz.Context
	project ids.ID
}

func newActor(h Harness) actor {
	p, project := ids.New(), ids.New()
	if h.NewProject != nil {
		project = h.NewProject()
	}
	h.Authz.AddPrincipal(p, authz.Agent)
	h.Authz.Grant(p, project, commit.ActionCommitVersion, "catalog.read")
	return actor{who: h.Authz.OpenSession(p, time.Hour), project: project}
}

func (a actor) cmd(t *testing.T, key string, req commit.PrepareRequest) commands.Context {
	t.Helper()
	body, _ := json.Marshal(struct {
		AssetID ids.ID         `json:"asset_id,omitempty"`
		Slug    string         `json:"slug,omitempty"`
		Base    ids.ID         `json:"base_version_id,omitempty"`
		Files   []install.File `json:"files"`
	}{req.AssetID, req.Slug, req.BaseVersionID, req.Files})
	hash, err := commands.RequestHash(commands.HashInput{CommandType: commit.CommandType, ProjectID: a.project, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return commands.Context{
		OperationID: ids.New(), IdempotencyKey: key, RequestHash: hash, CommandType: commit.CommandType,
		ActorID: a.who.PrincipalID, SessionID: a.who.SessionID, ProjectID: a.project, RecoveryEpoch: a.who.RecoveryEpoch,
	}
}

func (a actor) newAsset(h Harness, slug string, contents ...string) commit.PrepareRequest {
	var files []install.File
	for i, c := range contents {
		files = append(files, h.Installer.PutBlob(string(rune('a'+i))+".txt", []byte(c)))
	}
	return commit.PrepareRequest{Who: a.who, ProjectID: a.project, Slug: slug, ManifestDigest: ManifestDigest(files), Files: files}
}

func wantCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if got := errcode.CodeOf(err); err == nil || got != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// RunLedgerContract 对台账实现运行提交契约测试；台账的真实实现须通过它。
func RunLedgerContract(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Run("prepare install commit read", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		req := a.newAsset(h, "pansi/ch03/whitebox", "v1")
		p, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "k1", req), req)
		if err != nil {
			t.Fatal(err)
		}
		if p.VersionNumber != 1 {
			t.Fatalf("first version number = %d", p.VersionNumber)
		}
		// installed 之前与之后、committed 之前都不可读。
		wantCode(t, readErr(h, p), errcode.NotFound)
		proof, err := h.Installer.Install(t.Context(), p.InstallRequest())
		if err != nil {
			t.Fatal(err)
		}
		wantCode(t, readErr(h, p), errcode.NotFound)
		c, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof)
		if err != nil {
			t.Fatal(err)
		}
		got, err := h.Reader.Version(t.Context(), p.AssetID, p.VersionID)
		if err != nil || got != c {
			t.Fatalf("read after commit: %+v %v", got, err)
		}
		all, err := h.Reader.Versions(t.Context(), "", 10)
		if err != nil || len(all) != 1 || all[0].VersionID != p.VersionID {
			t.Fatalf("enumerate: %+v %v", all, err)
		}
		v, err := h.Ledger.Operation(t.Context(), p.OperationID)
		if err != nil || (v.Stage != commands.StageCommitted && v.Stage != commands.StageProjected) {
			t.Fatalf("operation: %+v %v", v, err)
		}
		assertView(t, v)
	})

	t.Run("prepare is idempotent per key and rejects a different request", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		req := a.newAsset(h, "asset/one", "v1")
		p1, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "same", req), req)
		if err != nil {
			t.Fatal(err)
		}
		p2, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "same", req), req) // 响应丢失后重试
		if err != nil {
			t.Fatal(err)
		}
		if p1.OperationID != p2.OperationID || p1.VersionID != p2.VersionID || p1.VersionNumber != p2.VersionNumber {
			t.Fatalf("retry allocated new identity: %+v vs %+v", p1, p2)
		}
		other := a.newAsset(h, "asset/two", "different")
		_, err = h.Ledger.Prepare(t.Context(), a.cmd(t, "same", other), other)
		wantCode(t, err, errcode.IdempotencyConflict)
	})

	t.Run("commit is idempotent and bound to its proof", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "asset/x", "v1")
		c1, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof)
		if err != nil {
			t.Fatal(err)
		}
		c2, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof) // 响应丢失后重试
		if err != nil || c1 != c2 {
			t.Fatalf("commit replay: %+v %v", c2, err)
		}
		forged := proof
		forged.InstallRef = "install/other"
		_, err = h.Ledger.Commit(t.Context(), p.OperationID, a.who, forged)
		wantCode(t, err, errcode.OperationNeedsReconciliation)
		all, _ := h.Reader.Versions(t.Context(), "", 10)
		if len(all) != 1 {
			t.Fatalf("versions = %d, want exactly one", len(all))
		}
	})

	t.Run("revocation before commit blocks without exposing the version", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "asset/revoked", "v1")
		h.Authz.Revoke(a.who.PrincipalID, a.project)
		_, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof)
		if err == nil {
			t.Fatal("commit accepted after revocation")
		}
		wantCode(t, readErr(h, p), errcode.NotFound)
		v, _ := h.Ledger.Operation(t.Context(), p.OperationID)
		if v.Stage != commands.StageBlocked {
			t.Fatalf("stage after revocation = %s, want blocked", v.Stage)
		}
		if _, q := h.Installer.Quarantined(p.OperationID); q {
			t.Fatal("blocked operation must keep its installed bytes, not quarantine them")
		}
	})

	t.Run("old recovery epoch cannot commit", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "asset/restored", "v1")
		h.Authz.Restore()
		_, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof)
		wantCode(t, err, errcode.TokenRevoked)
		wantCode(t, readErr(h, p), errcode.NotFound)
	})

	t.Run("operation accepted before a restore needs reconciliation", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "asset/before-restore", "v1")
		h.Authz.Restore()
		fresh := h.Authz.OpenSession(a.who.PrincipalID, time.Hour) // 恢复后重新认证的新会话
		_, err := h.Ledger.Commit(t.Context(), p.OperationID, fresh, proof)
		wantCode(t, err, errcode.OperationNeedsReconciliation)
		wantCode(t, readErr(h, p), errcode.NotFound)
		v, _ := h.Ledger.Operation(t.Context(), p.OperationID)
		if v.Stage != commands.StageBlocked {
			t.Fatalf("stage = %s, want blocked awaiting reconciliation", v.Stage)
		}
	})

	t.Run("forged proof is rejected on first commit", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "asset/forged", "v1")
		forged := proof
		forged.InstallRef = "install/somewhere-else"
		if _, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, forged); err == nil {
			t.Fatal("a proof the installer did not issue was committed")
		}
		wantCode(t, readErr(h, p), errcode.NotFound)
	})

	t.Run("proof for another manifest is quarantined", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "asset/mismatch", "v1")
		proof.Files = append([]install.File(nil), proof.Files...)
		proof.Files[0].Size++
		_, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof)
		wantCode(t, err, errcode.OperationNeedsReconciliation)
		if _, q := h.Installer.Quarantined(p.OperationID); !q {
			t.Fatal("mismatching install was not quarantined")
		}
		wantCode(t, readErr(h, p), errcode.NotFound)
	})

	t.Run("corrupted install is not committed", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "asset/corrupt", "v1")
		h.Installer.Corrupt(p.OperationID)
		_, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof)
		wantCode(t, err, errcode.HashMismatch)
		wantCode(t, readErr(h, p), errcode.NotFound)
	})

	t.Run("append checks base and pending operations", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p1, proof1 := prepareInstalled(t, h, a, "asset/chain", "v1")
		if _, err := h.Ledger.Commit(t.Context(), p1.OperationID, a.who, proof1); err != nil {
			t.Fatal(err)
		}
		files := []install.File{h.Installer.PutBlob("a.txt", []byte("v2"))}
		stale := commit.PrepareRequest{Who: a.who, ProjectID: a.project, AssetID: p1.AssetID, ManifestDigest: ManifestDigest(files), Files: files}
		_, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "stale", stale), stale)
		wantCode(t, err, errcode.BaseVersionConflict)

		next := stale
		next.BaseVersionID = p1.VersionID
		p2, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "v2", next), next)
		if err != nil || p2.VersionNumber != 2 {
			t.Fatalf("append: %+v %v", p2, err)
		}
		// 已有进行中的提交：第二个意图得到 RESOURCE_BUSY 与占用它的 operation。
		files3 := []install.File{h.Installer.PutBlob("a.txt", []byte("v3"))}
		racing := commit.PrepareRequest{Who: a.who, ProjectID: a.project, AssetID: p1.AssetID, BaseVersionID: p1.VersionID, ManifestDigest: ManifestDigest(files3), Files: files3}
		_, err = h.Ledger.Prepare(t.Context(), a.cmd(t, "v3", racing), racing)
		wantCode(t, err, errcode.ResourceBusy)
		if e, _ := errcode.As(err); e.OperationID != string(p2.OperationID) {
			t.Fatalf("RESOURCE_BUSY should name the pending operation, got %q", e.OperationID)
		}
		if h.Cancel != nil {
			// 取消一个已经隔离的操作不能释放别人的占用。
			settled, settledProof := prepareInstalled(t, h, a, "asset/settled", "v1")
			if _, err := h.Ledger.Commit(t.Context(), settled.OperationID, a.who, settledProof); err != nil {
				t.Fatal(err)
			}
			filesX := []install.File{h.Installer.PutBlob("a.txt", []byte("broken"))}
			broken := commit.PrepareRequest{Who: a.who, ProjectID: a.project, AssetID: settled.AssetID, BaseVersionID: settled.VersionID, ManifestDigest: ManifestDigest(filesX), Files: filesX}
			pBroken, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "broken", broken), broken)
			if err != nil {
				t.Fatal(err)
			}
			bad, err := h.Installer.Install(t.Context(), pBroken.InstallRequest())
			if err != nil {
				t.Fatal(err)
			}
			bad.Files = append([]install.File(nil), bad.Files...)
			bad.Files[0].Size++
			if _, err := h.Ledger.Commit(t.Context(), pBroken.OperationID, a.who, bad); err == nil {
				t.Fatal("mismatching proof committed")
			}
			files4 := []install.File{h.Installer.PutBlob("a.txt", []byte("retry"))}
			retry := commit.PrepareRequest{Who: a.who, ProjectID: a.project, AssetID: settled.AssetID, BaseVersionID: settled.VersionID, ManifestDigest: ManifestDigest(files4), Files: files4}
			pRetry, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "retry", retry), retry)
			if err != nil {
				t.Fatal(err)
			}
			_ = h.Cancel(pBroken.OperationID)
			files5 := []install.File{h.Installer.PutBlob("a.txt", []byte("third"))}
			third := commit.PrepareRequest{Who: a.who, ProjectID: a.project, AssetID: settled.AssetID, BaseVersionID: settled.VersionID, ManifestDigest: ManifestDigest(files5), Files: files5}
			_, err = h.Ledger.Prepare(t.Context(), a.cmd(t, "third", third), third)
			wantCode(t, err, errcode.ResourceBusy)
			if e, _ := errcode.As(err); e.OperationID != string(pRetry.OperationID) {
				t.Fatalf("reservation of %s was released by cancelling another operation", pRetry.OperationID)
			}
			dupName := a.newAsset(h, "asset/settled", "other")
			_, err = h.Ledger.Prepare(t.Context(), a.cmd(t, "dup-settled", dupName), dupName)
			wantCode(t, err, errcode.PathConflict)

			if err := h.Cancel(p2.OperationID); err != nil {
				t.Fatal(err)
			}
			p3, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "v3", racing), racing)
			if err != nil {
				t.Fatal(err)
			}
			if p3.VersionNumber != 3 {
				t.Fatalf("version number after cancel = %d, reserved numbers must not be reused", p3.VersionNumber)
			}
		}
	})

	t.Run("names and references", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "asset/named", "v1")
		if _, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof); err != nil {
			t.Fatal(err)
		}
		dup := a.newAsset(h, "asset/named", "other")
		_, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "dup", dup), dup)
		wantCode(t, err, errcode.PathConflict)
		_, err = h.Reader.Version(t.Context(), ids.New(), p.VersionID)
		wantCode(t, err, errcode.RefMismatch)
		_, err = h.Reader.Version(t.Context(), p.AssetID, ids.New())
		wantCode(t, err, errcode.NotFound)
	})

	t.Run("unauthorized prepare leaves no reservation", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		stranger := newActor(h)
		req := a.newAsset(h, "asset/private", "v1")
		req.Who = stranger.who
		cmd := stranger.cmd(t, "k", req)
		cmd.ProjectID = a.project
		req.ProjectID = a.project
		_, err := h.Ledger.Prepare(t.Context(), cmd, req)
		wantCode(t, err, errcode.Forbidden)
		ok := a.newAsset(h, "asset/private", "v1")
		if _, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "k", ok), ok); err != nil {
			t.Fatalf("name was reserved by a rejected request: %v", err)
		}
	})

	t.Run("only the actor cancels a pending operation", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "cancel/me", "v1")
		stranger := newActor(h)
		wantCode(t, h.Ledger.Cancel(t.Context(), p.OperationID, stranger.who), errcode.NotFound)
		if err := h.Ledger.Cancel(t.Context(), p.OperationID, a.who); err != nil {
			t.Fatal(err)
		}
		_, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof)
		wantCode(t, err, errcode.InvalidStateTransition)
		wantCode(t, readErr(h, p), errcode.NotFound)
		if _, err := h.Namespace.Claim(t.Context(), a.project, "cancel/me"); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("cancelled create still holds the name: %v", err)
		}
		p2, proof2 := prepareInstalled(t, h, a, "cancel/done", "v1")
		if _, err := h.Ledger.Commit(t.Context(), p2.OperationID, a.who, proof2); err != nil {
			t.Fatal(err)
		}
		wantCode(t, h.Ledger.Cancel(t.Context(), p2.OperationID, a.who), errcode.InvalidStateTransition)
	})

	t.Run("claims carry monotonic alias generations", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p, proof := prepareInstalled(t, h, a, "ch03/whitebox", "v1")
		if p.AliasGeneration != 1 {
			t.Fatalf("first generation = %d", p.AliasGeneration)
		}
		c, err := h.Namespace.Claim(t.Context(), a.project, "ch03/whitebox")
		if err != nil || c.State != commit.ClaimReserved || c.Generation != 1 || c.AssetID != p.AssetID {
			t.Fatalf("reserved claim: %+v %v", c, err)
		}
		if _, err := h.Reader.Asset(t.Context(), p.AssetID); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("asset visible before commit: %v", err)
		}
		committed, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof)
		if err != nil {
			t.Fatal(err)
		}
		if committed.AliasGeneration != 1 || committed.CommittedBy != a.who.PrincipalID {
			t.Fatalf("committed: %+v", committed)
		}
		c, err = h.Namespace.Claim(t.Context(), a.project, "CH03/WhiteBox") // 按折叠键查找
		if err != nil || c.State != commit.ClaimActive || c.AssetID != p.AssetID || c.Generation != 1 {
			t.Fatalf("active claim: %+v %v", c, err)
		}
		asset, err := h.Reader.Asset(t.Context(), p.AssetID)
		if err != nil || asset.Slug != "ch03/whitebox" || asset.Generation != 1 || asset.CreatedBy != a.who.PrincipalID || asset.ProjectID != a.project {
			t.Fatalf("asset: %+v %v", asset, err)
		}
		caseTwin := a.newAsset(h, "CH03/whitebox", "twin")
		_, err = h.Ledger.Prepare(t.Context(), a.cmd(t, "twin", caseTwin), caseTwin)
		wantCode(t, err, errcode.PathConflict)
		if _, err := h.Namespace.Claim(t.Context(), a.project, "ch03/other"); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("unclaimed path: %v", err)
		}
		if h.Release == nil {
			return
		}
		if err := h.Release(a.project, "ch03/whitebox"); err != nil {
			t.Fatal(err)
		}
		c, _ = h.Namespace.Claim(t.Context(), a.project, "ch03/whitebox")
		if c.State != commit.ClaimReleased || c.Generation != 1 || c.AssetID != p.AssetID {
			t.Fatalf("released claim keeps the last generation: %+v", c)
		}
		p2, proof2 := prepareInstalled(t, h, a, "ch03/whitebox", "reborn")
		if p2.AliasGeneration != 2 || p2.AssetID == p.AssetID {
			t.Fatalf("reused name: %+v", p2)
		}
		if _, err := h.Ledger.Commit(t.Context(), p2.OperationID, a.who, proof2); err != nil {
			t.Fatal(err)
		}
		c, _ = h.Namespace.Claim(t.Context(), a.project, "ch03/whitebox")
		if c.State != commit.ClaimActive || c.Generation != 2 || c.AssetID != p2.AssetID {
			t.Fatalf("second generation: %+v", c)
		}
		old, err := h.Reader.Asset(t.Context(), p.AssetID)
		if err != nil || old.Generation != 1 {
			t.Fatalf("the old asset keeps its identity: %+v %v", old, err)
		}
	})

	t.Run("a cancelled create does not consume a generation", func(t *testing.T) {
		h := newHarness(t)
		if h.Cancel == nil {
			t.Skip("harness cannot cancel")
		}
		a := newActor(h)
		req := a.newAsset(h, "drafts/one", "v1")
		p, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "c1", req), req)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Cancel(p.OperationID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Namespace.Claim(t.Context(), a.project, "drafts/one"); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("cancelled reservation still claims the name: %v", err)
		}
		again := a.newAsset(h, "drafts/one", "v1 again")
		p2, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "c2", again), again)
		if err != nil || p2.AliasGeneration != 1 {
			t.Fatalf("after cancel: %+v %v", p2, err)
		}
	})

	t.Run("versions by number and latest", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		p1, proof1 := prepareInstalled(t, h, a, "seq/asset", "v1")
		c1, err := h.Ledger.Commit(t.Context(), p1.OperationID, a.who, proof1)
		if err != nil {
			t.Fatal(err)
		}
		files := []install.File{h.Installer.PutBlob("a.txt", []byte("v2"))}
		next := commit.PrepareRequest{Who: a.who, ProjectID: a.project, AssetID: p1.AssetID, BaseVersionID: p1.VersionID, ManifestDigest: ManifestDigest(files), Files: files}
		p2, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "seq2", next), next)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := h.Reader.LatestVersion(t.Context(), p1.AssetID); err != nil || got != c1 {
			t.Fatalf("latest before second commit: %+v %v", got, err)
		}
		if _, err := h.Reader.VersionByNumber(t.Context(), p1.AssetID, 2); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("reserved number readable before commit: %v", err)
		}
		proof2, err := h.Installer.Install(t.Context(), p2.InstallRequest())
		if err != nil {
			t.Fatal(err)
		}
		c2, err := h.Ledger.Commit(t.Context(), p2.OperationID, a.who, proof2)
		if err != nil || c2.AliasGeneration != 0 {
			t.Fatalf("append commit: %+v %v", c2, err)
		}
		for n, want := range map[int64]commit.Committed{1: c1, 2: c2} {
			if got, err := h.Reader.VersionByNumber(t.Context(), p1.AssetID, n); err != nil || got != want {
				t.Fatalf("VersionByNumber(%d) = %+v %v", n, got, err)
			}
		}
		if got, _ := h.Reader.LatestVersion(t.Context(), p1.AssetID); got != c2 {
			t.Fatalf("latest = %+v", got)
		}
		if _, err := h.Reader.LatestVersion(t.Context(), ids.New()); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("unknown asset: %v", err)
		}
		// 按资产枚举只返回本资产的已提交版本，按号码升序分页；另一资产不混入。
		other, otherProof := prepareInstalled(t, h, a, "seq/other", "x1")
		if _, err := h.Ledger.Commit(t.Context(), other.OperationID, a.who, otherProof); err != nil {
			t.Fatal(err)
		}
		if got, err := h.Reader.AssetVersions(t.Context(), p1.AssetID, 0, 10); err != nil || len(got) != 2 || got[0] != c1 || got[1] != c2 {
			t.Fatalf("AssetVersions = %+v %v", got, err)
		}
		if got, err := h.Reader.AssetVersions(t.Context(), p1.AssetID, 0, 1); err != nil || len(got) != 1 || got[0] != c1 {
			t.Fatalf("AssetVersions first page = %+v %v", got, err)
		}
		if got, err := h.Reader.AssetVersions(t.Context(), p1.AssetID, 1, 10); err != nil || len(got) != 1 || got[0] != c2 {
			t.Fatalf("AssetVersions after 1 = %+v %v", got, err)
		}
		if got, err := h.Reader.AssetVersions(t.Context(), ids.New(), 0, 10); err != nil || len(got) != 0 {
			t.Fatalf("AssetVersions unknown asset = %+v %v", got, err)
		}
	})

	t.Run("metadata revisions are conditional and idempotent", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		h.Authz.Grant(a.who.PrincipalID, a.project, metaAction)
		p, proof := prepareInstalled(t, h, a, "meta/asset", "v1")
		if _, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof); err != nil {
			t.Fatal(err)
		}
		target := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: a.project, ID: p.AssetID}
		if _, err := h.Metadata.CurrentMetadata(t.Context(), target); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("revision before any commit: %v", err)
		}
		req := a.metaRequest(target, 0, "first")
		pm, err := h.Metadata.PrepareMetadata(t.Context(), a.metaCmd(t, "m1", req), req)
		if err != nil || pm.Revision != 1 {
			t.Fatalf("prepare revision: %+v %v", pm, err)
		}
		if again, err := h.Metadata.PrepareMetadata(t.Context(), a.metaCmd(t, "m1", req), req); err != nil || again != pm {
			t.Fatalf("prepare replay: %+v %v", again, err)
		}
		other := a.metaRequest(target, 0, "other")
		_, err = h.Metadata.PrepareMetadata(t.Context(), a.metaCmd(t, "m1", other), other)
		wantCode(t, err, errcode.IdempotencyConflict)
		_, err = h.Metadata.PrepareMetadata(t.Context(), a.metaCmd(t, "m-busy", other), other)
		wantCode(t, err, errcode.ResourceBusy)
		if e, _ := errcode.As(err); e.OperationID != string(pm.OperationID) {
			t.Fatalf("RESOURCE_BUSY names %q", e.OperationID)
		}
		rp := h.Revisions.Write(pm)
		cm, err := h.Metadata.CommitMetadata(t.Context(), pm.OperationID, a.who, rp)
		if err != nil || cm.Revision != 1 || cm.ContentDigest != req.ContentDigest || cm.CommittedBy != a.who.PrincipalID {
			t.Fatalf("commit revision: %+v %v", cm, err)
		}
		if replay, err := h.Metadata.CommitMetadata(t.Context(), pm.OperationID, a.who, rp); err != nil || replay != cm {
			t.Fatalf("commit replay: %+v %v", replay, err)
		}
		if cur, err := h.Metadata.CurrentMetadata(t.Context(), target); err != nil || cur != cm {
			t.Fatalf("current: %+v %v", cur, err)
		}
		stale := a.metaRequest(target, 0, "stale")
		_, err = h.Metadata.PrepareMetadata(t.Context(), a.metaCmd(t, "m-stale", stale), stale)
		wantCode(t, err, errcode.PreconditionFailed)

		// 修订文件与证明不符：不生效。
		second := a.metaRequest(target, 1, "second")
		pm2, err := h.Metadata.PrepareMetadata(t.Context(), a.metaCmd(t, "m2", second), second)
		if err != nil || pm2.Revision != 2 {
			t.Fatalf("second prepare: %+v %v", pm2, err)
		}
		bad := h.Revisions.Write(pm2)
		bad.ContentDigest = req.ContentDigest
		_, err = h.Metadata.CommitMetadata(t.Context(), pm2.OperationID, a.who, bad)
		wantCode(t, err, errcode.OperationNeedsReconciliation)
		if cur, _ := h.Metadata.CurrentMetadata(t.Context(), target); cur.Revision != 1 {
			t.Fatalf("mismatching revision became current: %+v", cur)
		}

		// 撤权后提交：阻塞，当前修订不变。
		third := a.metaRequest(target, 1, "third")
		pm3, err := h.Metadata.PrepareMetadata(t.Context(), a.metaCmd(t, "m3", third), third)
		if err != nil || pm3.Revision != 2 {
			t.Fatalf("third prepare: %+v %v", pm3, err)
		}
		h.Authz.Revoke(a.who.PrincipalID, a.project)
		_, err = h.Metadata.CommitMetadata(t.Context(), pm3.OperationID, a.who, h.Revisions.Write(pm3))
		if code := errcode.CodeOf(err); code != errcode.Forbidden && code != errcode.TokenRevoked && code != errcode.NotFound {
			t.Fatalf("commit after revocation: %v", err)
		}
		if cur, _ := h.Metadata.CurrentMetadata(t.Context(), target); cur.Revision != 1 {
			t.Fatalf("revoked revision became current: %+v", cur)
		}
		v, err := h.Ledger.Operation(t.Context(), pm3.OperationID)
		if err != nil || v.Stage != commands.StageBlocked {
			t.Fatalf("blocked metadata operation: %+v %v", v, err)
		}
	})

	t.Run("metadata needs an existing target and permission", func(t *testing.T) {
		h := newHarness(t)
		a := newActor(h)
		h.Authz.Grant(a.who.PrincipalID, a.project, metaAction)
		missing := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: a.project, ID: ids.New()}
		req := a.metaRequest(missing, 0, "x")
		_, err := h.Metadata.PrepareMetadata(t.Context(), a.metaCmd(t, "missing", req), req)
		wantCode(t, err, errcode.NotFound)
		stranger := newActor(h)
		p, proof := prepareInstalled(t, h, a, "meta/private", "v1")
		if _, err := h.Ledger.Commit(t.Context(), p.OperationID, a.who, proof); err != nil {
			t.Fatal(err)
		}
		target := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: a.project, ID: p.AssetID}
		sreq := stranger.metaRequest(target, 0, "x")
		cmd := stranger.metaCmd(t, "s", sreq)
		cmd.ProjectID = a.project
		_, err = h.Metadata.PrepareMetadata(t.Context(), cmd, sreq)
		wantCode(t, err, errcode.Forbidden)
	})

	t.Run("projects are registered once per key", func(t *testing.T) {
		h := newHarness(t)
		admin := newActor(h)
		h.Authz.Grant(admin.who.PrincipalID, "", commit.ActionCreateProject)
		req := commit.ProjectRequest{Who: admin.who, Key: "pansi"}
		p1, err := h.Projects.RegisterProject(t.Context(), admin.projectCmd(t, "p1", req), req)
		if err != nil || p1.Key != "pansi" || !p1.ProjectID.Valid() || p1.State != commit.ProjectActive {
			t.Fatalf("register: %+v %v", p1, err)
		}
		if again, err := h.Projects.RegisterProject(t.Context(), admin.projectCmd(t, "p1", req), req); err != nil || again != p1 {
			t.Fatalf("replay: %+v %v", again, err)
		}
		other := commit.ProjectRequest{Who: admin.who, Key: "library"}
		_, err = h.Projects.RegisterProject(t.Context(), admin.projectCmd(t, "p1", other), other)
		wantCode(t, err, errcode.IdempotencyConflict)
		_, err = h.Projects.RegisterProject(t.Context(), admin.projectCmd(t, "p2", req), req)
		wantCode(t, err, errcode.PathConflict)
		if got, err := h.Projects.ProjectByKey(t.Context(), "pansi"); err != nil || got != p1 {
			t.Fatalf("by key: %+v %v", got, err)
		}
		if got, err := h.Projects.Project(t.Context(), p1.ProjectID); err != nil || got != p1 {
			t.Fatalf("by id: %+v %v", got, err)
		}
		if _, err := h.Projects.ProjectByKey(t.Context(), "nope"); errcode.CodeOf(err) != errcode.NotFound {
			t.Fatalf("unknown key: %v", err)
		}
		agent := newActor(h)
		areq := commit.ProjectRequest{Who: agent.who, Key: "agents-own"}
		_, err = h.Projects.RegisterProject(t.Context(), agent.projectCmd(t, "a1", areq), areq)
		wantCode(t, err, errcode.Forbidden)
		badKey := commit.ProjectRequest{Who: admin.who, Key: "Bad Key"}
		_, err = h.Projects.RegisterProject(t.Context(), admin.projectCmd(t, "bad", badKey), badKey)
		wantCode(t, err, errcode.SchemaInvalid)
	})
}

// metaAction 是契约套件用于说明修订的授权动作。
const metaAction authz.Action = "catalog.patch_metadata"

func (a actor) metaRequest(target commit.MetadataTarget, expected int64, content string) commit.MetadataRequest {
	return commit.MetadataRequest{Who: a.who, Target: target, ExpectedRevision: expected,
		ContentDigest: digest.Of([]byte(content)), Action: metaAction}
}

func (a actor) metaCmd(t *testing.T, key string, req commit.MetadataRequest) commands.Context {
	t.Helper()
	body, _ := json.Marshal(struct {
		Kind    commit.TargetKind `json:"kind"`
		Digest  digest.Digest     `json:"content_digest"`
		Expects int64             `json:"expected_revision"`
	}{req.Target.Kind, req.ContentDigest, req.ExpectedRevision})
	hash, err := commands.RequestHash(commands.HashInput{CommandType: commit.CommandCommitMetadata, ProjectID: req.Target.ProjectID,
		Targets: []string{string(req.Target.ID)}, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return commands.Context{
		OperationID: ids.New(), IdempotencyKey: key, RequestHash: hash, CommandType: commit.CommandCommitMetadata,
		ActorID: a.who.PrincipalID, SessionID: a.who.SessionID, ProjectID: req.Target.ProjectID, RecoveryEpoch: a.who.RecoveryEpoch,
	}
}

func (a actor) projectCmd(t *testing.T, key string, req commit.ProjectRequest) commands.Context {
	t.Helper()
	body, _ := json.Marshal(struct {
		Key string `json:"key"`
	}{req.Key})
	hash, err := commands.RequestHash(commands.HashInput{CommandType: commit.CommandRegisterProject, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return commands.Context{
		OperationID: ids.New(), IdempotencyKey: key, RequestHash: hash, CommandType: commit.CommandRegisterProject,
		ActorID: a.who.PrincipalID, SessionID: a.who.SessionID, RecoveryEpoch: a.who.RecoveryEpoch,
	}
}

func prepareInstalled(t *testing.T, h Harness, a actor, slug string, contents ...string) (commit.Prepared, install.Proof) {
	t.Helper()
	req := a.newAsset(h, slug, contents...)
	key := "prep-" + strings.ReplaceAll(slug, "/", ".") + "-" + strings.Join(contents, "-")
	p, err := h.Ledger.Prepare(t.Context(), a.cmd(t, strings.ReplaceAll(key, " ", "_"), req), req)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := h.Installer.Install(t.Context(), p.InstallRequest())
	if err != nil {
		t.Fatal(err)
	}
	return p, proof
}

func readErr(h Harness, p commit.Prepared) error {
	_, err := h.Reader.Version(context.Background(), p.AssetID, p.VersionID)
	return err
}

func assertView(t *testing.T, v *commands.View) {
	t.Helper()
	reg, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v)
	if err := reg.ValidateJSON("lantai.operation/v1", data); err != nil {
		t.Fatalf("operation view: %v\n%s", err, data)
	}
}
