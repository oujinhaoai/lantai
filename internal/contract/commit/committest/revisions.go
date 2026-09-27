package committest

import (
	"context"
	"strings"
	"sync"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Revisions 是说明修订文件端的内存桩：记录“已写出”的修订文件并只认自己
// 签发的证明，供台账契约套件与 catalog 的单元测试使用。真实实现是 storage
// 的修订文件适配器。
type Revisions struct {
	mu      sync.Mutex
	clock   clock.Clock
	written map[ids.ID]commit.RevisionProof
	broken  map[ids.ID]bool
}

var _ commit.RevisionVerifier = (*Revisions)(nil)

// NewRevisions 创建修订文件桩。
func NewRevisions(clk clock.Clock) *Revisions {
	return &Revisions{clock: clk, written: map[ids.ID]commit.RevisionProof{}, broken: map[ids.ID]bool{}}
}

// Write 模拟写出 prepared 修订的文件并返回证明；同一操作重入返回同一证明。
func (r *Revisions) Write(p commit.PreparedMetadata) commit.RevisionProof {
	r.mu.Lock()
	defer r.mu.Unlock()
	if proof, ok := r.written[p.OperationID]; ok {
		return proof
	}
	proof := commit.RevisionProof{OperationID: p.OperationID, Target: p.Target, Revision: p.Revision,
		ContentDigest: p.ContentDigest, FileRef: "revisions/" + strings.ToLower(string(p.OperationID)), WrittenAt: r.clock.Now()}
	r.written[p.OperationID] = proof
	return proof
}

// Corrupt 模拟修订文件在写出后被改动。
func (r *Revisions) Corrupt(op ids.ID) {
	r.mu.Lock()
	r.broken[op] = true
	r.mu.Unlock()
}

// VerifyRevision 实现 commit.RevisionVerifier。
func (r *Revisions) VerifyRevision(_ context.Context, p commit.RevisionProof) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	issued, ok := r.written[p.OperationID]
	switch {
	case !ok:
		return errcode.New(errcode.OperationNeedsReconciliation, "no revision file written for operation")
	case r.broken[p.OperationID]:
		return errcode.New(errcode.HashMismatch, "revision file no longer matches its digest")
	case issued.Target != p.Target || issued.Revision != p.Revision || issued.ContentDigest != p.ContentDigest ||
		issued.FileRef != p.FileRef || !issued.WrittenAt.Equal(p.WrittenAt):
		return errcode.New(errcode.OperationNeedsReconciliation, "proof was not issued for this revision file")
	}
	return nil
}
