// Package installtest 提供 install.Installer 的内存桩，供台账（T03）等模块
// 在 storage（T02）完成前测试提交流程。它按契约核验内容与幂等性，并能
// 注入存储故障和安装后损坏；不涉及真实文件系统语义。
package installtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
)

// Memory 是内存安装器。
type Memory struct {
	mu       sync.Mutex
	clock    clock.Clock
	blobs    map[string][]byte
	installs map[ids.ID]*record
	failNext error
	calls    int
}

type record struct {
	req         install.Request
	proof       install.Proof
	corrupted   bool
	quarantined errcode.Code
}

var _ install.Installer = (*Memory)(nil)

// New 创建内存安装器。
func New(clk clock.Clock) *Memory {
	return &Memory{clock: clk, blobs: map[string][]byte{}, installs: map[ids.ID]*record{}}
}

// PutBlob 放入已上传并校验的内容，返回可放进清单的文件条目。
func (m *Memory) PutBlob(path string, content []byte) install.File {
	sum := sha256.Sum256(content)
	h := hex.EncodeToString(sum[:])
	m.mu.Lock()
	m.blobs[h] = slices.Clone(content)
	m.mu.Unlock()
	return install.File{Path: path, SHA256: h, Size: int64(len(content))}
}

// FailNext 让下一次 Install 返回 err（例如 STORAGE_FULL），模拟存储故障。
func (m *Memory) FailNext(err error) {
	m.mu.Lock()
	m.failNext = err
	m.mu.Unlock()
}

// Corrupt 模拟安装后内容损坏，之后 Verify 失败。
func (m *Memory) Corrupt(op ids.ID) {
	m.mu.Lock()
	if r := m.installs[op]; r != nil {
		r.corrupted = true
	}
	m.mu.Unlock()
}

// Quarantined 报告操作是否已隔离及原因。
func (m *Memory) Quarantined(op ids.ID) (errcode.Code, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.installs[op]
	if r == nil || r.quarantined == "" {
		return "", false
	}
	return r.quarantined, true
}

// InstallCalls 返回实际执行安装（非幂等重放）的次数。
func (m *Memory) InstallCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// Install 实现 install.Installer。
func (m *Memory) Install(_ context.Context, req install.Request) (install.Proof, error) {
	if err := req.Validate(); err != nil {
		return install.Proof{}, errcode.Wrap(errcode.SchemaInvalid, "invalid install request", err)
	}
	paths := make([]string, len(req.Files))
	for i, f := range req.Files {
		if err := pathrule.Check(f.Path); err != nil {
			return install.Proof{}, err
		}
		paths[i] = f.Path
	}
	if err := pathrule.CheckSet(paths); err != nil {
		return install.Proof{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failNext; err != nil {
		m.failNext = nil
		return install.Proof{}, err
	}
	if r := m.installs[req.OperationID]; r != nil {
		if r.quarantined != "" {
			return install.Proof{}, errcode.New(errcode.OperationNeedsReconciliation, "operation was quarantined")
		}
		if !sameRequest(r.req, req) {
			return install.Proof{}, errcode.New(errcode.IdempotencyConflict, "operation already installed a different manifest")
		}
		return r.proof, nil
	}
	for _, f := range req.Files {
		content, ok := m.blobs[f.SHA256]
		if !ok {
			return install.Proof{}, errcode.New(errcode.BlobGrantRequired, "")
		}
		if int64(len(content)) != f.Size {
			return install.Proof{}, errcode.New(errcode.HashMismatch, "size does not match the manifest")
		}
	}
	m.calls++
	p := install.Proof{
		OperationID: req.OperationID, ProjectID: req.ProjectID, AssetID: req.AssetID, VersionID: req.VersionID,
		VersionNumber: req.VersionNumber, ManifestDigest: req.ManifestDigest, Files: slices.Clone(req.Files),
		InstallRef: "install/" + strings.ToLower(string(req.OperationID)), InstalledAt: m.clock.Now(),
	}
	if len(req.Manifest) > 0 {
		sum := sha256.Sum256(req.Manifest)
		p.ManifestSHA256 = hex.EncodeToString(sum[:])
	}
	req.Manifest = bytes.Clone(req.Manifest)
	m.installs[req.OperationID] = &record{req: req, proof: p}
	return p, nil
}

// Verify 实现 install.Installer：只接受本安装器为该操作签发的证明。
func (m *Memory) Verify(_ context.Context, p install.Proof) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.installs[p.OperationID]
	switch {
	case r == nil:
		return errcode.New(errcode.OperationNeedsReconciliation, "no install recorded for operation")
	case r.quarantined != "":
		return errcode.New(errcode.OperationNeedsReconciliation, "operation was quarantined")
	case r.corrupted:
		return errcode.New(errcode.HashMismatch, "installed content no longer matches the proof")
	}
	if !sameProof(p, r.proof) {
		return errcode.New(errcode.OperationNeedsReconciliation, "proof was not issued for this install")
	}
	return p.Matches(r.req)
}

// sameProof 要求证明与安装器签发的完全一致，包括 install_ref 与 installed_at。
func sameProof(a, b install.Proof) bool {
	return a.OperationID == b.OperationID && a.ProjectID == b.ProjectID && a.AssetID == b.AssetID &&
		a.VersionID == b.VersionID && a.VersionNumber == b.VersionNumber && a.ManifestDigest == b.ManifestDigest &&
		a.ManifestSHA256 == b.ManifestSHA256 && a.InstallRef == b.InstallRef && a.InstalledAt.Equal(b.InstalledAt) &&
		slices.Equal(a.Files, b.Files)
}

// Quarantine 实现 install.Installer。
func (m *Memory) Quarantine(_ context.Context, op ids.ID, reason errcode.Code) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.installs[op]
	if r == nil {
		r = &record{}
		m.installs[op] = r
	}
	r.quarantined = reason
	return nil
}

func sameRequest(a, b install.Request) bool {
	return a.OperationID == b.OperationID && a.ProjectID == b.ProjectID && a.AssetID == b.AssetID &&
		a.VersionID == b.VersionID && a.VersionNumber == b.VersionNumber && a.ManifestDigest == b.ManifestDigest &&
		slices.Equal(a.Files, b.Files) && bytes.Equal(a.Manifest, b.Manifest)
}
