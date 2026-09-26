package installtest

import (
	"errors"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

// Harness 让契约套件驱动任意 Installer 实现。
type Harness struct {
	Installer install.Installer
	// Put 让内容对安装可用（真实实现即上传并校验、取得本操作授权）。
	Put func(path string, content []byte) install.File
	// Corrupt 可选：模拟安装后内容损坏。
	Corrupt func(op ids.ID)
}

// RunInstallerContract 对实现运行 install 契约测试。storage 的真实实现须通过它。
func RunInstallerContract(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Run("idempotent install", func(t *testing.T) {
		h := newHarness(t)
		req := request(h)
		p1, err := h.Installer.Install(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		p2, err := h.Installer.Install(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		d1, err := p1.Digest()
		if err != nil {
			t.Fatalf("proof does not satisfy %s: %v", install.Contract, err)
		}
		d2, _ := p2.Digest()
		if d1 != d2 {
			t.Fatal("re-installing the same request must return the same proof")
		}
		if err := p1.Matches(req); err != nil {
			t.Fatal(err)
		}
		if err := h.Installer.Verify(t.Context(), p1); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("only issued proofs verify", func(t *testing.T) {
		h := newHarness(t)
		req := request(h)
		p, err := h.Installer.Install(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		forgedRef := p
		forgedRef.InstallRef = "install/somewhere-else"
		forgedTime := p
		forgedTime.InstalledAt = p.InstalledAt.Add(time.Second)
		forgedFiles := p
		forgedFiles.Files = append([]install.File(nil), p.Files...)
		forgedFiles.Files[0].Size++
		for name, f := range map[string]install.Proof{"install_ref": forgedRef, "installed_at": forgedTime, "files": forgedFiles} {
			if h.Installer.Verify(t.Context(), f) == nil {
				t.Errorf("proof with altered %s verified", name)
			}
		}
		unknown := p
		unknown.OperationID = ids.New()
		if h.Installer.Verify(t.Context(), unknown) == nil {
			t.Error("proof for an operation that never installed verified")
		}
	})
	t.Run("same operation different manifest conflicts", func(t *testing.T) {
		h := newHarness(t)
		req := request(h)
		if _, err := h.Installer.Install(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		other := req
		other.Files = append([]install.File{h.Put("0-extra.txt", []byte("extra"))}, req.Files...)
		other.ManifestDigest = manifestDigest(other.Files)
		_, err := h.Installer.Install(t.Context(), other)
		wantCode(t, err, errcode.IdempotencyConflict)
	})
	t.Run("missing content needs a grant", func(t *testing.T) {
		h := newHarness(t)
		req := request(h)
		req.Files = append(req.Files, install.File{Path: "zz-missing.bin", SHA256: "ab" + req.Files[0].SHA256[2:], Size: 3})
		req.ManifestDigest = manifestDigest(req.Files)
		_, err := h.Installer.Install(t.Context(), req)
		wantCode(t, err, errcode.BlobGrantRequired)
	})
	t.Run("size mismatch", func(t *testing.T) {
		h := newHarness(t)
		req := request(h)
		req.Files[0].Size++
		req.ManifestDigest = manifestDigest(req.Files)
		_, err := h.Installer.Install(t.Context(), req)
		wantCode(t, err, errcode.HashMismatch)
	})
	t.Run("invalid request rejected before writing", func(t *testing.T) {
		h := newHarness(t)
		req := request(h)
		req.Files[0], req.Files[1] = req.Files[1], req.Files[0]
		if _, err := h.Installer.Install(t.Context(), req); err == nil {
			t.Fatal("unsorted file list accepted")
		}
		req = request(h)
		req.Files[0].Path = "../escape.txt"
		if _, err := h.Installer.Install(t.Context(), req); err == nil {
			t.Fatal("path traversal accepted")
		}
		req = request(h)
		req.Files[0].Path = "README\xff.md"
		if _, err := h.Installer.Install(t.Context(), req); err == nil {
			t.Fatal("path with invalid UTF-8 accepted")
		}
	})
	t.Run("quarantine keeps operation from installing again", func(t *testing.T) {
		h := newHarness(t)
		req := request(h)
		p, err := h.Installer.Install(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Installer.Quarantine(t.Context(), req.OperationID, errcode.Forbidden); err != nil {
			t.Fatal(err)
		}
		if h.Installer.Verify(t.Context(), p) == nil {
			t.Fatal("quarantined install still verifies")
		}
		if _, err := h.Installer.Install(t.Context(), req); err == nil {
			t.Fatal("quarantined operation installed again")
		}
	})
	if newHarness(t).Corrupt != nil {
		t.Run("corruption detected by verify", func(t *testing.T) {
			h := newHarness(t)
			req := request(h)
			p, err := h.Installer.Install(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			h.Corrupt(req.OperationID)
			if h.Installer.Verify(t.Context(), p) == nil {
				t.Fatal("corrupted install still verifies")
			}
		})
	}
}

func request(h Harness) install.Request {
	files := []install.File{
		h.Put("README.md", []byte("# synthetic asset\n")),
		h.Put("model/body.glb", []byte("glTF-synthetic-bytes")),
	}
	return install.Request{
		OperationID: ids.New(), ProjectID: ids.New(), AssetID: ids.New(), VersionID: ids.New(),
		VersionNumber: 1, ManifestDigest: manifestDigest(files), Files: files,
	}
}

// manifestDigest 为测试清单计算稳定摘要（真实清单摘要由 catalog 定义）。
func manifestDigest(files []install.File) digest.Digest {
	h := digest.NewHasher()
	for _, f := range files {
		h.Write([]byte(f.Path + "\x00" + f.SHA256 + "\x00"))
	}
	return h.Digest()
}

func wantCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got success", code)
	}
	var ve *schema.ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("want %s, got schema error %v", code, err)
	}
	if got := errcode.CodeOf(err); got != code {
		t.Fatalf("want %s, got %s (%v)", code, got, err)
	}
}
