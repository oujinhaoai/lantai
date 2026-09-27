// Package corecheck 是随核心编译的 manifest 一致性检查器。调用方负责提供已授权
// 的固定版本字节；本包只做纯校验，不读磁盘、联网或写台账，也不产生人审结论。
package corecheck

import (
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Check 验证文档与指定版本/摘要绑定。false 是业务检查不通过，不是运行崩溃。
func Check(raw []byte, ref ids.PermanentRef, expected digest.Digest) bool {
	if ref.Validate(true) != nil || !expected.Valid() {
		return false
	}
	doc, err := manifest.Parse(raw)
	return err == nil && doc.InstanceID == ref.InstanceID && doc.AssetID == ref.AssetID && doc.VersionID == ref.VersionID && doc.ManifestDigest == expected
}
