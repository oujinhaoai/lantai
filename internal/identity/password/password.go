// Package password 用 Argon2id（golang.org/x/crypto/argon2，RFC 9106）保存与
// 校验口令，不自制 KDF。参数随每条记录版本化保存，调整默认参数后旧记录仍按
// 自身参数校验；并发计算数由 Hasher 限制，避免登录洪水耗尽内存。
package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/sync/semaphore"
	"golang.org/x/text/unicode/norm"
)

// Params 是 Argon2id 参数。Memory 单位为 KiB。
type Params struct {
	Algorithm string `json:"alg"`
	Version   int    `json:"v"`
	Memory    uint32 `json:"m"`
	Time      uint32 `json:"t"`
	Threads   uint8  `json:"p"`
	KeyLen    uint32 `json:"len"`
}

// Default 是新口令的参数：64 MiB、3 轮、单线程（RFC 9106 第二推荐方案的
// 单线程形式）。在目标 NAS 上的耗时须实测后再调整，调整只影响新记录。
var Default = Params{Algorithm: "argon2id", Version: argon2.Version, Memory: 64 * 1024, Time: 3, Threads: 1, KeyLen: 32}

const saltLen = 16

// 口令长度限制：下限防止弱口令，上限防止超长输入拖慢计算。
const (
	MinLength = 12
	MaxBytes  = 1024
)

// ErrPolicy 表示口令不满足长度或编码要求。
var ErrPolicy = errors.New("password: does not satisfy the password policy")

// Record 是保存的口令校验值。
type Record struct {
	Params Params
	Salt   []byte
	Hash   []byte
}

// ParamsJSON 返回参数的 JSON 表示，写入库中的 params 列。
func (r Record) ParamsJSON() (string, error) {
	b, err := json.Marshal(r.Params)
	return string(b), err
}

// ParseParams 解析库中的 params 列并检查参数在允许范围内。
func ParseParams(s string) (Params, error) {
	var p Params
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return p, fmt.Errorf("password: params: %w", err)
	}
	if err := p.check(); err != nil {
		return p, err
	}
	return p, nil
}

func (p Params) check() error {
	switch {
	case p.Algorithm != "argon2id":
		return fmt.Errorf("password: unsupported algorithm %q", p.Algorithm)
	case p.Version != argon2.Version:
		return fmt.Errorf("password: unsupported argon2 version %d", p.Version)
	case p.Memory < 8 || p.Memory > 4*1024*1024 || p.Time < 1 || p.Time > 100 || p.Threads < 1 || p.KeyLen < 16 || p.KeyLen > 64:
		return fmt.Errorf("password: argon2 parameters out of range: %+v", p)
	}
	return nil
}

// Normalize 按 NFC 规范化口令，使不同平台输入法得到的同一口令一致；并检查
// 长度与编码。
func Normalize(pw string) (string, error) {
	if !utf8.ValidString(pw) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrPolicy)
	}
	pw = norm.NFC.String(pw)
	if len(pw) > MaxBytes {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrPolicy, MaxBytes)
	}
	if utf8.RuneCountInString(pw) < MinLength {
		return "", fmt.Errorf("%w: shorter than %d characters", ErrPolicy, MinLength)
	}
	return pw, nil
}

// Hasher 限制同时进行的 Argon2 计算数，并持有新记录使用的参数。
type Hasher struct {
	params Params
	sem    *semaphore.Weighted
}

// NewHasher 创建 Hasher；concurrency 为同时计算的上限（内存上限约为
// concurrency × Memory）。
func NewHasher(p Params, concurrency int) (*Hasher, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if concurrency < 1 {
		concurrency = 1
	}
	return &Hasher{params: p, sem: semaphore.NewWeighted(int64(concurrency))}, nil
}

// Params 返回新记录的参数。
func (h *Hasher) Params() Params { return h.params }

// Hash 规范化并保存新口令。
func (h *Hasher) Hash(ctx context.Context, pw string) (Record, error) {
	pw, err := Normalize(pw)
	if err != nil {
		return Record{}, err
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return Record{}, err
	}
	sum, err := h.derive(ctx, pw, salt, h.params)
	if err != nil {
		return Record{}, err
	}
	return Record{Params: h.params, Salt: salt, Hash: sum}, nil
}

// Verify 按记录自身的参数校验口令，比较使用常数时间。
func (h *Hasher) Verify(ctx context.Context, pw string, r Record) (bool, error) {
	if err := r.Params.check(); err != nil {
		return false, err
	}
	pw = norm.NFC.String(pw)
	if !utf8.ValidString(pw) || len(pw) > MaxBytes {
		return false, nil
	}
	sum, err := h.derive(ctx, pw, r.Salt, r.Params)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(sum, r.Hash) == 1, nil
}

// Burn 用当前参数做一次不会成功的计算，让“主体不存在”与“口令错误”的响应
// 耗时相近，不能据此枚举主体名。
func (h *Hasher) Burn(ctx context.Context) error {
	_, err := h.derive(ctx, "lantai-nonexistent-principal", make([]byte, saltLen), h.params)
	return err
}

func (h *Hasher) derive(ctx context.Context, pw string, salt []byte, p Params) ([]byte, error) {
	if err := h.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer h.sem.Release(1)
	return argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, p.KeyLen), nil
}
