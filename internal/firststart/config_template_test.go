//go:build linux || darwin

package firststart

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedConfigTemplateOwnedFileWithReadOnlyParent(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, "data")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(t.TempDir(), "template.yaml")
	raw := []byte("contract: lantai.config/v1\nhttp:\n  allowed_origins: [https://localhost:9443]\n")
	if err := os.WriteFile(template, raw, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	if err := SeedConfigTemplate(home, template); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "config.yaml")
	got, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("template not copied intact", err)
	}
	st, err := os.Lstat(config)
	if err != nil || st.Mode().Perm() != 0600 || !privateMetadata(st) {
		t.Fatal("configuration is not a private service-owned file", err)
	}
	// Recovery removes/replaces the owned file, while the read-only deployment
	// template is unchanged. No write to the data volume's parent is needed.
	if err = os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if got, err = os.ReadFile(template); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("read-only template changed", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 1 || entries[0].Name() != "lantai.lock" {
		t.Fatal("bootstrap created business data", err)
	}
}

func TestSeedConfigTemplatePreservesExistingOrInterruptedState(t *testing.T) {
	t.Run("existing configuration", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "config.yaml")
		raw := []byte("contract: lantai.config/v1\nhttp:\n  allowed_origins: [https://localhost:9443]\n")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := SeedConfigTemplate(home, filepath.Join(t.TempDir(), "missing")); err != nil {
			t.Fatal("existing instance must not depend on its old template", err)
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
			t.Fatal("existing configuration overwritten")
		}
	})
	t.Run("restore marker without config", func(t *testing.T) {
		home := t.TempDir()
		marker := filepath.Join(home, "instance.json")
		if err := os.WriteFile(marker, []byte("retained restore intent"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := SeedConfigTemplate(home, "missing-template"); err == nil {
			t.Fatal("ambiguous restore marker silently accepted")
		}
		if _, err := os.Lstat(filepath.Join(home, "config.yaml")); !os.IsNotExist(err) {
			t.Fatal("missing restore configuration was recreated")
		}
		if got, _ := os.ReadFile(marker); string(got) != "retained restore intent" {
			t.Fatal("restore state changed")
		}
	})
}

func TestSeedConfigTemplateRejectsUnsafeOrInvalidInputs(t *testing.T) {
	for _, kind := range []string{"source link", "target link", "home link", "shared write", "invalid schema", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			template := filepath.Join(t.TempDir(), "template.yaml")
			if err := os.WriteFile(template, []byte("contract: lantai.config/v1\n"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "source link":
				link := template + ".link"
				if err := os.Symlink(template, link); err != nil {
					t.Fatal(err)
				}
				template = link
			case "target link":
				if err := os.Symlink(template, filepath.Join(home, "config.yaml")); err != nil {
					t.Fatal(err)
				}
			case "home link":
				link := filepath.Join(t.TempDir(), "home")
				if err := os.Symlink(home, link); err != nil {
					t.Fatal(err)
				}
				home = link
			case "shared write":
				if err := os.Chmod(home, 0770); err != nil {
					t.Fatal(err)
				}
			case "invalid schema":
				if err := os.WriteFile(template, []byte("contract: lantai.config/v1\npassword: forbidden\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(template, []byte(strings.Repeat("x", (64<<10)+1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := SeedConfigTemplate(home, template); err == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
}
