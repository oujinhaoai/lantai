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
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/install/installtest"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

// Harness 让契约套件驱动任意台账实现；安装端使用 installtest 桩。
type Harness struct {
	Ledger    commit.Ledger
	Reader    commit.Reader
	Authz     *authztest.Static
	Installer *installtest.Memory
	Clock     *clock.Fake
	// Cancel 可选：放弃尚未提交的操作。
	Cancel func(op ids.ID) error
}

// NewHarness 用内存台账组装一个可直接使用的环境。
func NewHarness(t *testing.T) Harness {
	clk := clock.NewFake(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	az := authztest.New(clk, ids.New())
	in := installtest.New(clk)
	m := New(clk, az, in)
	return Harness{Ledger: m, Reader: m, Authz: az, Installer: in, Clock: clk, Cancel: m.Cancel}
}

type actor struct {
	who     authz.Context
	project ids.ID
}

func newActor(h Harness) actor {
	p, project := ids.New(), ids.New()
	h.Authz.AddPrincipal(p, authz.Agent)
	h.Authz.Grant(p, project, commit.ActionCommitVersion, commit.ActionReadVersion)
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
			// 取消一个已经终结的操作不能释放别人的占用。
			settled, settledProof := prepareInstalled(t, h, a, "asset/settled", "v1")
			bad := settledProof
			bad.Files = append([]install.File(nil), bad.Files...)
			bad.Files[0].Size++
			if _, err := h.Ledger.Commit(t.Context(), settled.OperationID, a.who, bad); err == nil {
				t.Fatal("mismatching proof committed")
			}
			files4 := []install.File{h.Installer.PutBlob("a.txt", []byte("retry"))}
			retry := commit.PrepareRequest{Who: a.who, ProjectID: a.project, AssetID: settled.AssetID, ManifestDigest: ManifestDigest(files4), Files: files4}
			pRetry, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "retry", retry), retry)
			if err != nil {
				t.Fatal(err)
			}
			_ = h.Cancel(settled.OperationID)
			files5 := []install.File{h.Installer.PutBlob("a.txt", []byte("third"))}
			third := commit.PrepareRequest{Who: a.who, ProjectID: a.project, AssetID: settled.AssetID, ManifestDigest: ManifestDigest(files5), Files: files5}
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
}

func prepareInstalled(t *testing.T, h Harness, a actor, slug string, contents ...string) (commit.Prepared, install.Proof) {
	t.Helper()
	req := a.newAsset(h, slug, contents...)
	p, err := h.Ledger.Prepare(t.Context(), a.cmd(t, "prep-"+strings.ReplaceAll(slug, "/", "."), req), req)
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
