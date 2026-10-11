package deploy_test

import (
	"os"
	"path/filepath"
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
			Privileged  bool
			Networks    []string
			Volumes     []string
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
	for _, s := range c.Services {
		if s.TTY || len(s.Profiles) != 0 {
			t.Fatal("default startup must not depend on profiles or a TTY")
		}
	}
}
