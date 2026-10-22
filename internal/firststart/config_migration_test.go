//go:build linux || darwin

package firststart

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

func activeMigrationFixture(t *testing.T) (operations.Options, string, string, []byte) {
	t.Helper()
	opts, setup, _ := fixture(t)
	s := openSession(t, opts, setup)
	commitAdmin(t, s)
	if err := s.finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(opts.Home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(t.TempDir(), "legacy.yaml")
	if err = os.WriteFile(template, raw, 0400); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(opts.Home, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	return opts, setup, template, raw
}
func migrationStateHashes(t *testing.T, opts operations.Options, raw []byte) map[string][32]byte {
	t.Helper()
	l, _ := operations.NewLayout(opts.Home)
	cfg, err := operations.ParseConfig(l, raw)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{l.MarkerPath(), masterkey.Path(cfg.SecretsDir)}
	for _, db := range []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime, ownership.Events, ownership.Index} {
		paths = append(paths, l.DBPath(db))
	}
	out := map[string][32]byte{}
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		out[p] = sha256.Sum256(b)
	}
	return out
}
func assertMigrationHashes(t *testing.T, before map[string][32]byte) {
	t.Helper()
	for p, h := range before {
		b, err := os.ReadFile(p)
		if err != nil || sha256.Sum256(b) != h {
			t.Fatalf("existing data changed: %s %v", p, err)
		}
	}
}

func TestSeedLegacyConfigMigrationRetainsIdentityKeyAndDatabases(t *testing.T) {
	opts, setup, template, raw := activeMigrationFixture(t)
	before := migrationStateHashes(t, opts, raw)
	if err := SeedConfigTemplate(opts.Home, template); err != nil {
		t.Fatal(err)
	}
	assertMigrationHashes(t, before)
	p := filepath.Join(opts.Home, "config.yaml")
	got, _ := os.ReadFile(p)
	if !bytes.Equal(got, raw) {
		t.Fatal("effective legacy config changed")
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0600 || !privateMetadata(st) {
		t.Fatal("configuration permissions", st)
	}
	// Same command retries with no template: existing valid config always wins.
	if err := SeedConfigTemplate(opts.Home, template+".missing"); err != nil {
		t.Fatal(err)
	}
	assertMigrationHashes(t, before)
	if e, err := OpenEntry(t.Context(), opts, setup); err != nil || e != nil {
		t.Fatal("setup reopened", err)
	}
	a, err := application.Open(t.Context(), application.Options{Instance: opts})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSeedLegacyConfigMigrationFailsBeforeAdoption(t *testing.T) {
	for _, kind := range []string{"missing_template", "wrong_key", "missing_key", "mismatched_database", "invalid_config"} {
		t.Run(kind, func(t *testing.T) {
			opts, _, template, raw := activeMigrationFixture(t)
			l, _ := operations.NewLayout(opts.Home)
			cfg, _ := operations.ParseConfig(l, raw)
			if err := os.Chmod(template, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing_template":
				template += ".missing"
			case "missing_key":
				if err := os.Rename(masterkey.Path(cfg.SecretsDir), masterkey.Path(cfg.SecretsDir)+".retained"); err != nil {
					t.Fatal(err)
				}
			case "wrong_key":
				wrong := t.TempDir()
				if err := os.Chmod(wrong, 0700); err != nil {
					t.Fatal(err)
				}
				s, _, _ := fixture(t)
				inst, err := operations.Create(t.Context(), operations.CreateOptions{Options: s})
				if err != nil {
					t.Fatal(err)
				}
				k, err := masterkey.Generate(inst.IDs(), inst.Clock(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = masterkey.Save(wrong, k); err != nil {
					t.Fatal(err)
				}
				_ = inst.Close(context.Background())
				changed := bytes.ReplaceAll(raw, []byte(cfg.SecretsDir), []byte(wrong))
				if err = os.WriteFile(template, changed, 0400); err != nil {
					t.Fatal(err)
				}
			case "mismatched_database":
				db, err := sqlite.Open(t.Context(), l.DBPath(ownership.Main), sqlite.Options{})
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.ExecContext(t.Context(), "UPDATE instance_binding SET instance_id='different-instance'")
				if err != nil {
					t.Fatal(err)
				}
				_ = db.Close()
			case "invalid_config":
				if err := os.WriteFile(template, []byte("contract: unknown\n"), 0400); err != nil {
					t.Fatal(err)
				}
			}
			markerBefore, _ := os.ReadFile(l.MarkerPath())
			keyBefore, _ := os.ReadFile(masterkey.Path(cfg.SecretsDir))
			if err := SeedConfigTemplate(opts.Home, template); err == nil {
				t.Fatal("unsafe legacy adoption succeeded")
			}
			if _, err := os.Lstat(l.ConfigPath()); !os.IsNotExist(err) {
				t.Fatal("failed preflight installed config", err)
			}
			markerAfter, _ := os.ReadFile(l.MarkerPath())
			keyAfter, _ := os.ReadFile(masterkey.Path(cfg.SecretsDir))
			if !bytes.Equal(markerBefore, markerAfter) || !bytes.Equal(keyBefore, keyAfter) {
				t.Fatal("failed preflight modified identity/key")
			}
			if _, err := os.Lstat(filepath.Join(opts.Home, configBootstrapDir)); !os.IsNotExist(err) {
				t.Fatal("preflight created transaction", err)
			}
		})
	}
}

const seedHelperEnv = "LANTAI_TEST_SEED_CRASH"

func TestSeedConfigCrashHelper(t *testing.T) {
	if os.Getenv(seedHelperEnv) == "" {
		t.Skip("child-only seed interruption helper")
	}
	err := seedConfigTemplate(os.Getenv("LANTAI_TEST_SEED_HOME"), os.Getenv("LANTAI_TEST_SEED_INPUT"), func(stage seedStage) error {
		if string(stage) == os.Getenv(seedHelperEnv) {
			fmt.Println("SEED-CRASH-READY")
			time.Sleep(5 * time.Minute)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func killSeedAt(t *testing.T, home, input string, stage seedStage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSeedConfigCrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), seedHelperEnv+"="+string(stage), "LANTAI_TEST_SEED_HOME="+home, "LANTAI_TEST_SEED_INPUT="+input)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	sc := bufio.NewScanner(pipe)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "SEED-CRASH-READY") {
			ready = true
			break
		}
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("child did not reach actual crash boundary", stderr.String())
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
}
func TestSeedConfigProcessDeathBeforeAndAfterPublication(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, stage := range []seedStage{seedAfterFileSync, seedAfterPublish} {
			t.Run(fmt.Sprintf("legacy_%v_%s", legacy, stage), func(t *testing.T) {
				var opts operations.Options
				var setup, template string
				var raw []byte
				var before map[string][32]byte
				if legacy {
					opts, setup, template, raw = activeMigrationFixture(t)
					before = migrationStateHashes(t, opts, raw)
				} else {
					opts, setup, _ = fixture(t)
					raw, _ = os.ReadFile(filepath.Join(opts.Home, "config.yaml"))
					template = filepath.Join(t.TempDir(), "input.yaml")
					if err := os.WriteFile(template, raw, 0400); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(opts.Home, "config.yaml")); err != nil {
						t.Fatal(err)
					}
				}
				killSeedAt(t, opts.Home, template, stage)
				target := filepath.Join(opts.Home, "config.yaml")
				if stage == seedAfterFileSync {
					if _, err := os.Lstat(target); !os.IsNotExist(err) {
						t.Fatal("premature publication", err)
					}
				} else {
					got, err := os.ReadFile(target)
					if err != nil || !bytes.Equal(got, raw) {
						t.Fatal("incomplete published configuration", err)
					}
				}
				if err := SeedConfigTemplate(opts.Home, template); err != nil {
					t.Fatal("automatic retry", err)
				}
				if _, err := os.Lstat(filepath.Join(opts.Home, configBootstrapDir)); !os.IsNotExist(err) {
					t.Fatal("transaction retained after retry", err)
				}
				got, _ := os.ReadFile(target)
				if !bytes.Equal(got, raw) {
					t.Fatal("retry changed config")
				}
				st, _ := os.Stat(target)
				if st.Mode().Perm() != 0600 || !privateMetadata(st) {
					t.Fatal("retry permissions", st)
				}
				if legacy {
					assertMigrationHashes(t, before)
				}
				e, err := OpenEntry(t.Context(), opts, setup)
				if err != nil {
					t.Fatal("startup after retry", err)
				}
				if legacy && e != nil {
					t.Fatal("upgrade reopened setup")
				}
				if !legacy && e == nil {
					t.Fatal("new installation not resumable")
				}
				if e != nil {
					_ = e.Close()
				}
			})
		}
	}
}

func TestSeedBootstrapPartialWriteAndUnknownState(t *testing.T) {
	for _, kind := range []string{"prefix", "mismatch", "unknown_entry", "old_temp", "ordinary_error", "changed_input"} {
		t.Run(kind, func(t *testing.T) {
			opts, _, _ := fixture(t)
			path := filepath.Join(opts.Home, "config.yaml")
			raw, _ := os.ReadFile(path)
			_ = os.Remove(path)
			template := filepath.Join(t.TempDir(), "input.yaml")
			_ = os.WriteFile(template, raw, 0400)
			tx := filepath.Join(opts.Home, configBootstrapDir)
			if kind != "old_temp" && kind != "ordinary_error" {
				if err := os.Mkdir(tx, 0700); err != nil {
					t.Fatal(err)
				}
			}
			keep := ""
			switch kind {
			case "prefix":
				_ = os.WriteFile(configPartPath(tx, raw), raw[:7], 0600)
			case "changed_input":
				keep = configPartPath(tx, raw)
				_ = os.WriteFile(keep, raw[:7], 0600)
				_ = os.Chmod(template, 0600)
				_ = os.WriteFile(template, append(raw, []byte("# changed input\n")...), 0400)
			case "mismatch":
				keep = configPartPath(tx, raw)
				_ = os.WriteFile(keep, []byte("unknown bytes"), 0600)
			case "unknown_entry":
				keep = filepath.Join(tx, "valuable")
				_ = os.WriteFile(keep, []byte("unknown bytes"), 0600)
			case "old_temp":
				keep = filepath.Join(opts.Home, ".config.yaml.tmp-123456789")
				_ = os.WriteFile(keep, raw, 0600)
			}
			if kind == "ordinary_error" {
				fault := errors.New("injected before publication")
				err := seedConfigTemplate(opts.Home, template, func(seedStage) error { return fault })
				if !errors.Is(err, fault) {
					t.Fatal(err)
				}
				if _, err = os.Lstat(tx); !os.IsNotExist(err) {
					t.Fatal("normal error did not clean bootstrap temp", err)
				}
				if _, err = os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("normal prepublication error published config", err)
				}
				if err = SeedConfigTemplate(opts.Home, template); err != nil {
					t.Fatal(err)
				}
				return
			}
			err := SeedConfigTemplate(opts.Home, template)
			if kind == "prefix" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("unknown state adopted")
				}
				if _, e := os.Lstat(keep); e != nil {
					t.Fatal("unknown file removed", e)
				}
			}
		})
	}
}

func TestSeedConfigRestoreAndInitializingOwnership(t *testing.T) {
	for _, kind := range []string{"restore", "initializing"} {
		t.Run(kind, func(t *testing.T) {
			opts, _, template, _ := activeMigrationFixture(t)
			l, _ := operations.NewLayout(opts.Home)
			m, err := operations.ReadMarker(l)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "restore" {
				m.RecoveryEpoch = 2
				m.Restore = &operations.RestoreRun{RunID: ids.New(), BackupID: ids.New(), ManifestDigest: digest.Of([]byte("synthetic")), Stage: "copying", SourceEpoch: 1, StartedAt: clock.Format(time.Now()), SourceKeyID: ids.New()}
			} else {
				m.State = operations.MarkerInitializing
			}
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = operations.ParseMarker(b); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(l.MarkerPath(), b, 0600); err != nil {
				t.Fatal(err)
			}
			err = SeedConfigTemplate(opts.Home, template)
			if kind == "restore" && err != nil {
				t.Fatal("restore owner blocked", err)
			}
			if kind == "initializing" && err == nil {
				t.Fatal("initializing marker treated as active upgrade")
			}
			if _, err = os.Lstat(l.ConfigPath()); !os.IsNotExist(err) {
				t.Fatal("template installed into lifecycle-owned state", err)
			}
			got, _ := os.ReadFile(l.MarkerPath())
			if !bytes.Equal(b, got) {
				t.Fatal("lifecycle marker changed")
			}
		})
	}
}

func TestSeedPublishedLegacyWitnessRequiresActiveMarkerBeforeCleanup(t *testing.T) {
	opts, _, _ := fixture(t)
	config := filepath.Join(opts.Home, "config.yaml")
	raw, _ := os.ReadFile(config)
	tx := filepath.Join(opts.Home, configBootstrapDir)
	if err := os.Mkdir(tx, 0700); err != nil {
		t.Fatal(err)
	}
	part := configPartPath(tx, raw)
	if err := os.Link(config, part); err != nil {
		t.Fatal(err)
	}
	witness := filepath.Join(tx, "legacy.empty")
	if err := os.WriteFile(witness, nil, 0600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = finishPublishedBootstrap(operations.Layout{Home: opts.Home}, tx, st, raw); err == nil {
		t.Fatal("legacy witness accepted without active marker")
	}
	for _, p := range []string{config, part, witness} {
		if _, err = os.Lstat(p); err != nil {
			t.Fatal("file removed before context check", p, err)
		}
	}
}
