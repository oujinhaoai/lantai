package operations

import (
	"os"
	"testing"
	"time"
)

func TestListenerConfigurationPreservesIndependentDefaults(t *testing.T) {
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(l.ConfigPath(), []byte("contract: lantai.config/v1\nlisten:\n  api: '127.0.0.1:8180'\n  merged: true\nhttp:\n  max_json_bytes: 4096\ntransfer:\n  batch_bytes_per_second: 1048576\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen.API != "127.0.0.1:8180" || cfg.Listen.Transfer != "127.0.0.1:8081" || cfg.Listen.Operations != "127.0.0.1:9090" || !cfg.Listen.Merged || cfg.HTTP.MaxJSONBytes != 4096 || cfg.HTTP.APITimeoutSeconds != 30 || cfg.Transfer.InteractiveSlots != 4 || cfg.Transfer.BatchBytesPerSecond != 1048576 {
		t.Fatalf("lost configuration defaults: %+v", cfg)
	}
	for _, bad := range []string{"http:\n  max_json_bytes: -1\n", "transfer:\n  interactive_slots: 0\n", "listen:\n  public_transfer: true\n"} {
		if err = os.WriteFile(l.ConfigPath(), []byte("contract: lantai.config/v1\n"+bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadConfig(l); err == nil {
			t.Fatalf("accepted invalid configuration %q", bad)
		}
	}
}

// 上传限额与会话到期可在 storage 下配置；越界由 schema 拒绝，两项同时设置
// 且互相矛盾时也拒绝（BUG-20260930-02）。
func TestUploadLimitsFromConfig(t *testing.T) {
	l, err := NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(l)
	if err != nil || cfg.Uploads != (UploadLimits{}) {
		t.Fatalf("default upload limits must be left to storage: %+v %v", cfg.Uploads, err)
	}
	write := func(storage string) {
		t.Helper()
		if err := os.WriteFile(l.ConfigPath(), []byte("contract: lantai.config/v1\nstorage:\n"+storage), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("  max_file_bytes: 1000\n  max_upload_bytes: 4000\n  max_upload_files: 3\n  upload_idle_expiry_seconds: 120\n  upload_absolute_expiry_seconds: 600\n")
	if cfg, err = LoadConfig(l); err != nil {
		t.Fatal(err)
	}
	want := UploadLimits{MaxFileBytes: 1000, MaxUploadBytes: 4000, MaxUploadFiles: 3, IdleExpiry: 2 * time.Minute, AbsoluteExpiry: 10 * time.Minute}
	if cfg.Uploads != want || cfg.MinFreeBytes != DefaultMinFreeBytes {
		t.Fatalf("upload limits = %+v, min free %d", cfg.Uploads, cfg.MinFreeBytes)
	}
	write("  max_upload_files: 7\n")
	if cfg, err = LoadConfig(l); err != nil || cfg.Uploads != (UploadLimits{MaxUploadFiles: 7}) {
		t.Fatalf("partial upload limits = %+v %v", cfg.Uploads, err)
	}
	for _, bad := range []string{
		"  max_file_bytes: 0\n",
		"  max_upload_files: 1000001\n",
		"  upload_idle_expiry_seconds: 59\n",
		"  upload_absolute_expiry_seconds: 2592001\n",
		"  max_file_bytes: 5000\n  max_upload_bytes: 4000\n",
		"  upload_idle_expiry_seconds: 700\n  upload_absolute_expiry_seconds: 600\n",
		"  max_part_bytes: 1048576\n",
	} {
		write(bad)
		if _, err = LoadConfig(l); err == nil {
			t.Fatalf("accepted invalid storage configuration %q", bad)
		}
	}
}
