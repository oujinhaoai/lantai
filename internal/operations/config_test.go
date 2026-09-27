package operations

import (
	"os"
	"testing"
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
