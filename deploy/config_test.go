package deploy_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/oujinhaoai/lantai/internal/operations"
	"go.yaml.in/yaml/v3"
)

func TestCoreTemplatesUseCurrentConfigurationContract(t *testing.T) {
	for _, name := range []string{"config.native.yaml", "config.container.yaml"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), b, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := operations.LoadConfig(operations.Layout{Home: dir})
			if err != nil {
				t.Fatal(err)
			}
			if c.Listen.Operations != "127.0.0.1:9090" || c.Listen.Merged {
				t.Fatal("template exposes operations or merges stream and JSON listeners")
			}
		})
	}
}

func TestComposeOnlyPublishesGateway(t *testing.T) {
	b, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Services map[string]struct {
			Ports       []string
			Command     []string
			Privileged  bool
			Networks    []string
			Volumes     []string
			Tmpfs       []string
			NetworkMode string `yaml:"network_mode"`
			User        string
			ReadOnly    bool `yaml:"read_only"`
			Profiles    []string
			TTY         bool
			DependsOn   map[string]struct{ Condition string } `yaml:"depends_on"`
		}
		Networks map[string]struct{ Internal bool }
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Services) != 3 || len(c.Services["core"].Ports) != 0 || len(c.Services["gateway"].Ports) != 1 || c.Services["core"].Privileged || !c.Networks["backend"].Internal {
		t.Fatal("container topology publishes core or adds another public service")
	}
	p := c.Services["prepare"]
	if len(p.Ports) != 0 || p.NetworkMode != "none" || p.Privileged || !p.ReadOnly || p.User != "0:0" {
		t.Fatal("volume preparation must be an isolated, nonprivileged, read-only helper")
	}
	if c.Services["core"].DependsOn["prepare"].Condition != "service_completed_successfully" {
		t.Fatal("core must wait for successful volume preparation")
	}
	core := c.Services["core"]
	if !slices.Contains(core.Command, "--config-template") || !slices.Contains(core.Command, "/etc/lantai/config.template.yaml") {
		t.Fatal("core must seed an owned configuration from the read-only template")
	}
	for _, mount := range core.Volumes {
		if mount == "${LANTAI_CONFIG_FILE:?set the reviewed config.container.yaml}:/var/lib/lantai/config.yaml:ro" {
			t.Fatal("recovery cannot replace a read-only configuration bind mount")
		}
	}
	if !slices.Contains(core.Volumes, "${LANTAI_CONFIG_FILE:?set the reviewed config.container.yaml}:/etc/lantai/config.template.yaml:ro") {
		t.Fatal("deployment template must remain read-only outside the data volume")
	}
	if n := c.Services["core"].Networks; len(n) != 1 || n[0] != "backend" {
		t.Fatal("core must remain only on the internal backend")
	}
	if n := c.Services["gateway"].Networks; len(n) != 2 || !slices.Contains(n, "edge") || !slices.Contains(n, "backend") || c.Networks["edge"].Internal {
		t.Fatal("gateway must bridge the publishable edge and internal backend")
	}
	for _, name := range []string{"core", "gateway"} {
		mounts := c.Services[name].Tmpfs
		if len(mounts) != 1 || mounts[0] != "/tmp:rw,noexec,nosuid,nodev,mode=1777,size=64m" {
			t.Fatalf("%s tmpfs options must parse as one bounded private mount, got %v", name, mounts)
		}
	}
	for _, s := range c.Services {
		if s.TTY || len(s.Profiles) != 0 {
			t.Fatal("default startup must not depend on profiles or a TTY")
		}
	}
}
