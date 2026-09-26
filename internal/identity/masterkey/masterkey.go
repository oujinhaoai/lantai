// Package masterkey 管理兰台实例的主密钥：初始化时随机生成，单独保存在密钥
// 目录（不在五库、事件或素材目录中），用于加密 TOTP 种子等需要可逆保存的
// 认证资料，并派生 CSRF 等服务端子密钥。
//
// 加密使用标准库经过验证的 AES-256-GCM，每次随机 96 位 nonce，附加数据绑定
// 用途与对象 ID，密文不能挪用到别的对象；子密钥由 HKDF-SHA256 按标签派生。
// 主密钥丢失意味着已登记的 TOTP 因子无法验证，只能走因子恢复；主密钥需按
// 运维流程单独备份。
package masterkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// Contract 是主密钥文件的格式标识。
const Contract = "lantai.master-key/v1"

// FileName 是密钥目录中主密钥文件的名称。
const FileName = "master.key"

const keyLen = 32

// sealVersion 是密文格式的首字节，便于以后更换算法。
const sealVersion = 1

// Key 是主密钥。零值不可用；字段不导出，避免被序列化进日志。
type Key struct {
	id        ids.ID
	material  []byte
	createdAt time.Time
}

type fileWire struct {
	Contract  string `json:"contract"`
	KeyID     ids.ID `json:"key_id"`
	Key       string `json:"key"`
	CreatedAt string `json:"created_at"`
}

// ErrMissing 表示密钥文件不存在。
var ErrMissing = errors.New("masterkey: master key file does not exist")

// ErrDecrypt 表示密文无法用当前主密钥与附加数据解开（被改动、挪用或密钥不符）。
var ErrDecrypt = errors.New("masterkey: ciphertext cannot be opened with this key")

// Generate 随机生成新主密钥。
func Generate(gen *ids.Generator, clk clock.Clock, rnd io.Reader) (*Key, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	id, err := gen.New()
	if err != nil {
		return nil, err
	}
	k := &Key{id: id, material: make([]byte, keyLen), createdAt: clk.Now()}
	if _, err := io.ReadFull(rnd, k.material); err != nil {
		return nil, fmt.Errorf("masterkey: read entropy: %w", err)
	}
	return k, nil
}

// ID 返回密钥 ID；加密资料记录它以便将来轮换。
func (k *Key) ID() ids.ID { return k.id }

// Path 返回 dir 下的主密钥文件路径。
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Save 把新密钥写入 dir（目录不存在时以仅本用户可访问的权限创建）；文件已
// 存在时返回 fs.ErrExist，绝不覆盖已有主密钥。
func Save(dir string, k *Key) error {
	if err := fsutil.EnsurePrivateDir(dir); err != nil {
		return err
	}
	w := fileWire{Contract: Contract, KeyID: k.id, Key: base64.StdEncoding.EncodeToString(k.material), CreatedAt: clock.Format(k.createdAt)}
	raw, err := json.Marshal(w)
	if err != nil {
		return err
	}
	raw, err = canonjson.Canonicalize(raw)
	if err != nil {
		return err
	}
	return fsutil.CreateFileExclusive(Path(dir), append(raw, '\n'), 0o600)
}

// Load 读取 dir 下的主密钥。Unix 上文件对组或其他用户开放任何权限时拒绝，
// 与 SSH 私钥的做法一致。
func Load(dir string) (*Key, error) {
	path := Path(dir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrMissing, path)
	}
	if err != nil {
		return nil, err
	}
	if err := fsutil.CheckPrivate(path); err != nil {
		return nil, err
	}
	var w fileWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("masterkey: %s: %w", path, err)
	}
	if w.Contract != Contract || !w.KeyID.Valid() {
		return nil, fmt.Errorf("masterkey: %s is not a %s file", path, Contract)
	}
	material, err := base64.StdEncoding.DecodeString(w.Key)
	if err != nil || len(material) != keyLen {
		return nil, fmt.Errorf("masterkey: %s: key must be %d bytes of base64", path, keyLen)
	}
	created, err := clock.Parse(w.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("masterkey: %s: %w", path, err)
	}
	return &Key{id: w.KeyID, material: material, createdAt: created}, nil
}

func (k *Key) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.material)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal 加密 plaintext；aad 必须绑定用途与所属对象（例如因子 ID 与主体 ID），
// 解密时给出同样的 aad 才能打开。
func (k *Key) Seal(plaintext, aad []byte) ([]byte, error) {
	g, err := k.aead()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+g.NonceSize(), 1+g.NonceSize()+len(plaintext)+g.Overhead())
	out[0] = sealVersion
	if _, err := io.ReadFull(rand.Reader, out[1:]); err != nil {
		return nil, fmt.Errorf("masterkey: nonce: %w", err)
	}
	return g.Seal(out, out[1:], plaintext, aad), nil
}

// Open 解密 Seal 的结果；密文、附加数据或密钥任何不符都返回 ErrDecrypt。
func (k *Key) Open(sealed, aad []byte) ([]byte, error) {
	g, err := k.aead()
	if err != nil {
		return nil, err
	}
	if len(sealed) < 1+g.NonceSize()+g.Overhead() || sealed[0] != sealVersion {
		return nil, ErrDecrypt
	}
	nonce := sealed[1 : 1+g.NonceSize()]
	pt, err := g.Open(nil, nonce, sealed[1+g.NonceSize():], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// Derive 按标签派生 32 字节子密钥（HKDF-SHA256）。不同标签的子密钥相互独立，
// 子密钥泄露不暴露主密钥。
func (k *Key) Derive(label string) ([]byte, error) {
	return hkdf.Key(sha256.New, k.material, []byte(k.id), "lantai/"+label, keyLen)
}
