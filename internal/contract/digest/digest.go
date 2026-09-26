// Package digest 定义兰台内容摘要的两种文本形式。
//
// 命名规则：字段名为 sha256 的值是 64 位小写十六进制（例如 Blob 与清单
// 文件条目）；以 _digest 结尾或名为 request_hash 的字段带算法前缀，
// 形如 "sha256:<64 位小写十六进制>"。两种形式都不接受大写或其他算法。
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"strings"
)

// Prefix 是带算法前缀摘要的前缀。
const Prefix = "sha256:"

// Digest 是 "sha256:<hex>" 形式的摘要。
type Digest string

// ErrInvalid 表示摘要格式不符合契约。
var ErrInvalid = errors.New("digest: invalid format")

// ValidHex 报告 s 是否为 64 位小写十六进制 SHA-256。
func ValidHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Parse 校验 "sha256:<hex>" 形式。
func Parse(s string) (Digest, error) {
	hexPart, ok := strings.CutPrefix(s, Prefix)
	if !ok || !ValidHex(hexPart) {
		return "", ErrInvalid
	}
	return Digest(s), nil
}

// FromHex 把 64 位小写十六进制转换为带前缀的摘要。
func FromHex(h string) (Digest, error) {
	if !ValidHex(h) {
		return "", ErrInvalid
	}
	return Digest(Prefix + h), nil
}

// Hex 返回不带前缀的十六进制部分；格式非法时返回空串。
func (d Digest) Hex() string {
	h, ok := strings.CutPrefix(string(d), Prefix)
	if !ok || !ValidHex(h) {
		return ""
	}
	return h
}

// Valid 报告 d 是否符合契约格式。
func (d Digest) Valid() bool { return d.Hex() != "" }

// String 实现 fmt.Stringer。
func (d Digest) String() string { return string(d) }

// Of 计算 data 的摘要。
func Of(data []byte) Digest {
	sum := sha256.Sum256(data)
	return Digest(Prefix + hex.EncodeToString(sum[:]))
}

// Hasher 流式计算 SHA-256 并统计字节数，内存占用与输入大小无关。
type Hasher struct {
	h hash.Hash
	n int64
}

// NewHasher 创建流式哈希器。
func NewHasher() *Hasher { return &Hasher{h: sha256.New()} }

// Write 实现 io.Writer，永不返回错误。
func (h *Hasher) Write(p []byte) (int, error) {
	n, _ := h.h.Write(p)
	h.n += int64(n)
	return n, nil
}

// Size 返回已写入字节数。
func (h *Hasher) Size() int64 { return h.n }

// Hex 返回当前的 64 位小写十六进制摘要。
func (h *Hasher) Hex() string { return hex.EncodeToString(h.h.Sum(nil)) }

// Digest 返回当前的带前缀摘要。
func (h *Hasher) Digest() Digest { return Digest(Prefix + h.Hex()) }
