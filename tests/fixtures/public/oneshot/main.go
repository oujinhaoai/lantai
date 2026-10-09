// Command oneshot is a synthetic file-protocol plugin used only by tests. Its
// behavior is selected by the frozen package config ("mode") or, for CLI
// commands, the first argument, so hosts can be exercised against legal
// results and every runtime fault class. It reads no Lantai credentials.
package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/oujinhaoai/lantai/sdk/go/extension"
)

type config struct {
	Mode      string `json:"mode"`
	SleepMS   int    `json:"sleep_ms"`
	Heartbeat string `json:"heartbeat"`
}

func main() {
	if hb := os.Getenv("FIXTURE_HEARTBEAT"); hb != "" {
		heartbeat(hb)
		return
	}
	extension.Main(run)
}

func run(r *extension.Run) error {
	if r.Contract() == extension.CommandInputContract {
		return command(r)
	}
	j, err := r.Processor()
	if err != nil {
		return err
	}
	var c config
	if len(j.Config) > 0 {
		if err = json.Unmarshal(j.Config, &c); err != nil {
			return err
		}
	}
	if j.Mode == "probe" {
		switch c.Mode {
		case "probe_unknown_field", "probe_unknown_producer_field":
			if err = r.FinishProcessor(extension.ProcessorResult{OperationID: j.OperationID, Status: "completed", Producer: *j.Producer}); err != nil {
				return err
			}
			return injectForbiddenField(r, c)
		case "probe_checks":
			return r.FinishProcessor(extension.ProcessorResult{OperationID: j.OperationID, Status: "completed", Producer: *j.Producer, Checks: []extension.Check{{Contract: extension.CheckResultContract, Ref: extension.Ref{InstanceID: j.OperationID, AssetID: j.OperationID, VersionID: j.OperationID}, ManifestDigest: j.InputDigest, Verdict: "pass", Findings: []string{}, Producer: *j.Producer}}})
		case "probe_files":
			path, err := r.Output("unexpected.bin")
			if err != nil {
				return err
			}
			if err = os.WriteFile(path, []byte("synthetic"), 0600); err != nil {
				return err
			}
			return r.FinishProcessor(extension.ProcessorResult{OperationID: j.OperationID, Status: "completed", Producer: *j.Producer})
		case "probe_hang":
			c.Mode = "hang"
			return fault(r, c)
		case "probe_big_output":
			c.Mode = "big_output"
			return fault(r, c)
		case "probe_bad_schema":
			return os.WriteFile(outPath(r), []byte(`{"status":"pass"}`), 0600)
		case "probe_no_result":
			return nil
		case "probe_unsupported":
			return r.FinishProcessor(extension.ProcessorResult{OperationID: j.OperationID, Status: "unsupported", Producer: *j.Producer})
		case "probe_records":
			return r.FinishProcessor(extension.ProcessorResult{OperationID: j.OperationID, Status: "completed", Producer: *j.Producer, Records: []extension.Record{{Schema: "lantai.synthetic/v1", Payload: map[string]any{"unexpected": true}}}})
		case "probe_delay":
			time.Sleep(time.Duration(c.SleepMS) * time.Millisecond)
		}
	}
	if j.Mode == "probe" && c.Mode != "probe_crash" {
		return r.FinishProcessor(extension.ProcessorResult{OperationID: j.OperationID, Status: "completed", Producer: *j.Producer})
	}
	if err = fault(r, c); err != nil || c.Mode == "no_result" {
		return err
	}
	res := extension.ProcessorResult{OperationID: j.OperationID, Status: "completed", Producer: *j.Producer}
	switch c.Mode {
	case "unsupported":
		res.Status = "unsupported"
	case "bad_schema":
		return os.WriteFile(outPath(r), []byte(`{"contract":"lantai.processor-result/v1","status":"pass"}`), 0o600)
	default:
		for _, ref := range j.InputRefs {
			res.Checks = append(res.Checks, check(r, j, ref, c.Mode == "fail"))
		}
	}
	if c.Mode == "business" {
		res.Contract = extension.ProcessorResultContract
		res.Files = []extension.File{}
		res.Records = []extension.Record{}
		raw, err := json.Marshal(res)
		if err != nil {
			return err
		}
		var doc map[string]any
		if err = json.Unmarshal(raw, &doc); err != nil {
			return err
		}
		for _, x := range doc["checks"].([]any) {
			x.(map[string]any)["check_key"] = "media_structure"
		}
		raw, err = json.Marshal(doc)
		if err != nil {
			return err
		}
		return os.WriteFile(outPath(r), raw, 0600)
	}
	if err = r.FinishProcessor(res); err != nil {
		return err
	}
	switch c.Mode {
	case "unknown_field", "unknown_producer_field", "unknown_check_field":
		return injectForbiddenField(r, c)
	case "extra_file":
		p, _ := r.Output("undeclared.bin")
		return os.WriteFile(p, []byte("late"), 0o600)
	case "symlink":
		p, _ := r.Output("link")
		return os.Symlink(filepath.Dir(p), p)
	case "exit1":
		os.Exit(1)
	}
	return nil
}

// The SDK emits a valid result first. Deliberately alter its actual wire bytes
// afterward, so tests detect fields lost by permissive struct decoding.
func injectForbiddenField(r *extension.Run, c config) error {
	b, err := os.ReadFile(outPath(r))
	if err != nil {
		return err
	}
	var doc map[string]any
	if err = json.Unmarshal(b, &doc); err != nil {
		return err
	}
	switch c.Mode {
	case "probe_unknown_producer_field", "unknown_producer_field":
		doc["producer"].(map[string]any)["unexpected"] = true
	case "unknown_check_field":
		doc["checks"].([]any)[0].(map[string]any)["unexpected"] = true
	default:
		doc["unexpected_business_payload"] = map[string]any{"unexpected": true}
	}
	b, err = json.Marshal(doc)
	if err != nil {
		return err
	}
	if c.Heartbeat != "" {
		if err = os.WriteFile(c.Heartbeat, b, 0600); err != nil {
			return err
		}
	}
	return os.WriteFile(outPath(r), b, 0600)
}

// check compares the host-supplied manifest.yaml identity with the fixed input.
func check(r *extension.Run, j extension.ProcessorJob, ref extension.Ref, forceFail bool) extension.Check {
	c := extension.Check{Contract: extension.CheckResultContract, Ref: ref, ManifestDigest: j.InputDigest, Verdict: "fail", Findings: []string{"manifest_identity_mismatch"}, Producer: *j.Producer}
	raw, err := r.Input("manifest.yaml", 8<<20)
	var doc struct {
		InstanceID     string `yaml:"instance_id"`
		AssetID        string `yaml:"asset_id"`
		VersionID      string `yaml:"version_id"`
		ManifestDigest string `yaml:"manifest_digest"`
		Content        struct {
			Metadata map[string]any `yaml:"metadata"`
		} `yaml:"content"`
	}
	if err == nil && yaml.Unmarshal(raw, &doc) == nil && !forceFail && doc.InstanceID == ref.InstanceID && doc.AssetID == ref.AssetID && doc.VersionID == ref.VersionID && doc.ManifestDigest == j.InputDigest {
		c.Verdict, c.Findings = "pass", []string{}
	}
	if j.Mode != "probe" && len(j.Config) > 0 {
		var cfg config
		_ = json.Unmarshal(j.Config, &cfg)
		extra, _ := doc.Content.Metadata["extra"].(map[string]any)
		if cfg.Mode == "business" && extra["synthetic_valid"] != true {
			c.Verdict, c.Findings = "fail", []string{"synthetic_structure_invalid"}
		}
	}
	return c
}

func command(r *extension.Run) error {
	j, err := r.Command()
	if err != nil {
		return err
	}
	c := config{}
	if len(j.Args) > 0 {
		c.Mode = j.Args[0]
	}
	if len(j.Args) > 1 {
		c.Heartbeat = j.Args[1]
	}
	if err = fault(r, c); err != nil || c.Mode == "no_result" {
		return err
	}
	for _, in := range j.Inputs {
		b, err := r.Input(in.Name, 64<<20)
		if err != nil {
			return err
		}
		p, err := r.Output("copies/" + in.Name)
		if err != nil {
			return err
		}
		if err = os.WriteFile(p, []byte(strings.ToUpper(string(b))), 0o600); err != nil {
			return err
		}
	}
	summary := map[string]any{"inputs": len(j.Inputs)}
	if c.Mode == "inspect" {
		summary["args"] = j.Args
		names := []string{}
		for _, kv := range os.Environ() {
			names = append(names, strings.SplitN(kv, "=", 2)[0])
		}
		sort.Strings(names)
		summary["environment_names"] = names
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		actual, err := os.Stat(cwd)
		if err != nil {
			return err
		}
		expected, err := os.Stat(filepath.Dir(os.Args[1]))
		if err != nil {
			return err
		}
		// macOS aliases /var and /private/var; compare directory identity.
		summary["private_working_directory"] = os.SameFile(actual, expected)
	}
	return r.FinishCommand(extension.CommandResult{OperationID: j.OperationID, Command: j.Command, Status: "completed", Summary: summary, Producer: j.Producer})
}

func fault(r *extension.Run, c config) error {
	if c.SleepMS > 0 {
		time.Sleep(time.Duration(c.SleepMS) * time.Millisecond)
	}
	switch c.Mode {
	case "crash", "probe_crash":
		os.Exit(3)
	case "hang":
		sleepForever()
	case "hang_child":
		self, err := os.Executable()
		if err != nil {
			return err
		}
		child := exec.Command(self)
		child.Env = append(os.Environ(), "FIXTURE_HEARTBEAT="+c.Heartbeat)
		if err = child.Start(); err != nil {
			return err
		}
		sleepForever()
	case "big_output":
		p, err := r.Output("big.bin")
		if err != nil {
			return err
		}
		return os.WriteFile(p, make([]byte, 2<<20), 0o600)
	case "env":
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "PATH=") || strings.Contains(strings.ToUpper(kv), "TOKEN") || strings.HasPrefix(kv, "HOME=") {
				return errors.New("unexpected inherited environment: " + strings.SplitN(kv, "=", 2)[0])
			}
		}
	}
	return nil
}

func outPath(r *extension.Run) string {
	p, _ := r.Output("placeholder")
	return filepath.Join(filepath.Dir(p), "result.json")
}

// sleepForever blocks without tripping the Go runtime's deadlock detector.
func sleepForever() {
	for {
		time.Sleep(time.Hour)
	}
}

func heartbeat(p string) {
	for {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write([]byte{'.'})
			f.Close()
		}
		time.Sleep(20 * time.Millisecond)
	}
}
