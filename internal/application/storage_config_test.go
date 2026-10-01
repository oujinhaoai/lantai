package application

import (
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// 实例配置的上传限额传给存储模块；调用方显式给出的值优先，都未设置时留给
// 存储模块取默认值。
func TestStorageConfigTakesUploadLimitsFromInstanceConfig(t *testing.T) {
	cfg := operations.Config{MinFreeBytes: 1 << 20, Uploads: operations.UploadLimits{MaxFileBytes: 1000, MaxUploadBytes: 4000, MaxUploadFiles: 3, IdleExpiry: 2 * time.Minute, AbsoluteExpiry: 10 * time.Minute}}
	got := storageConfig(storage.Config{}, cfg)
	if got.MinFreeBytes != 1<<20 || got.MaxFileBytes != 1000 || got.MaxUploadBytes != 4000 || got.MaxUploadFiles != 3 || got.UploadIdleTTL != 2*time.Minute || got.UploadMaxTTL != 10*time.Minute {
		t.Fatalf("instance limits not applied: %+v", got)
	}
	got = storageConfig(storage.Config{MaxUploadFiles: 9, UploadMaxTTL: time.Hour}, cfg)
	if got.MaxUploadFiles != 9 || got.UploadMaxTTL != time.Hour || got.MaxFileBytes != 1000 {
		t.Fatalf("explicit options must win: %+v", got)
	}
	if got = storageConfig(storage.Config{}, operations.Config{}); got.MaxFileBytes != 0 || got.MaxUploadFiles != 0 || got.UploadIdleTTL != 0 {
		t.Fatalf("unset limits must stay zero for storage defaults: %+v", got)
	}
}
