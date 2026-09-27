package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(t.Context(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsageAndUnknownCommand(t *testing.T) {
	if code, out, _ := runCLI(t); code != exitUsage || !strings.Contains(out, "usage: lantai") {
		t.Fatalf("no args: %d %q", code, out)
	}
	if code, out, _ := runCLI(t, "help"); code != exitOK || !strings.Contains(out, "schema") {
		t.Fatalf("help: %d %q", code, out)
	}
	// 未实现的子命令不会被当作空命令接受。
	for _, c := range []string{"node", "mcp", "task"} {
		if code, _, _ := runCLI(t, c); code != exitUsage {
			t.Errorf("%s: exit %d, want usage error", c, code)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := runCLI(t, "version", "-json")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var v versionInfo
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Contracts) == 0 || len(v.Protocols) != 3 || v.GoVersion == "" {
		t.Fatalf("version info: %+v", v)
	}
	for _, p := range v.Protocols {
		if p.Status == "enabled" {
			t.Fatalf("%s must not be enabled in this build", p.Protocol)
		}
	}
	if code, out, _ := runCLI(t, "version"); code != exitOK || !strings.Contains(out, "lantai.agent-execution/v1") {
		t.Fatalf("text version: %d %q", code, out)
	}
}

func TestSchemaListAndValidate(t *testing.T) {
	code, out, _ := runCLI(t, "schema", "list")
	if code != exitOK || !strings.Contains(out, "lantai.error/v1") {
		t.Fatalf("list: %d %q", code, out)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.json")
	broken := filepath.Join(dir, "broken.json")
	os.WriteFile(good, []byte("error:\n  code: RATE_LIMITED\n  message: slow down\n  retryable: true\n  recovery_action: retry\n"), 0o644)
	os.WriteFile(bad, []byte(`{"error":{"code":"x","message":"m","retryable":false,"recovery_action":"none"}}`), 0o644)
	os.WriteFile(broken, []byte(`{"error":`), 0o644)

	if code, out, _ := runCLI(t, "schema", "validate", "lantai.error/v1", good); code != exitOK || !strings.HasPrefix(out, "ok") {
		t.Fatalf("good: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "schema", "validate", "-json", "lantai.error/v1", good, bad, broken)
	if code != exitInvalid {
		t.Fatalf("mixed: exit %d", code)
	}
	var res []validation
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || !res[0].Valid || res[1].Valid || res[2].Valid {
		t.Fatalf("results: %+v", res)
	}
	if res[1].Error.Error.Code != "SCHEMA_INVALID" || res[2].Error.Error.Details[0].Reason != "parse_error" {
		t.Fatalf("errors: %+v %+v", res[1].Error, res[2].Error)
	}
	for _, contract := range []string{"lantai.nope/v1", "lantai.common-defs/v1", "lantai.common-defs/v1#", "lantai.common-defs/v1#/$defs/nope", "lantai.error/v1#nope"} {
		if code, _, _ := runCLI(t, "schema", "validate", contract, good); code != exitUsage {
			t.Errorf("%s: exit %d, want usage error", contract, code)
		}
	}
	if code, _, _ := runCLI(t, "schema", "validate", "lantai.error/v1", filepath.Join(dir, "missing.json")); code != exitInternal {
		t.Fatalf("missing file: exit %d", code)
	}
	fence := filepath.Join(dir, "fence.json")
	os.WriteFile(fence, []byte(`{"attempt_id":"01J8Z3K4M5N6P7Q8R9S0T1V2W3","lease_fence":2,"recovery_epoch":1}`), 0o644)
	if code, out, _ := runCLI(t, "schema", "validate", "lantai.execution-common/v1#/$defs/task_fence", fence); code != exitOK {
		t.Fatalf("fragment: %d %q", code, out)
	}
}

func TestSchemaValidateExtremeNumbersReturnsStructuredError(t *testing.T) {
	for ext, document := range map[string]string{
		"json": `{"blobs":[1e-100000000,0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19]}`,
		"yaml": "blobs: [1e-100000000,0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19]\n",
	} {
		t.Run(ext, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid."+ext)
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			code, output, stderr := runCLI(t, "schema", "validate", "-json", "lantai.pin/v1", path)
			if code != exitInvalid || stderr != "" {
				t.Fatalf("exit=%d stderr=%q output=%q", code, stderr, output)
			}
			var results []validation
			if err := json.Unmarshal([]byte(output), &results); err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Valid || results[0].Error == nil || results[0].Error.Error.Code != "SCHEMA_INVALID" {
				t.Fatalf("result=%+v", results)
			}
		})
	}
}
