// Package ids 实现兰台对象标识的公共契约。
//
// 所有对象 ID 都是服务端生成的 ULID，只接受 26 位大写 Crockford Base32
// 规范形式；小写、易混字母或其他变体一律拒绝，保证同一对象只有一种拼写，
// 可以直接作为唯一键比较。ID 里的时间成分只用于诊断和存储局部性，
// 不能用来推断业务先后，业务顺序以 revision 为准。
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
)

// ID 是规范形式的 ULID 字符串。
type ID string

// Len 是规范 ULID 的字符长度。
const Len = 26

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// maxTime 是 48 位时间成分能表示的最大毫秒数。
const maxTime = 1<<48 - 1

// ErrInvalid 表示字符串不是规范形式的 ULID。
var ErrInvalid = errors.New("ids: not a canonical ULID")

var decodeTable = func() [256]byte {
	var t [256]byte
	for i := range t {
		t[i] = 0xFF
	}
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = byte(i)
	}
	return t
}()

// Parse 校验并返回规范 ULID。
func Parse(s string) (ID, error) {
	if _, err := decode(s); err != nil {
		return "", err
	}
	return ID(s), nil
}

// MustParse 用于测试与常量，非法输入 panic。
func MustParse(s string) ID {
	id, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}

// Valid 报告 id 是否为规范 ULID。
func (id ID) Valid() bool {
	_, err := decode(string(id))
	return err == nil
}

// String 实现 fmt.Stringer。
func (id ID) String() string { return string(id) }

// Time 返回 ID 的时间成分；仅用于诊断，不代表业务顺序。
func (id ID) Time() (time.Time, error) {
	b, err := decode(string(id))
	if err != nil {
		return time.Time{}, err
	}
	var ts [8]byte
	copy(ts[2:], b[:6])
	return clock.FromMillis(int64(binary.BigEndian.Uint64(ts[:]))), nil
}

// Generator 生成新的 ULID；时钟与随机源可替换，便于测试。
type Generator struct {
	Clock clock.Clock
	Rand  io.Reader
}

var defaultGenerator = &Generator{Clock: clock.System{}, Rand: rand.Reader}

// New 用系统时钟与 crypto/rand 生成新 ID。
func New() ID { return defaultGenerator.MustNew() }

// New 生成新 ID；随机源读取失败时返回错误。
func (g *Generator) New() (ID, error) {
	ms := clock.Millis(g.Clock.Now())
	if ms < 0 || ms > maxTime {
		return "", fmt.Errorf("ids: clock value %d ms outside ULID range", ms)
	}
	var b [16]byte
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[:6], ts[2:])
	if _, err := io.ReadFull(g.Rand, b[6:]); err != nil {
		return "", fmt.Errorf("ids: read entropy: %w", err)
	}
	return ID(encode(b)), nil
}

// MustNew 与 New 相同，失败时 panic。crypto/rand 在受支持平台上不会失败。
func (g *Generator) MustNew() ID {
	id, err := g.New()
	if err != nil {
		panic(err)
	}
	return id
}

// childDomain 是派生子操作 ID 时使用的域分隔前缀，属于契约的一部分。
const childDomain = "lantai.child-operation/v1"

// DeriveChild 从父 operation_id 与稳定的步骤键派生子 operation_id。
//
// 同一父 ID 与步骤键永远得到同一子 ID，因此重试不会生成新的子操作；
// 结果仍是规范 ULID：前 48 位沿用父 ID 的时间成分，后 80 位取
// SHA-256(childDomain 0x00 parent 0x00 step) 的前 10 字节。
// 步骤键必须由调用方稳定给出，例如 "item:<target_id>" 或 "step:publish"。
func DeriveChild(parent ID, step string) (ID, error) {
	pb, err := decode(string(parent))
	if err != nil {
		return "", fmt.Errorf("ids: derive child: parent: %w", err)
	}
	if err := checkStepKey(step); err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(childDomain))
	h.Write([]byte{0})
	h.Write([]byte(parent))
	h.Write([]byte{0})
	h.Write([]byte(step))
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:6], pb[:6])
	copy(b[6:], sum[:10])
	return ID(encode(b)), nil
}

func checkStepKey(step string) error {
	if step == "" || len(step) > 200 {
		return fmt.Errorf("ids: step key must be 1-200 bytes, got %d", len(step))
	}
	for _, r := range step {
		if r < 0x21 || r > 0x7E {
			return fmt.Errorf("ids: step key %q must be printable ASCII without spaces", step)
		}
	}
	return nil
}

func encode(b [16]byte) string {
	var out [Len]byte
	for i := range Len {
		var v byte
		for bit := range 5 {
			pos := 5*i + bit - 2 // 130 位表示的前 2 位固定为 0
			v <<= 1
			if pos >= 0 && b[pos/8]&(0x80>>(pos%8)) != 0 {
				v |= 1
			}
		}
		out[i] = alphabet[v]
	}
	return string(out[:])
}

func decode(s string) ([16]byte, error) {
	var b [16]byte
	if len(s) != Len {
		return b, ErrInvalid
	}
	for i := range Len {
		v := decodeTable[s[i]]
		if v == 0xFF {
			return b, ErrInvalid
		}
		if i == 0 && v > 7 {
			return b, ErrInvalid
		}
		for bit := range 5 {
			pos := 5*i + bit - 2
			if pos < 0 {
				continue
			}
			if v&(0x10>>bit) != 0 {
				b[pos/8] |= 0x80 >> (pos % 8)
			}
		}
	}
	return b, nil
}

// PermanentRef 是指向具体版本的永久引用。
//
// 本馆入口可以省略 InstanceID；持久保存（uses、审定、发布、任务输入、
// 检查与迁移映射）前必须用 Complete 补齐。
type PermanentRef struct {
	InstanceID ID `json:"instance_id,omitempty"`
	AssetID    ID `json:"asset_id"`
	VersionID  ID `json:"version_id"`
}

// URIScheme 是永久引用的外部 URI 方案。
const URIScheme = "lantai"

// Validate 校验字段格式；requireInstance 为 true 时要求 InstanceID 已补齐。
// 版本是否属于该资产由所属模块判定（REF_MISMATCH），不在此处推断。
func (r PermanentRef) Validate(requireInstance bool) error {
	if requireInstance || r.InstanceID != "" {
		if !r.InstanceID.Valid() {
			return fmt.Errorf("ids: permanent ref instance_id: %w", ErrInvalid)
		}
	}
	if !r.AssetID.Valid() {
		return fmt.Errorf("ids: permanent ref asset_id: %w", ErrInvalid)
	}
	if !r.VersionID.Valid() {
		return fmt.Errorf("ids: permanent ref version_id: %w", ErrInvalid)
	}
	return nil
}

// Complete 在缺少 InstanceID 时补上本馆 ID，返回可持久保存的引用。
func (r PermanentRef) Complete(local ID) PermanentRef {
	if r.InstanceID == "" {
		r.InstanceID = local
	}
	return r
}

// URI 返回 lantai://<instance_id>/assets/<asset_id>/versions/<version_id>。
// 引用必须已补齐 InstanceID。
func (r PermanentRef) URI() (string, error) {
	if err := r.Validate(true); err != nil {
		return "", err
	}
	return URIScheme + "://" + string(r.InstanceID) + "/assets/" + string(r.AssetID) + "/versions/" + string(r.VersionID), nil
}

// ParseURI 解析永久引用 URI；只接受规范形式，不做大小写或路径修正。
func ParseURI(s string) (PermanentRef, error) {
	rest, ok := strings.CutPrefix(s, URIScheme+"://")
	if !ok {
		return PermanentRef{}, fmt.Errorf("ids: permanent ref URI must start with %s://", URIScheme)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 5 || parts[1] != "assets" || parts[3] != "versions" {
		return PermanentRef{}, errors.New("ids: permanent ref URI must be lantai://<instance_id>/assets/<asset_id>/versions/<version_id>")
	}
	r := PermanentRef{InstanceID: ID(parts[0]), AssetID: ID(parts[2]), VersionID: ID(parts[4])}
	if err := r.Validate(true); err != nil {
		return PermanentRef{}, err
	}
	return r, nil
}
