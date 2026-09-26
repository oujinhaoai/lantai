package operations

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
)

// ConfigContract 是 config.yaml 的契约标识。
const ConfigContract = "lantai.config/v1"

// DefaultMinFreeBytes 是数据根所在文件系统的默认最低可用空间。
const DefaultMinFreeBytes uint64 = 1 << 30

// DefaultInstanceName 是未配置名称时初始化使用的实例名。
const DefaultInstanceName = "lantai"

// Config 是解析后的服务配置。配置里不放密钥与凭据。
type Config struct {
	// InstanceName 只在初始化时写入实例标记。
	InstanceName string
	// SecretsDir 是密钥材料目录的绝对路径。
	SecretsDir string
	// MinFreeBytes 低于它时实例不开放写入。
	MinFreeBytes uint64
	// Present 表示 config.yaml 存在。
	Present bool
}

type configWire struct {
	Contract string `json:"contract"`
	Instance struct {
		Name string `json:"name"`
	} `json:"instance"`
	Secrets struct {
		Dir string `json:"dir"`
	} `json:"secrets"`
	Storage struct {
		MinFreeBytes *uint64 `json:"min_free_bytes"`
	} `json:"storage"`
}

// LoadConfig 读取并校验数据根下的 config.yaml；文件不存在时返回默认配置。
func LoadConfig(l Layout) (Config, error) {
	cfg := Config{
		InstanceName: DefaultInstanceName,
		SecretsDir:   filepath.Join(l.Home, "secrets"),
		MinFreeBytes: DefaultMinFreeBytes,
	}
	raw, err := os.ReadFile(l.ConfigPath())
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("operations: read config: %w", err)
	}
	doc, err := yamljson.Decode(raw)
	if err != nil {
		return cfg, fmt.Errorf("operations: %s: %w", l.ConfigPath(), err)
	}
	reg, err := schema.Default()
	if err != nil {
		return cfg, err
	}
	if err := reg.Validate(ConfigContract, doc); err != nil {
		return cfg, fmt.Errorf("operations: %s: %w", l.ConfigPath(), err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return cfg, err
	}
	var w configWire
	if err := json.Unmarshal(b, &w); err != nil {
		return cfg, fmt.Errorf("operations: %s: %w", l.ConfigPath(), err)
	}
	cfg.Present = true
	if w.Instance.Name != "" {
		cfg.InstanceName = w.Instance.Name
	}
	if w.Secrets.Dir != "" {
		dir := filepath.FromSlash(w.Secrets.Dir)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(l.Home, dir)
		}
		cfg.SecretsDir = filepath.Clean(dir)
	}
	if w.Storage.MinFreeBytes != nil {
		cfg.MinFreeBytes = *w.Storage.MinFreeBytes
	}
	return cfg, nil
}
