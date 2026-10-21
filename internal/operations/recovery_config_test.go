package operations

import (
	"fmt"
	"testing"
)

func TestRecoveryConfigPreservesPortableSettingsAndRebindsHost(t *testing.T) {
	source := []byte(`{"contract":"lantai.config/v1","instance":{"name":"source-library"},"secrets":{"dir":"/old/private"},"listen":{"api":"127.0.0.1:1111"},"http":{"allowed_origins":["https://old.example"],"max_json_bytes":2097152,"api_timeout_seconds":23},"storage":{"min_free_bytes":16777216,"allow_fuse":true,"max_file_bytes":65536},"transfer":{"interactive_slots":2,"batch_slots":3,"batch_per_principal":1,"batch_bytes_per_second":131072,"batch_bytes_per_second_while_interactive":65536},"lifecycle":{"scheduler":true,"interval_seconds":420,"batch":17}}`)
	target := []byte(`{"contract":"lantai.config/v1","instance":{"name":"new-empty"},"secrets":{"dir":"/new/private"},"listen":{"api":"127.0.0.1:2222"},"http":{"allowed_origins":["https://new.example"],"max_json_bytes":8388608},"storage":{"allow_fuse":false,"max_file_bytes":999999}}`)
	m, err := mergeRecoveryConfig(source, target)
	if err != nil {
		t.Fatal(err)
	}
	obj := func(k string) map[string]any { return m[k].(map[string]any) }
	if obj("secrets")["dir"] != "/new/private" || obj("listen")["api"] != "127.0.0.1:2222" || obj("instance")["name"] != "source-library" {
		t.Fatal("wrong configuration binding", m)
	}
	if fmt.Sprint(obj("storage")["max_file_bytes"]) != "65536" || obj("storage")["allow_fuse"] != false || fmt.Sprint(obj("http")["max_json_bytes"]) != "2097152" || obj("lifecycle")["scheduler"] != false || fmt.Sprint(obj("lifecycle")["batch"]) != "17" {
		t.Fatal("portable settings lost or automatic action revived", m)
	}
	m, err = mergeRecoveryConfig(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["listen"]; ok {
		t.Fatal("old listener revived")
	}
	if _, ok := m["http"].(map[string]any)["allowed_origins"]; ok {
		t.Fatal("old origin revived")
	}
}
