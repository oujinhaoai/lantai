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
			Ports      []string
			Privileged bool
			Networks   []string
			Volumes    []string
		}
		Networks map[string]struct{ Internal bool }
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Services) != 2 || len(c.Services["core"].Ports) != 0 || len(c.Services["gateway"].Ports) != 1 || c.Services["core"].Privileged || !c.Networks["backend"].Internal {
		t.Fatal("container topology publishes core or adds another public service")
	}
}
