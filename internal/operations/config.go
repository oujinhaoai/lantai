package operations

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

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
	// Uploads 是上传限额与会话到期；零值表示采用存储模块默认值。
	Uploads UploadLimits
	// Present 表示 config.yaml 存在。
	Present   bool
	Listen    ListenConfig
	HTTP      HTTPConfig
	Transfer  TransferConfig
	Lifecycle LifecycleConfig
}

// UploadLimits 是 config.yaml storage 下的上传限额与会话到期。
type UploadLimits struct {
	MaxFileBytes   int64
	MaxUploadBytes int64
	MaxUploadFiles int
	IdleExpiry     time.Duration
	AbsoluteExpiry time.Duration
}

// LifecycleConfig enables the T08 due-purge/GC scheduler. It is off by default:
// automatic deletion starts only by an explicit deployment decision.
type LifecycleConfig struct {
	Scheduler       bool `json:"scheduler"`
	IntervalSeconds int  `json:"interval_seconds"`
	Batch           int  `json:"batch"`
}

// ListenConfig 的地址都是核心内部监听，不是客户端传输 URL。公网 TLS 与路径
// 转发由网关负责；默认只绑定本机。Merged 仅供开发时复用 API 监听。
type ListenConfig struct {
	API        string `json:"api"`
	Transfer   string `json:"transfer"`
	Operations string `json:"operations"`
	Merged     bool   `json:"merged"`
}

type HTTPConfig struct {
	AllowedOrigins    []string `json:"allowed_origins"`
	MaxJSONBytes      int64    `json:"max_json_bytes"`
	APITimeoutSeconds int      `json:"api_timeout_seconds"`
}

type TransferConfig struct {
	InteractiveSlots                    int   `json:"interactive_slots"`
	BatchSlots                          int   `json:"batch_slots"`
	BatchPerPrincipal                   int   `json:"batch_per_principal"`
	BatchBytesPerSecond                 int64 `json:"batch_bytes_per_second"`
	BatchBytesPerSecondWhileInteractive int64 `json:"batch_bytes_per_second_while_interactive"`
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
		MinFreeBytes          *uint64 `json:"min_free_bytes"`
		MaxFileBytes          int64   `json:"max_file_bytes"`
		MaxUploadBytes        int64   `json:"max_upload_bytes"`
		MaxUploadFiles        int     `json:"max_upload_files"`
		IdleExpirySeconds     int64   `json:"upload_idle_expiry_seconds"`
		AbsoluteExpirySeconds int64   `json:"upload_absolute_expiry_seconds"`
	} `json:"storage"`
	Listen    ListenConfig    `json:"listen"`
	HTTP      HTTPConfig      `json:"http"`
	Transfer  TransferConfig  `json:"transfer"`
	Lifecycle LifecycleConfig `json:"lifecycle"`
}

// LoadConfig 读取并校验数据根下的 config.yaml；文件不存在时返回默认配置。
func LoadConfig(l Layout) (Config, error) {
	cfg := Config{
		InstanceName: DefaultInstanceName,
		SecretsDir:   filepath.Join(l.Home, "secrets"),
		MinFreeBytes: DefaultMinFreeBytes,
		Listen:       ListenConfig{API: "127.0.0.1:8080", Transfer: "127.0.0.1:8081", Operations: "127.0.0.1:9090"},
		HTTP:         HTTPConfig{MaxJSONBytes: 8 << 20, APITimeoutSeconds: 30},
		Transfer:     TransferConfig{InteractiveSlots: 4, BatchSlots: 8, BatchPerPrincipal: 4},
		Lifecycle:    LifecycleConfig{IntervalSeconds: 300, Batch: 100},
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
	w := configWire{Listen: cfg.Listen, HTTP: cfg.HTTP, Transfer: cfg.Transfer, Lifecycle: cfg.Lifecycle}
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
	// 取值范围由 schema 校验；这里只拒绝两项同时设置且互相矛盾的组合，
	// 只设一项时由存储模块按较小者生效。
	st := w.Storage
	if st.MaxFileBytes > 0 && st.MaxUploadBytes > 0 && st.MaxFileBytes > st.MaxUploadBytes {
		return cfg, fmt.Errorf("operations: %s: storage.max_file_bytes (%d) exceeds storage.max_upload_bytes (%d)", l.ConfigPath(), st.MaxFileBytes, st.MaxUploadBytes)
	}
	if st.IdleExpirySeconds > 0 && st.AbsoluteExpirySeconds > 0 && st.IdleExpirySeconds > st.AbsoluteExpirySeconds {
		return cfg, fmt.Errorf("operations: %s: storage.upload_idle_expiry_seconds (%d) exceeds storage.upload_absolute_expiry_seconds (%d)", l.ConfigPath(), st.IdleExpirySeconds, st.AbsoluteExpirySeconds)
	}
	cfg.Uploads = UploadLimits{MaxFileBytes: st.MaxFileBytes, MaxUploadBytes: st.MaxUploadBytes, MaxUploadFiles: st.MaxUploadFiles,
		IdleExpiry: time.Duration(st.IdleExpirySeconds) * time.Second, AbsoluteExpiry: time.Duration(st.AbsoluteExpirySeconds) * time.Second}
	cfg.Listen, cfg.HTTP, cfg.Transfer, cfg.Lifecycle = w.Listen, w.HTTP, w.Transfer, w.Lifecycle
	return cfg, nil
}
